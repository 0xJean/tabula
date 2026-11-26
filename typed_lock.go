package tabula

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// RowLockStrength selects the PostgreSQL locking clause on a physical relation.
type RowLockStrength uint8

const (
	ForUpdate RowLockStrength = iota + 1
	ForNoKeyUpdate
	ForShare
	ForKeyShare
)

// LockWait selects how an operation handles contention.
type LockWait uint8

const (
	// Wait blocks until PostgreSQL grants the lock or the operation ends.
	Wait LockWait = iota + 1
	// SkipLocked excludes conflicting rows from a locking read.
	SkipLocked
	// NoWait raises PostgreSQL lock_not_available on a conflicting row.
	NoWait
	// Try returns false immediately on an advisory lock conflict.
	Try
)

// RowLock identifies a locking strength, a specific occurrence and a wait mode.
type RowLock struct {
	strength RowLockStrength
	target   *occurrence
	wait     LockWait
}

// ForRows declares a lock on target. Wait, SkipLocked and NoWait are valid.
func ForRows[T any](strength RowLockStrength, target Rel[T], wait LockWait) RowLock {
	return RowLock{strength, target.source, wait}
}

// clause applies only to a physical row selection. It records UPDATE on any
// one column independently of the SELECT grants derived from expressions.
func (l RowLock) clause(c *typedCompiler, meta *StatementShape) (string, error) {
	if _, ok := c.sources[l.target]; !ok {
		return "", errors.New("tabula: row lock target is outside this query")
	}
	if err := l.target.checkCommand("UPDATE"); err != nil {
		return "", err
	}
	var strength, wait string
	switch l.strength {
	case ForUpdate:
		strength = "UPDATE"
	case ForNoKeyUpdate:
		strength = "NO KEY UPDATE"
	case ForShare:
		strength = "SHARE"
	case ForKeyShare:
		strength = "KEY SHARE"
	default:
		return "", errors.New("tabula: invalid row lock strength")
	}
	switch l.wait {
	case Wait:
	case SkipLocked:
		wait = " SKIP LOCKED"
	case NoWait:
		wait = " NOWAIT"
	default:
		return "", errors.New("tabula: a row lock requires Wait, SkipLocked or NoWait")
	}
	meta.Effects |= EffectRowLock
	meta.AnyColumnUpdate = append(meta.AnyColumnUpdate, l.target.table.sql())
	return " FOR " + strength + " OF " + typedQuote(l.target.alias) + wait, nil
}

// LockedRowsSpec selects rows of the lock's occurrence and projects them into
// a view. Omit Where to select all visible rows. A column-scoped table needs
// ScopeType and can execute only on a scoped session.
type LockedRowsSpec[S any] struct {
	Lock      RowLock
	Where     Pred
	ScopeType PGType[S]
	Select    []Projection
	// DeletedField declares an explicit catalog type and optional encoder for
	// the table's soft-delete column. Supply the FieldOf this occurrence.
	DeletedField SoftDeleteColumn
}

// LockedRows is one registered, single-table locking read.
type LockedRows[T, S any] struct {
	query effectRows[T, S]
	live  Binding
}

// DefineLockedRows registers a projected, physical row lock. The table must
// own the lock's occurrence. Its scope and soft-deletion rules apply.
func DefineLockedRows[T, K, S, R any](view *View[R, S], key string, table *Table[T, K, S], spec LockedRowsSpec[S]) *LockedRows[R, S] {
	p, err := TryDefineLockedRows(view, key, table, spec)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineLockedRows registers the complete lock or returns a declaration error.
func TryDefineLockedRows[T, K, S, R any](view *View[R, S], key string, table *Table[T, K, S], spec LockedRowsSpec[S]) (*LockedRows[R, S], error) {
	src := spec.Lock.target
	if view == nil || view.entry == nil || table == nil || src == nil || src.entry != table.entry || view.reg != table.reg || key == "" {
		return nil, errors.New("tabula: a locking query needs a registered view, key and occurrence of its table")
	}
	c, err := newTypedCompiler(true, src)
	if err != nil {
		return nil, err
	}
	columns, fields, err := c.project(view.entry, spec.Select)
	if err != nil {
		return nil, err
	}
	scope, err := c.scopePredicate(src, spec.ScopeType.id)
	if err != nil {
		return nil, err
	}
	var predicates []string
	if scope != "" {
		predicates = append(predicates, scope)
	}
	visible, live, err := lockVisibility(c, src, table, spec.DeletedField)
	if err != nil {
		return nil, err
	}
	if visible != "" {
		predicates = append(predicates, visible)
	}
	if spec.Where.part != nil {
		if !sameSQLType(spec.Where.kind.id, Bool().id) {
			return nil, errors.New("tabula: row lock predicate requires PostgreSQL boolean")
		}
		predicate, err := spec.Where.part.render(c)
		if err != nil {
			return nil, err
		}
		predicates = append(predicates, predicate)
	}
	meta, err := c.readRequirements()
	if err != nil {
		return nil, err
	}
	meta.Result = fields
	c.hasSecret = c.hasSecret || slices.ContainsFunc(view.cols, func(col column[R]) bool { return col.secret })
	clause, err := spec.Lock.clause(c, &meta)
	if err != nil {
		return nil, err
	}
	query := "SELECT " + strings.Join(columns, ", ") + " FROM " + src.table.sql() + " AS " + typedQuote(src.alias)
	if len(predicates) != 0 {
		query += " WHERE " + strings.Join(predicates, " AND ")
	}
	shape, err := c.finishShape(key+".lock", query+clause, spec.ScopeType.id, meta)
	if err != nil {
		return nil, err
	}
	if live.input != nil {
		// The descriptor owns this value. Keep it out of SQL observation logs.
		shape.shape.Inputs[shape.layout.positions[live.input]].Secret = true
	}
	if err := view.registerShapes(key, []*compiledShape{shape}); err != nil {
		return nil, err
	}
	return &LockedRows[R, S]{query: effectRows[R, S]{view, shape}, live: live}, nil
}

// All locks and decodes every selected row in a writable transaction.
func (p *LockedRows[T, S]) All(ctx context.Context, s Session[S], args ...Binding) ([]T, error) {
	if p == nil || p.query.shape == nil {
		return nil, ErrStatementAuthority
	}
	if p.live.input != nil {
		args = append(slices.Clip(args), p.live)
	}
	return p.query.all(ctx, s, CallEach, args)
}
