package tabula

import (
	"fmt"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// SessionGuardConfig selects prohibited session operations. Prefixes name GUC
// namespaces (for example "scope."). The constructor copies and folds them.
// Dynamic set_config keys are rejected whenever a namespace is protected.
type SessionGuardConfig struct {
	VariablePrefixes           []string
	ForbidRole                 bool
	ForbidSessionAuthorization bool
	ForbidSetConfig            bool
}

// NewSessionGuard compiles an application GuardQuery for trusted adapter SQL.
// RESET ALL and DISCARD ALL are rejected if they could reset protected state.
// Enabling any restriction also rejects Unicode-escaped identifiers.
func NewSessionGuard(cfg SessionGuardConfig) (func(string) error, error) {
	prefixes := slices.Clone(cfg.VariablePrefixes)
	for i, p := range prefixes {
		if p == "" || strings.TrimSpace(p) != p {
			return nil, fmt.Errorf("invalid session variable prefix %q", p)
		}
		prefixes[i] = strings.ToLower(p)
	}
	cfg.VariablePrefixes = prefixes
	check := compileSessionGuard(cfg)
	return func(query string) error { return check(pgstmt.Lex(query)) }, nil
}

func compileSessionGuard(cfg SessionGuardConfig) func([]pgstmt.Token) error {
	guardsState := cfg.ForbidRole || cfg.ForbidSessionAuthorization || len(cfg.VariablePrefixes) > 0
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
			if (guardsState || cfg.ForbidSetConfig) && tok == (pgstmt.Token{Kind: pgstmt.Symbol, Text: "u&"}) {
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
			if !start {
				continue
			}
			start = false
			op := at(i)
			if op == "discard" && at(i+1) == "all" && guardsState {
				return ErrForbiddenStatement
			}
			if op != "set" && op != "reset" {
				continue
			}
			n := i + 1
			if op == "set" && (at(n) == "local" || (at(n) == "session" && at(n+1) != "authorization")) {
				n++
			}
			name := at(n)
			for j := n + 1; at(j) == "."; j += 2 {
				name += "." + at(j+1)
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
