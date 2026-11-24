package sqlcheck

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// PolicyAccess names a relation (PostgreSQL regclass spelling) and a command
// requiring a permissive policy for the execution role.
type PolicyAccess struct{ Table, Command string }

func inferAccess(query string) ([]PolicyAccess, error) {
	t, ok := pgstmt.DML(query)
	if !ok {
		return nil, fmt.Errorf("expected a single data statement")
	}
	needsAccess := fmt.Errorf("declare Access for complex SQL")
	word := func(i int) string {
		if i >= 0 && i < len(t) && t[i].Kind == pgstmt.Word {
			return t[i].Text
		}
		return ""
	}
	pos := func(w string) int { return slices.Index(t, pgstmt.Token{Kind: pgstmt.Word, Text: w}) }
	symbols := func(i int, s string) bool { return i < len(t) && t[i] == (pgstmt.Token{Kind: pgstmt.Symbol, Text: s}) }
	id := func(i int) bool { return i < len(t) && (t[i].Kind == pgstmt.Word || t[i].Kind == pgstmt.Quoted) }
	op := word(0)
	from, lock := -1, -1
	for i, depth := 1, 0; i < len(t); i++ {
		switch {
		case symbols(i, "("):
			depth++
		case symbols(i, ")"):
			depth--
		}
		switch word(i) {
		case "select", "join", "using", "with", "table", "union", "intersect", "except":
			return nil, needsAccess
		case "from":
			if op == "select" && depth > 0 {
				continue // e.g. substring(note FROM 1 FOR 3)
			}
			if from >= 0 || op != "select" && op != "delete" {
				return nil, needsAccess
			}
			from = i
		case "for":
			if depth == 0 && (word(i+1) == "update" || word(i+1) == "share" ||
				word(i+1) == "no" && word(i+2) == "key" && word(i+3) == "update" ||
				word(i+1) == "key" && word(i+2) == "share") {
				lock = i
			}
		}
	}
	var n int
	switch op {
	case "table":
		n = 1
		op = "select"
	case "values":
		return nil, nil
	case "select":
		n = from + 1
		if n == 0 {
			return nil, nil
		}
	case "insert":
		if word(1) != "into" {
			return nil, needsAccess
		}
		n = 2
	case "update":
		n = 1
	case "delete":
		if word(1) != "from" {
			return nil, needsAccess
		}
		n = 2
	default:
		return nil, needsAccess
	}
	if word(n) == "only" {
		n++
	}
	if !id(n) || word(n) == "lateral" {
		return nil, needsAccess
	}
	table := quoteIdent(t[n].Text)
	n++
	if symbols(n, ".") {
		if !id(n + 1) {
			return nil, needsAccess
		}
		table += "." + quoteIdent(t[n+1].Text)
		n += 2
	}
	if symbols(n, ".") || op == "select" && symbols(n, "(") {
		return nil, needsAccess
	}
	if op == "select" {
		for j := n; j < len(t); j++ {
			if slices.Contains([]string{"where", "group", "order", "limit", "offset", "for", "fetch", "window", "having"}, word(j)) {
				break
			}
			if symbols(j, ",") || symbols(j, "(") {
				return nil, needsAccess
			}
		}
	}
	out := []PolicyAccess{{table, strings.ToUpper(op)}}
	add := func(cmd string) {
		a := PolicyAccess{table, cmd}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if pos("returning") >= 0 {
		add("SELECT")
	}
	if w := pos("where"); w >= 0 && (op == "delete" || op == "update") && readsColumn(t[w+1:]) {
		add("SELECT")
	}
	if op == "update" {
		start := pos("set") + 1
		for start < len(t) {
			// Each simple assignment starts with one target column. More
			// elaborate target forms need an application-supplied Access list.
			if !id(start) || !symbols(start+1, "=") {
				return nil, needsAccess
			}
			end := start + 2
			depth := 0
			for ; end < len(t); end++ {
				if depth == 0 && (symbols(end, ",") || word(end) == "where" || word(end) == "returning") {
					break
				}
				if symbols(end, "(") || symbols(end, "[") {
					depth++
				}
				if symbols(end, ")") || symbols(end, "]") {
					depth--
				}
			}
			if readsColumn(t[start+2 : end]) {
				add("SELECT")
			}
			if !symbols(end, ",") {
				break
			}
			start = end + 1
		}
	}
	if op == "insert" {
		if i := pos("conflict"); i >= 0 && (word(i+1) != "do" || word(i+2) == "update") {
			add("SELECT")
			if pos("update") >= 0 {
				add("UPDATE")
			}
		}
	}
	if op == "select" && lock >= n {
		add("UPDATE")
	}
	return out, nil
}

// readsColumn conservatively recognizes references in simple DML expressions.
// Function names and casts name catalog objects rather than row inputs.
func readsColumn(tokens []pgstmt.Token) bool {
	isID := func(i int) bool {
		return i < len(tokens) && (tokens[i].Kind == pgstmt.Word || tokens[i].Kind == pgstmt.Quoted)
	}
	sym := func(i int, value string) bool {
		return i >= 0 && i < len(tokens) && tokens[i] == (pgstmt.Token{Kind: pgstmt.Symbol, Text: value})
	}
	for i := 0; i < len(tokens); i++ {
		if !isID(i) {
			continue
		}
		start := i
		for sym(i+1, ".") && isID(i+2) {
			i += 2
		}
		if sym(i+1, "(") || sym(start-1, ":") && sym(start-2, ":") {
			continue
		}
		if i == start && tokens[i].Kind == pgstmt.Word {
			switch tokens[i].Text {
			case "null", "true", "false", "default", "current_date", "current_time", "current_timestamp", "current_user", "localtime", "localtimestamp",
				"and", "or", "not", "is", "in", "like", "ilike", "between", "case", "when", "then", "else", "end", "array", "returning":
				continue
			}
		}
		return true
	}
	return false
}

// CheckPolicyCoverage reads pg_policy for each required command. It checks
// permissive rules, ALL/PUBLIC and inherited memberships, and flags bypassing
// roles on RLS tables. A top-level row-locking clause on a simple SELECT also
// requires UPDATE coverage. Predicates are exercised separately by RunRLSMatrix.
func CheckPolicyCoverage(ctx context.Context, db *sql.DB, work RoleStatements) ([]string, error) {
	if err := work.validate(); err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var findings []string
	for role, stmts := range work {
		if err := auditRole(ctx, tx, role); err != nil {
			return nil, err
		}
		for _, s := range stmts {
			auto, err := inferAccess(s.SQL)
			if err != nil && len(s.Access) == 0 {
				findings = append(findings, fmt.Sprintf("%s/%s: %v", role, s.Name, err))
				continue
			}
			needs := slices.Clone(s.Access)
			for _, a := range auto {
				if !slices.Contains(needs, a) {
					needs = append(needs, a)
				}
			}
			for _, a := range needs {
				cmd, ok := map[string]string{"SELECT": "r", "INSERT": "a", "UPDATE": "w", "DELETE": "d"}[a.Command]
				if !ok || a.Table == "" {
					return nil, fmt.Errorf("invalid policy access %+v", a)
				}
				var rls, active, covered bool
				var kind string
				err := tx.QueryRowContext(ctx, `SELECT c.relkind,c.relrowsecurity,pg_catalog.row_security_active(c.oid),EXISTS(
     SELECT FROM pg_catalog.pg_policy p WHERE p.polrelid=c.oid AND p.polpermissive AND p.polcmd IN ('*',$2::"char")
      AND EXISTS(SELECT FROM unnest(p.polroles) r WHERE CASE WHEN r=0 THEN true ELSE pg_catalog.pg_has_role(current_user,r,'USAGE') END))
     FROM pg_catalog.pg_class c WHERE c.oid=pg_catalog.to_regclass($1)`, a.Table, cmd).Scan(&kind, &rls, &active, &covered)
				label := fmt.Sprintf("%s/%s: %s %s", role, s.Name, a.Table, a.Command)
				switch {
				case errors.Is(err, sql.ErrNoRows):
					findings = append(findings, label+": relation missing")
				case err != nil:
					return nil, err
				case kind == "v":
					findings = append(findings, label+": specify and check the underlying workload and its execution role")
				case rls && !active:
					findings = append(findings, label+": role bypasses RLS")
				case rls && !covered:
					findings = append(findings, label+": no applicable permissive policy")
				}
			}
		}
	}
	slices.Sort(findings)
	return slices.Compact(findings), nil
}
