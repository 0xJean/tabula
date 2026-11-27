package tabula

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

type sqlClassification struct {
	effects     ShapeEffects
	stmtEffects statementEffects
	fixedMethod CallMethod
}

func classifySQLStatement(query string, kind sqlStmtKind, advisoryLock bool) (sqlClassification, error) {
	tokens, ok := pgstmt.SingleDML(query)
	if !ok {
		return sqlClassification{}, errors.New("SQL must be a single valid PostgreSQL data statement")
	}
	stmt := statementHead(tokens)
	var bodies [][]pgstmt.Token
	main := outerCommand(stmt, &bodies)
	if len(main) == 0 {
		return sqlClassification{}, errors.New("unsupported SQL statement or CTE form")
	}
	outerCmd := main[0].Text
	if readCommand(outerCmd) && hasClause(main, "into") {
		return sqlClassification{}, errors.New("SELECT INTO is not allowed")
	}

	writeCmdSet := make(map[string]bool)
	hasModifyingCTE := false
	pending := slices.Clone(bodies)
	for len(pending) > 0 {
		last := len(pending) - 1
		body := pending[last]
		pending = pending[:last]
		cmd := outerCommand(statementHead(body), &pending)
		if len(cmd) == 0 {
			return sqlClassification{}, errors.New("unsupported CTE body")
		}
		if readCommand(cmd[0].Text) {
			if hasClause(cmd, "into") {
				return sqlClassification{}, errors.New("SELECT INTO inside CTE is not allowed")
			}
			continue
		}
		if writeCommand(cmd[0].Text) {
			hasModifyingCTE = true
			writeCmdSet[strings.ToUpper(cmd[0].Text)] = true
			continue
		}
		return sqlClassification{}, fmt.Errorf("unsupported CTE command %q", cmd[0].Text)
	}
	outerWrite := writeCommand(outerCmd)
	if outerWrite {
		writeCmdSet[strings.ToUpper(outerCmd)] = true
	}
	hasReturning := outerWrite && hasClause(main, "returning")
	hasLock := hasRowLock(tokens)
	hasXactLock, hasSessionLock := scanAdvisoryLocks(tokens)
	if hasSessionLock {
		return sqlClassification{}, errors.New("session-level advisory lock functions are not allowed; use transaction-level pg_*advisory_xact_lock*")
	}

	writes := make([]statementWrite, 0, len(writeCmdSet))
	for _, cmd := range slices.Sorted(maps.Keys(writeCmdSet)) {
		writes = append(writes, statementWrite{command: cmd})
	}

	switch kind {
	case sqlKindRead:
		if outerWrite || hasModifyingCTE {
			return sqlClassification{}, errors.New("DefineSQL accepts read-only queries only; use DefineSQLExec or DefineSQLReturning for writes")
		}
		if advisoryLock && !hasXactLock {
			return sqlClassification{}, errors.New("AdvisoryLock requires a transaction-level pg_[try_]advisory_xact_lock[_shared] call")
		}
		eff := EffectRead
		if hasLock {
			eff |= EffectRowLock
		}
		if advisoryLock || hasXactLock {
			eff |= EffectAdvisoryLock
		}
		return sqlClassification{
			effects:     eff,
			stmtEffects: statementEffects{bits: eff},
		}, nil

	case sqlKindExec:
		if advisoryLock {
			if !isStandaloneXactAdvisoryLock(tokens) {
				return sqlClassification{}, errors.New("DefineSQLExec with AdvisoryLock requires a standalone SELECT calling pg_[try_]advisory_xact_lock[_shared](...)")
			}
			eff := EffectRead | EffectAdvisoryLock
			return sqlClassification{
				effects:     eff,
				stmtEffects: statementEffects{bits: eff},
				fixedMethod: CallEach,
			}, nil
		}
		if !outerWrite {
			if hasModifyingCTE {
				return sqlClassification{}, errors.New("modifying CTE returning rows must use DefineSQLReturning (or end with an outer INSERT/UPDATE/DELETE/MERGE without RETURNING)")
			}
			return sqlClassification{}, errors.New("DefineSQLExec requires a write statement (INSERT/UPDATE/DELETE/MERGE) or AdvisoryLock: true")
		}
		if hasReturning {
			return sqlClassification{}, errors.New("DefineSQLExec rejects statements with top-level RETURNING; use DefineSQLReturning")
		}
		return sqlClassification{
			effects: EffectWrite,
			stmtEffects: statementEffects{
				bits:     EffectWrite,
				writes:   writes,
				writeCTE: hasModifyingCTE,
			},
		}, nil

	case sqlKindReturning:
		if advisoryLock {
			return sqlClassification{}, errors.New("DefineSQLReturning does not accept AdvisoryLock; use DefineSQL or DefineSQLExec")
		}
		if outerWrite {
			if !hasReturning {
				return sqlClassification{}, errors.New("DefineSQLReturning requires a top-level RETURNING clause or a modifying CTE returning rows; use DefineSQLExec for count-only writes")
			}
		} else if !hasModifyingCTE {
			return sqlClassification{}, errors.New("DefineSQLReturning requires a write statement; use DefineSQL for read-only queries")
		}
		return sqlClassification{
			effects: EffectWrite,
			stmtEffects: statementEffects{
				bits:     EffectWrite,
				writes:   writes,
				writeCTE: hasModifyingCTE,
			},
		}, nil

	default:
		return sqlClassification{}, errors.New("unknown SQL declaration kind")
	}
}

func hasRowLock(tokens []pgstmt.Token) bool {
	for i := 0; i < len(tokens); i++ {
		if tokens[i] != (pgstmt.Token{Kind: pgstmt.Word, Text: "for"}) {
			continue
		}
		if i > 0 && (tokens[i-1] == (pgstmt.Token{Kind: pgstmt.Symbol, Text: "."}) ||
			tokens[i-1] == (pgstmt.Token{Kind: pgstmt.Word, Text: "as"})) {
			continue
		}
		if isWord(tokens[i+1:], "update") || isWord(tokens[i+1:], "share") ||
			(isWord(tokens[i+1:], "key") && isWord(tokens[i+2:], "share")) ||
			(isWord(tokens[i+1:], "no") && isWord(tokens[i+2:], "key") && isWord(tokens[i+3:], "update")) {
			return true
		}
	}
	return false
}

func isXactAdvisoryLockFunc(name string) bool {
	switch name {
	case "pg_advisory_xact_lock", "pg_advisory_xact_lock_shared",
		"pg_try_advisory_xact_lock", "pg_try_advisory_xact_lock_shared":
		return true
	default:
		return false
	}
}

func isSessionAdvisoryLockFunc(name string) bool {
	switch name {
	case "pg_advisory_lock", "pg_advisory_lock_shared",
		"pg_try_advisory_lock", "pg_try_advisory_lock_shared",
		"pg_advisory_unlock", "pg_advisory_unlock_shared", "pg_advisory_unlock_all":
		return true
	default:
		return false
	}
}

func scanAdvisoryLocks(tokens []pgstmt.Token) (hasXact, hasSession bool) {
	for i := 0; i < len(tokens)-1; i++ {
		if tokens[i].Kind != pgstmt.Word && tokens[i].Kind != pgstmt.Quoted {
			continue
		}
		if tokens[i+1] != (pgstmt.Token{Kind: pgstmt.Symbol, Text: "("}) {
			continue
		}
		name := strings.ToLower(tokens[i].Text)
		if isXactAdvisoryLockFunc(name) {
			hasXact = true
		}
		if isSessionAdvisoryLockFunc(name) {
			hasSession = true
		}
	}
	return hasXact, hasSession
}

func isStandaloneXactAdvisoryLock(tokens []pgstmt.Token) bool {
	if len(tokens) < 4 || !isWord(tokens, "select") {
		return false
	}
	rest := tokens[1:]
	if len(rest) >= 2 && (rest[0].Kind == pgstmt.Word || rest[0].Kind == pgstmt.Quoted) &&
		strings.EqualFold(rest[0].Text, "pg_catalog") && isSymbol(rest[1:], ".") {
		rest = rest[2:]
	}
	if len(rest) < 3 || (rest[0].Kind != pgstmt.Word && rest[0].Kind != pgstmt.Quoted) ||
		!isXactAdvisoryLockFunc(strings.ToLower(rest[0].Text)) {
		return false
	}
	body, tail := takeGroup(rest[1:])
	if len(body) == 0 || len(tail) != 0 {
		return false
	}
	for _, tok := range body {
		if tok.Kind == pgstmt.Word && (readCommand(tok.Text) || writeCommand(tok.Text) || tok.Text == "with") {
			return false
		}
	}
	return true
}
