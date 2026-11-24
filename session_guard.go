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
// namespace is protected.
type SessionGuardConfig struct {
	VariablePrefixes           []string
	ForbidRole                 bool
	ForbidSessionAuthorization bool
	ForbidSetConfig            bool
}

// NewSessionGuard compiles an application GuardQuery for trusted adapter SQL.
// RESET ALL and DISCARD ALL are rejected if they could reset protected state.
// Any active restriction rejects anonymous DO blocks and Unicode-escaped
// identifiers. A zero config permits all statements.
func NewSessionGuard(cfg SessionGuardConfig) (func(string) error, error) {
	prefixes := slices.Clone(cfg.VariablePrefixes)
	for i, p := range prefixes {
		if p == "" || strings.TrimSpace(p) != p {
			return nil, fmt.Errorf("invalid session variable prefix %q", p)
		}
		prefixes[i] = strings.ToLower(p)
	}
	cfg.VariablePrefixes = prefixes
	check := compileSessionGuard(cfg, false)
	return func(query string) error { return check(pgstmt.Lex(query)) }, nil
}

func compileSessionGuard(cfg SessionGuardConfig, readOnlyTx bool) func([]pgstmt.Token) error {
	guardsState := cfg.ForbidRole || cfg.ForbidSessionAuthorization || len(cfg.VariablePrefixes) > 0
	restricted := guardsState || cfg.ForbidSetConfig
	protected := func(name string) bool {
		name = strings.ToLower(name)
		if cfg.ForbidRole && name == "role" || cfg.ForbidSessionAuthorization && name == "session_authorization" {
			return true
		}
		for _, p := range cfg.VariablePrefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	return func(t []pgstmt.Token) error {
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
				if readOnlyTx && literalKey && strings.EqualFold(t[i+2].Text, "transaction_read_only") {
					return ErrReadOnly
				}
				if cfg.ForbidSetConfig || guardsState && (!literalKey || protected(t[i+2].Text)) {
					return fmt.Errorf("%w: set_config", ErrForbiddenStatement)
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
			if readOnlyTx && tok.Kind == pgstmt.Word {
				switch op {
				case "commit", "end", "abort":
					return ErrReadOnly
				case "prepare":
					if i+1 < len(t) && t[i+1] == (pgstmt.Token{Kind: pgstmt.Word, Text: "transaction"}) {
						return ErrReadOnly
					}
				case "rollback":
					n := i + 1
					if at(n) == "work" || at(n) == "transaction" {
						n++
					}
					if at(n) != "to" {
						return ErrReadOnly
					}
				}
			}
			if op == "discard" && at(i+1) == "all" && guardsState {
				return ErrForbiddenStatement
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
			if readOnlyTx && (name == "transaction_read_only" || op == "set" && name == "transaction" && requestsReadWrite(t[n+1:])) {
				return ErrReadOnly
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

// SET TRANSACTION can combine access mode with isolation and deferrability.
func requestsReadWrite(options []pgstmt.Token) bool {
	for i, tok := range options {
		if tok == (pgstmt.Token{Kind: pgstmt.Symbol, Text: ";"}) {
			break
		}
		if tok == (pgstmt.Token{Kind: pgstmt.Word, Text: "read"}) && i+1 < len(options) &&
			options[i+1] == (pgstmt.Token{Kind: pgstmt.Word, Text: "write"}) {
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
