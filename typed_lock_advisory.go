package tabula

import (
	"context"
	"errors"
	"strings"
)

// AdvisoryMode selects an exclusive or shared transaction-level advisory lock.
type AdvisoryMode uint8

const (
	Exclusive AdvisoryMode = iota + 1
	Shared
)

// AdvisoryLock declares a transaction-level PostgreSQL advisory operation.
// The bigint and two-integer key spaces are independent in PostgreSQL.
type AdvisoryLock struct {
	mode AdvisoryMode
	wait LockWait
	keys []advisoryKey
}

type advisoryKey struct {
	part     expressionPart
	kind     catalogType
	nullable bool
}

// AdvisoryXactLock uses a single bigint key. Wait blocks, Try returns false
// on contention. Both modes hold an acquired lock until transaction end.
func AdvisoryXactLock(mode AdvisoryMode, wait LockWait, key Expr[int64]) AdvisoryLock {
	return AdvisoryLock{mode, wait, []advisoryKey{{key.part, key.kind.id, key.nullable}}}
}

// AdvisoryXactLockPair uses the separate PostgreSQL (integer, integer) overload.
func AdvisoryXactLockPair(mode AdvisoryMode, wait LockWait, first, second Expr[int32]) AdvisoryLock {
	return AdvisoryLock{mode, wait, []advisoryKey{
		{first.part, first.kind.id, first.nullable}, {second.part, second.kind.id, second.nullable},
	}}
}

// AdvisoryLockPlan acquires a registered advisory lock through an authorized
// transaction session. Its View owns the statement in the registry.
type AdvisoryLockPlan[T, S any] struct{ query effectRows[T, S] }

// DefineAdvisoryLock registers a transaction lock independently of any table
// access. The output view has one boolean column indicating acquisition.
func DefineAdvisoryLock[T, S any](view *View[T, S], key string, lock AdvisoryLock) *AdvisoryLockPlan[T, S] {
	p, err := TryDefineAdvisoryLock(view, key, lock)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineAdvisoryLock registers the complete operation or returns an error.
func TryDefineAdvisoryLock[T, S any](view *View[T, S], key string, lock AdvisoryLock) (*AdvisoryLockPlan[T, S], error) {
	if view == nil || view.entry == nil || len(view.cols) != 1 || view.cols[0].goType != Bool().id.goType || !typedIdent(view.cols[0].name) || key == "" {
		return nil, errors.New("tabula: an advisory lock needs a registered one-boolean view and key")
	}
	function := "pg_advisory_xact_lock"
	switch lock.wait {
	case Wait:
	case Try:
		function = "pg_try_advisory_xact_lock"
	default:
		return nil, errors.New("tabula: an advisory lock requires Wait or Try")
	}
	switch lock.mode {
	case Exclusive:
	case Shared:
		function += "_shared"
	default:
		return nil, errors.New("tabula: invalid advisory lock mode")
	}
	want := Int8().id
	switch len(lock.keys) {
	case 1:
	case 2:
		want = Int4().id
	default:
		return nil, errors.New("tabula: an advisory lock needs one bigint or two integers")
	}
	c, err := newTypedCompiler(false)
	if err != nil {
		return nil, err
	}
	name := physicalName{"pg_catalog", function}
	params, types := make([]string, len(lock.keys)), make([]string, len(lock.keys))
	for i, key := range lock.keys {
		if key.part == nil || key.nullable || !sameSQLType(want, key.kind) {
			return nil, errors.New("tabula: advisory key needs the exact non-null PostgreSQL type")
		}
		value, err := renderTypedArgument(c, key.part, want)
		if err != nil {
			return nil, err
		}
		types[i] = want.qualified()
		params[i] = value
	}
	col := view.cols[0]
	call := name.sql() + "(" + strings.Join(params, ", ") + ")"
	query := "SELECT " + call + " AS " + typedQuote(col.name)
	if lock.wait == Wait {
		// The blocking overload returns void. A function scan calls it once and
		// produces one true row only after the non-null key has been acquired.
		query = "SELECT TRUE AS " + typedQuote(col.name) + " FROM " + call
	}
	c.hasSecret = col.secret
	sig := name.sql() + "(" + strings.Join(types, ", ") + ")"
	shape, err := c.finishShape(key+".lock", query, catalogType{}, StatementShape{
		Effects: EffectRead | EffectAdvisoryLock, SchemaUsage: []string{"pg_catalog"},
		Privileges: []DefinedPrivilege{{Function: sig, Privilege: "EXECUTE"}},
		Result:     []ShapeField{{Name: col.name, Type: Bool().id.qualified()}},
	})
	if err != nil {
		return nil, err
	}
	if err := view.registerShapes(key, []*compiledShape{shape}); err != nil {
		return nil, err
	}
	return &AdvisoryLockPlan[T, S]{effectRows[T, S]{view, shape}}, nil
}

// Acquire reports whether this transaction holds the requested lock. Wait
// returns true on success. Try returns false while another transaction holds
// a conflicting lock. Direct rejects both modes.
func (p *AdvisoryLockPlan[T, S]) Acquire(ctx context.Context, s Session[S], args ...Binding) (bool, error) {
	if p == nil || p.query.shape == nil {
		return false, ErrStatementAuthority
	}
	var held bool
	err := p.query.scan(ctx, s, CallScan, args, func() []any { return []any{&held} })
	return held, err
}
