package tabula

import (
	"fmt"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// SessionGuardConfig selects prohibited session operations. The constructor
// copies GUC name prefixes such as "scope.", "session." and "local." and matches
// them case-insensitively. Dynamic set_config keys are rejected whenever a
// namespace is protected. ForbidVariableReads additionally rejects reading the
// protected namespaces: current_setting with a protected or non-literal key,
// SHOW of a protected name or SHOW ALL, and reads of the pg_settings view.
// A result column may be named pg_settings. ForbidVariableReads requires at
// least one VariablePrefixes entry.
type SessionGuardConfig struct {
	VariablePrefixes           []string
	ForbidRole                 bool
	ForbidSessionAuthorization bool
	ForbidSetConfig            bool
	ForbidVariableReads        bool
}

// NewSessionGuard compiles an application GuardQuery for trusted adapter SQL.
// RESET ALL, DISCARD ALL, and transaction-ending commands (COMMIT, END, ABORT,
// non-savepoint ROLLBACK, PREPARE TRANSACTION) are rejected whenever the guard
// protects role, session-authorization, or variable state. Any active
// restriction rejects anonymous DO blocks, Unicode-escaped identifiers and
// UPDATE of pg_settings, which PostgreSQL applies as SET. A zero config
// permits all statements.
func NewSessionGuard(cfg SessionGuardConfig) (func(string) error, error) {
	prefixes := slices.Clone(cfg.VariablePrefixes)
	for i, p := range prefixes {
		if p == "" || strings.TrimSpace(p) != p {
			return nil, fmt.Errorf("invalid session variable prefix %q", p)
		}
		prefixes[i] = strings.ToLower(p)
	}
	if cfg.ForbidVariableReads && len(prefixes) == 0 {
		return nil, fmt.Errorf("ForbidVariableReads requires VariablePrefixes")
	}
	cfg.VariablePrefixes = prefixes
	check := compileSessionGuard(cfg, false)
	return func(query string) error { return check(pgstmt.Lex(query)) }, nil
}

func compileSessionGuard(cfg SessionGuardConfig, readOnlyTx bool) func([]pgstmt.Token) error {
	guardsState := cfg.ForbidRole || cfg.ForbidSessionAuthorization || len(cfg.VariablePrefixes) > 0 || readOnlyTx
	restricted := guardsState || cfg.ForbidSetConfig
	protected := func(name string) bool {
		name = strings.ToLower(name)
		if readOnlyTx && (name == "transaction_read_only" || name == "default_transaction_read_only") {
			return true
		}
		if cfg.ForbidRole && name == "role" || cfg.ForbidSessionAuthorization && name == "session_authorization" {
			return true
		}
		return protectedVariable(cfg.VariablePrefixes, name)
	}
	return func(t []pgstmt.Token) error {
		if cfg.ForbidVariableReads && readsSettingsView(t) {
			return fmt.Errorf("%w: pg_settings", ErrForbiddenStatement)
		}
		at := func(i int) string {
			if i < len(t) && t[i].Kind != pgstmt.Literal {
				return strings.ToLower(t[i].Text)
			}
			return ""
		}
		start := true
		for i, tok := range t {
			// The server decodes Unicode-escaped identifiers with an optional
			// UESCAPE clause. Require ordinary identifiers on guarded SQL.
			if restricted && tok == (pgstmt.Token{Kind: pgstmt.Symbol, Text: "u&"}) {
				return fmt.Errorf("%w: escaped identifier", ErrForbiddenStatement)
			}
			if tok.Kind == pgstmt.Symbol && tok.Text == ";" {
				start = true
				continue
			}
			if at(i) == "set_config" && at(i+1) == "(" {
				literalKey := i+3 < len(t) && t[i+2].Kind == pgstmt.Literal && at(i+3) == "," && !strings.Contains(t[i+2].Text, "\\")
				if cfg.ForbidSetConfig || guardsState && (!literalKey || protected(t[i+2].Text)) {
					return fmt.Errorf("%w: set_config", ErrForbiddenStatement)
				}
			}
			// UPDATE pg_settings is applied by PostgreSQL as set_config.
			if restricted && at(i) == "update" && settingsRelation(t, i+1) {
				return fmt.Errorf("%w: pg_settings update", ErrForbiddenStatement)
			}
			if cfg.ForbidVariableReads {
				if at(i) == "current_setting" && at(i+1) == "(" {
					literalKey := i+3 < len(t) && t[i+2].Kind == pgstmt.Literal && (at(i+3) == "," || at(i+3) == ")") && !strings.Contains(t[i+2].Text, "\\")
					if !literalKey || protectedVariable(cfg.VariablePrefixes, t[i+2].Text) {
						return fmt.Errorf("%w: current_setting", ErrForbiddenStatement)
					}
				}
			}
			if !start {
				continue
			}
			start = false
			if restricted && tok == (pgstmt.Token{Kind: pgstmt.Word, Text: "do"}) {
				return fmt.Errorf("%w: DO", ErrForbiddenStatement)
			}
			op := at(i)
			if guardsState && tok.Kind == pgstmt.Word {
				switch op {
				case "commit", "end", "abort":
					return fmt.Errorf("%w: transaction control (%s)", ErrForbiddenStatement, op)
				case "prepare":
					if i+1 < len(t) && t[i+1] == (pgstmt.Token{Kind: pgstmt.Word, Text: "transaction"}) {
						return fmt.Errorf("%w: transaction control (prepare transaction)", ErrForbiddenStatement)
					}
				case "rollback":
					n := i + 1
					if at(n) == "work" || at(n) == "transaction" {
						n++
					}
					if at(n) != "to" {
						return fmt.Errorf("%w: transaction control (rollback)", ErrForbiddenStatement)
					}
				}
			}
			if op == "discard" && at(i+1) == "all" && guardsState {
				return fmt.Errorf("%w: discard all", ErrForbiddenStatement)
			}
			if cfg.ForbidVariableReads && op == "show" {
				name := at(i + 1)
				for j := i + 2; at(j) == "."; j += 2 {
					name += "." + at(j+1)
				}
				if name == "all" || protectedVariable(cfg.VariablePrefixes, name) {
					return fmt.Errorf("%w: show %s", ErrForbiddenStatement, name)
				}
			}
			if op != "set" && op != "reset" {
				continue
			}
			n := i + 1
			if op == "set" && hasSetModifier(t[n:]) {
				n++
			}
			name := at(n)
			for j := n + 1; at(j) == "."; j += 2 {
				name += "." + at(j+1)
			}
			if readOnlyTx && op == "set" {
				if name == "transaction" ||
					name == "characteristics" && at(n+1) == "as" && at(n+2) == "transaction" && setsAccessMode(t[n+3:]) {
					return fmt.Errorf("%w: set transaction", ErrForbiddenStatement)
				}
			}
			if (op == "reset" && name == "all" && guardsState) ||
				(cfg.ForbidSessionAuthorization && name == "session" && at(n+1) == "authorization") ||
				protected(name) {
				return fmt.Errorf("%w: %s %s", ErrForbiddenStatement, op, name)
			}
		}
		return nil
	}
}

// readsSettingsView permits output labels but conservatively rejects other
// uses of the catalog name. Scan each query level separately: a nested FROM
// belongs to the inner query and a comma in a FROM list stays in that list.
func readsSettingsView(t []pgstmt.Token) bool {
	bare := pgstmt.Token{Kind: pgstmt.Word, Text: "pg_settings"}
	quoted := pgstmt.Token{Kind: pgstmt.Quoted, Text: "pg_settings"}
	if !slices.Contains(t, bare) && !slices.Contains(t, quoted) {
		return false
	}

	var targets bool
	var outer []bool
	var prev pgstmt.Token
	for i, tok := range t {
		switch tok {
		case bare, quoted:
			if !targets && prev != (pgstmt.Token{Kind: pgstmt.Word, Text: "as"}) {
				return true
			}
		case pgstmt.Token{Kind: pgstmt.Symbol, Text: "("}:
			outer = append(outer, targets)
			targets = false
		case pgstmt.Token{Kind: pgstmt.Symbol, Text: ")"}:
			if n := len(outer); n > 0 {
				targets = outer[n-1]
				outer = outer[:n-1]
			}
		case pgstmt.Token{Kind: pgstmt.Symbol, Text: ";"}:
			targets = false
			outer = outer[:0]
		case pgstmt.Token{Kind: pgstmt.Word, Text: "select"}:
			targets = startsSelectList(t[:i])
		case pgstmt.Token{Kind: pgstmt.Word, Text: "by"}:
			// ORDER and GROUP are reserved keywords except in identifier
			// positions such as a qualified name or an explicit label.
			if prev.Kind == pgstmt.Word && (prev.Text == "order" || prev.Text == "group") {
				targets = i < 2 || t[i-2] != (pgstmt.Token{Kind: pgstmt.Symbol, Text: "."}) &&
					t[i-2] != (pgstmt.Token{Kind: pgstmt.Word, Text: "as"})
			}
		default:
			if tok.Kind == pgstmt.Word {
				switch tok.Text {
				case "from", "into", "table", "join", "using", "only", "where", "group", "having",
					"window", "order", "limit", "offset", "fetch", "for", "union", "intersect", "except":
					targets = false
				}
			}
		}
		prev = tok
	}
	return false
}

func startsSelectList(before []pgstmt.Token) bool {
	if len(before) == 0 {
		return true
	}
	prev := before[len(before)-1]
	if prev.Kind == pgstmt.Symbol {
		return prev.Text == "(" || prev.Text == ")" || prev.Text == ";"
	}
	if len(before) >= 2 && prev.Kind == pgstmt.Word && (prev.Text == "all" || prev.Text == "distinct") {
		prev = before[len(before)-2]
	}
	return prev.Kind == pgstmt.Word && (prev.Text == "union" || prev.Text == "intersect" || prev.Text == "except")
}

// SET SESSION CHARACTERISTICS can combine access mode with isolation clauses.
func setsAccessMode(options []pgstmt.Token) bool {
	for i, tok := range options {
		if tok == (pgstmt.Token{Kind: pgstmt.Symbol, Text: ";"}) {
			break
		}
		if tok == (pgstmt.Token{Kind: pgstmt.Word, Text: "read"}) && i+1 < len(options) &&
			options[i+1].Kind == pgstmt.Word && (options[i+1].Text == "only" || options[i+1].Text == "write") {
			return true
		}
	}
	return false
}

// A SET modifier precedes a separate name. A dot after LOCAL or SESSION
// continues the parameter name, even with intervening whitespace or comments.
func hasSetModifier(args []pgstmt.Token) bool {
	if len(args) < 2 || args[1].Kind != pgstmt.Word && args[1].Kind != pgstmt.Quoted {
		return false
	}
	switch args[0] {
	case pgstmt.Token{Kind: pgstmt.Word, Text: "local"}:
		return true
	case pgstmt.Token{Kind: pgstmt.Word, Text: "session"}:
		// Leave the two keywords of SET SESSION AUTHORIZATION together.
		return args[1] != (pgstmt.Token{Kind: pgstmt.Word, Text: "authorization"})
	}
	return false
}

func protectedVariable(prefixes []string, name string) bool {
	name = strings.ToLower(name)
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// settingsRelation reports whether t[j:] names the pg_settings view, optionally
// after ONLY and qualified as pg_catalog. Unquoted words are already folded;
// a quoted name must match exactly, as PostgreSQL resolves it.
func settingsRelation(t []pgstmt.Token, j int) bool {
	name := func(k int) string {
		if k < len(t) && (t[k].Kind == pgstmt.Word || t[k].Kind == pgstmt.Quoted) {
			return t[k].Text
		}
		return ""
	}
	if j < len(t) && t[j] == (pgstmt.Token{Kind: pgstmt.Word, Text: "only"}) {
		j++
	}
	if name(j) == "pg_catalog" && j+1 < len(t) && t[j+1] == (pgstmt.Token{Kind: pgstmt.Symbol, Text: "."}) {
		j += 2
	}
	return name(j) == "pg_settings"
}
