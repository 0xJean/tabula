package tabula

import (
	"fmt"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// StatementPolicy lists the PostgreSQL commands accepted by session methods.
// Read applies to Scan and Each (select, table, values). Write applies to Exec
// and Returning (insert, update, delete, merge). Include with in the
// corresponding list to permit a WITH prefix on an otherwise allowed command.
// Command names are case-insensitive. With an active policy, an empty list
// permits no methods on that side. With no flags or command lists, both sides are unrestricted.
type StatementPolicy struct {
	Read, Write []string
	// RequireReturning requires an outer RETURNING clause for the Returning method.
	RequireReturning bool
	// AllowAuthorizedWriteCTE admits a signed modifying CTE with an outer read
	// result. The write list must include with and every native write command.
	// Direct SQL and unsigned calls retain the ordinary command policy.
	AllowAuthorizedWriteCTE bool
	// AllowAuthorizedWriteRoutine admits exact signed write routine calls under
	// Returning even though their SQL starts with SELECT. It grants no database
	// privilege and does not change read-only or session-escape checks.
	AllowAuthorizedWriteRoutine bool
}

// StatementGuard is the compiled, immutable policy for driver sessions and
// offline inventories. Its zero value leaves SQL unrestricted.
type StatementGuard struct {
	single                           bool
	read, write                      map[string]bool
	requireReturning                 bool
	allowWriteCTE, allowWriteRoutine bool
}

// Enabled reports whether the guard enforces statement boundaries.
func (g StatementGuard) Enabled() bool { return g.single }

// Compile validates the allowlists and takes a copy of their contents. A
// configured policy implies singleStatement. With only singleStatement enabled,
// the guard checks statement boundaries without restricting the method.
func (p StatementPolicy) Compile(singleStatement bool) (StatementGuard, error) {
	if len(p.Read) == 0 && len(p.Write) == 0 && !p.RequireReturning && !p.AllowAuthorizedWriteCTE && !p.AllowAuthorizedWriteRoutine {
		return StatementGuard{single: singleStatement}, nil
	}
	verbs := func(names []string, allowed func(string) bool) (map[string]bool, error) {
		set := make(map[string]bool, len(names))
		for _, name := range names {
			v := strings.ToLower(name)
			if v != "with" && !allowed(v) {
				return nil, fmt.Errorf("tabula: unsupported statement policy command %q", name)
			}
			set[v] = true
		}
		if set["with"] && len(set) == 1 {
			return nil, fmt.Errorf("tabula: with needs an allowed outer command")
		}
		return set, nil
	}
	read, err := verbs(p.Read, readCommand)
	if err != nil {
		return StatementGuard{}, err
	}
	write, err := verbs(p.Write, writeCommand)
	if err != nil {
		return StatementGuard{}, err
	}
	if p.RequireReturning && len(write) == 0 {
		return StatementGuard{}, fmt.Errorf("tabula: RequireReturning needs an allowed write command")
	}
	return StatementGuard{single: true, read: read, write: write, requireReturning: p.RequireReturning, allowWriteCTE: p.AllowAuthorizedWriteCTE, allowWriteRoutine: p.AllowAuthorizedWriteRoutine}, nil
}

type statementDenial string

func (e statementDenial) Error() string { return string(e) }
func (statementDenial) Unwrap() error   { return ErrForbiddenStatement }

const singleStatementDenied statementDenial = "expected one PostgreSQL data statement"

// GuardSingleStatement admits one SELECT, VALUES, TABLE, INSERT, UPDATE,
// DELETE, MERGE or WITH in UTF-8 SQL, optionally terminated by a semicolon.
// It rejects incomplete lexical boundaries and ordinary string quote escapes
// whose interpretation depends on connection settings. Use bind parameters or
// explicit E strings for those values. PostgreSQL validates the grammar and
// authorizes data and function access.
func GuardSingleStatement(query string) error {
	if _, ok := pgstmt.SingleDML(query); !ok {
		return singleStatementDenied
	}
	return nil
}

// Check applies the guard to a Scan, Each, Exec or Returning call. A rejection
// matches ErrForbiddenStatement. The read methods reject SELECT INTO and
// data-modifying CTEs. Access inside functions still depends on database grants.
func (g StatementGuard) Check(method, query string) error {
	if !g.single {
		return nil
	}
	tokens, ok := pgstmt.SingleDML(query)
	if !ok {
		return singleStatementDenied
	}
	if g.read == nil {
		return nil
	}
	allowed := g.read
	write := false
	switch method {
	case "Exec", "Returning":
		allowed, write = g.write, true
	case "Scan", "Each":
	default:
		return statementDenial("unknown SQL session method")
	}

	stmt := statementHead(tokens)
	if isWord(stmt, "with") && !allowed["with"] {
		return statementDenial("WITH is not enabled for this session method")
	}
	var bodies [][]pgstmt.Token
	main := outerCommand(stmt, &bodies)
	if len(main) == 0 || !allowed[main[0].Text] || !write && hasClause(main, "into") {
		return statementDenial("SQL command is not allowed for this session method")
	}
	if method == "Returning" && g.requireReturning && !hasClause(main, "returning") {
		return statementDenial("Returning requires an outer RETURNING clause")
	}
	for len(bodies) > 0 {
		last := len(bodies) - 1
		body := bodies[last]
		bodies = bodies[:last]
		cmd := outerCommand(statementHead(body), &bodies)
		if len(cmd) == 0 {
			return statementDenial("unsupported common table expression")
		}
		if readCommand(cmd[0].Text) && !hasClause(cmd, "into") {
			continue
		}
		if !write || !writeCommand(cmd[0].Text) || !allowed[cmd[0].Text] {
			return statementDenial("common table expression uses a disallowed command")
		}
	}
	return nil
}

func readCommand(s string) bool { return s == "select" || s == "values" || s == "table" }
func writeCommand(s string) bool {
	return s == "insert" || s == "update" || s == "delete" || s == "merge"
}

func isWord(t []pgstmt.Token, s string) bool {
	return len(t) > 0 && t[0] == (pgstmt.Token{Kind: pgstmt.Word, Text: s})
}

func isSymbol(t []pgstmt.Token, s string) bool {
	return len(t) > 0 && t[0] == (pgstmt.Token{Kind: pgstmt.Symbol, Text: s})
}

func statementHead(t []pgstmt.Token) []pgstmt.Token {
	for isSymbol(t, "(") {
		t = t[1:]
	}
	return t
}

// outerCommand collects each CTE body and finds the command that owns the
// result. Unsupported WITH forms fail closed under a method policy.
func outerCommand(t []pgstmt.Token, bodies *[][]pgstmt.Token) []pgstmt.Token {
	if isWord(t, "with") {
		t = t[1:]
		if isWord(t, "recursive") {
			t = t[1:]
		}
		for {
			if len(t) == 0 || t[0].Kind != pgstmt.Word && t[0].Kind != pgstmt.Quoted {
				return nil
			}
			t = t[1:]
			if isSymbol(t, "(") { // Optional column names.
				_, t = takeGroup(t)
			}
			if !isWord(t, "as") {
				return nil
			}
			t = t[1:]
			if isWord(t, "not") {
				t = t[1:]
				if !isWord(t, "materialized") {
					return nil
				}
			}
			if isWord(t, "materialized") {
				t = t[1:]
			}
			body, rest := takeGroup(t)
			if len(body) == 0 {
				return nil
			}
			*bodies = append(*bodies, body)
			t = rest
			if !isSymbol(t, ",") {
				break
			}
			t = t[1:]
		}
		t = statementHead(t)
	}
	if len(t) == 0 || t[0].Kind != pgstmt.Word || !readCommand(t[0].Text) && !writeCommand(t[0].Text) {
		return nil
	}
	return t
}

func takeGroup(t []pgstmt.Token) (body, rest []pgstmt.Token) {
	if !isSymbol(t, "(") {
		return nil, nil
	}
	depth := 1
	for i := 1; i < len(t); i++ {
		switch t[i] {
		case pgstmt.Token{Kind: pgstmt.Symbol, Text: "("}:
			depth++
		case pgstmt.Token{Kind: pgstmt.Symbol, Text: ")"}:
			depth--
			if depth == 0 {
				return t[1:i], t[i+1:]
			}
		}
	}
	return nil, nil
}

func hasClause(t []pgstmt.Token, keyword string) bool {
	depth := 0
	for i := 1; i < len(t); i++ {
		switch t[i] {
		case pgstmt.Token{Kind: pgstmt.Symbol, Text: "("}:
			depth++
		case pgstmt.Token{Kind: pgstmt.Symbol, Text: ")"}:
			depth--
		case pgstmt.Token{Kind: pgstmt.Word, Text: keyword}:
			if depth == 0 && !isSymbol(t[i-1:], ".") && !isWord(t[i-1:], "as") {
				return true
			}
		}
	}
	return false
}
