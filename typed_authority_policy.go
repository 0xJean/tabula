package tabula

import (
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// CheckAuthorized verifies the registry, receiving session and method before
// applying the policy to a registered call. Opt-in exceptions inspect the
// immutable compiled effects, never an application-editable AuthorizedSQL.
// Drivers also Open the call and enforce transaction, context and session guards.
func (g StatementGuard) CheckAuthorized(authority SQLAuthority, owner any, method CallMethod, call AuthorizedStatement) error {
	if err := authority.checkCall(owner, method, call); err != nil {
		return err
	}
	shape := call.shape
	policy := callPolicyMethod(method)
	if shape.shape.Effects&EffectWrite == 0 || method != CallExec && method != CallReturning && method != CallReturningEach {
		return g.Check(policy, shape.sql)
	}
	// A signed graph can reveal additional native commands, notably the UPDATE
	// half of an upsert, that are not separate leading tokens in its SQL.
	if g.write != nil {
		for _, write := range shape.effects.writes {
			if !g.write[strings.ToLower(write.command)] {
				return statementDenial("registered write uses a disallowed command")
			}
		}
	}
	err := g.Check(policy, shape.sql)
	if err == nil {
		return nil
	}
	effects := shape.effects
	if effects.writeCTE {
		return g.checkWriteCTE(shape)
	}
	if effects.writeRoutine && g.allowWriteRoutine && signedWriteRoutines(shape) {
		tokens, ok := pgstmt.SingleDML(shape.sql)
		if !ok {
			return singleStatementDenied
		}
		var bodies [][]pgstmt.Token
		main := outerCommand(statementHead(tokens), &bodies)
		if !isWord(main, "select") || len(bodies) != 0 || hasClause(main, "into") {
			return statementDenial("expected a registered write routine call")
		}
		return nil
	}
	return err
}

func callPolicyMethod(method CallMethod) string {
	switch method {
	case CallExec:
		return "Exec"
	case CallScan:
		return "Scan"
	case CallEach:
		return "Each"
	case CallReturning, CallReturningEach:
		return "Returning"
	default:
		return ""
	}
}

func (g StatementGuard) checkWriteCTE(shape *compiledShape) error {
	if !g.allowWriteCTE || !g.write["with"] || len(shape.effects.writes) == 0 {
		return statementDenial("registered write CTEs are not enabled for this session")
	}
	if shape.effects.writeRoutine && (!g.allowWriteRoutine || !signedWriteRoutines(shape)) {
		return statementDenial("registered write routines are not enabled for this session")
	}
	tokens, ok := pgstmt.SingleDML(shape.sql)
	if !ok {
		return singleStatementDenied
	}
	if !isWord(statementHead(tokens), "with") {
		return statementDenial("expected a registered WITH statement")
	}
	pending := [][]pgstmt.Token{statementHead(tokens)}
	for len(pending) > 0 {
		i := len(pending) - 1
		body := pending[i]
		pending = pending[:i]
		cmd := outerCommand(statementHead(body), &pending)
		if len(cmd) == 0 {
			return statementDenial("unsupported common table expression")
		}
		if readCommand(cmd[0].Text) && !hasClause(cmd, "into") {
			continue
		}
		if !writeCommand(cmd[0].Text) || !g.write[cmd[0].Text] {
			return statementDenial("registered write CTE uses a disallowed command")
		}
	}
	return nil
}

func signedWriteRoutines(shape *compiledShape) bool {
	found := false
	for _, routine := range shape.shape.Routines {
		if routine.ReadOnly {
			continue
		}
		found = true
		granted := false
		for _, grant := range shape.shape.Privileges {
			if grant.Function == routine.Signature && grant.Privilege == "EXECUTE" {
				granted = true
				break
			}
		}
		if !granted {
			return false
		}
	}
	return found
}
