package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
	"strings"
)

// Locks replaces the query's locks with this ordered list of distinct physical
// occurrence targets. Each target keeps its own strength and wait mode. A
// duplicate, hidden or nullable target fails before registration; no implicit
// strongest-mode merge occurs. Lock replaces this entire list with one lock.
// Aggregate, distinct, window and set results cannot be locked. Exact counts
// and paired List calls are unavailable on locking reads.
func (q ReadSelect) Locks(first RowLock, rest ...RowLock) ReadSelect {
	q.lock = RowLock{}
	q.locks = slices.Concat([]RowLock{first}, rest)
	return q
}

func (q ReadSelect) hasRowLocks() bool { return q.lock.declared || len(q.locks) != 0 }

func (q ReadSelect) rowLocks() []RowLock {
	if q.lock.declared {
		return []RowLock{q.lock}
	}
	return q.locks
}

func (q ReadSelect) checkRowLocks(c *typedCompiler, aggregate bool, outer map[*occurrence]bool) error {
	if !q.hasRowLocks() {
		return nil
	}
	if aggregate || q.distinct || len(q.distinctOn) != 0 || q.hasWindows() {
		return fmt.Errorf("tabula: row locks require ungrouped physical rows without distinct or window results")
	}
	local := make(map[*occurrence]bool)
	add := func(source ReadSource) {
		if !isNilBound(source) {
			if physical, err := source.readSource(); err == nil {
				local[physical.rel] = true
			}
		}
	}
	add(q.from)
	for _, join := range q.joins {
		add(join.source)
	}
	seen := make(map[*occurrence]bool)
	for _, lock := range q.rowLocks() {
		if !lock.declared || lock.target == nil || !local[lock.target] {
			return fmt.Errorf("tabula: row lock needs a visible physical occurrence in this query block")
		}
		if seen[lock.target] {
			return fmt.Errorf("tabula: duplicate row lock target %q", lock.target.alias)
		}
		seen[lock.target] = true
		if outer[lock.target] {
			return fmt.Errorf("tabula: row locks require a preserved occurrence")
		}
		if _, err := lock.clause(c, &tabula.StatementShape{}); err != nil {
			return err
		}
	}
	return nil
}

func (q ReadSelect) rowLockClauses(c *typedCompiler, meta *tabula.StatementShape) (string, error) {
	var sql strings.Builder
	for _, lock := range q.rowLocks() {
		clause, err := lock.clause(c, meta)
		if err != nil {
			return "", err
		}
		sql.WriteString(clause)
	}
	return sql.String(), nil
}
