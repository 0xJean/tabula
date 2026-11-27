package plan

import (
	"context"
	"errors"
	"strings"

	"github.com/0xJean/tabula"
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
	return AdvisoryLock{mode, wait, []advisoryKey{{key.part, typeID(key.kind), key.nullable}}}
}

// AdvisoryXactLockPair uses the separate PostgreSQL (integer, integer) overload.
func AdvisoryXactLockPair(mode AdvisoryMode, wait LockWait, first, second Expr[int32]) AdvisoryLock {
	return AdvisoryLock{mode, wait, []advisoryKey{
		{first.part, typeID(first.kind), first.nullable}, {second.part, typeID(second.kind), second.nullable},
	}}
}

// AdvisoryLockPlan acquires a registered advisory lock through an authorized
// transaction session. Its View owns the statement in the registry.
type AdvisoryLockPlan[T, S any] struct{ query effectRows[T, S] }

// DefineAdvisoryLock registers a transaction lock independently of any table
// access. The output view has one boolean column indicating acquisition.
func DefineAdvisoryLock[T, S any](view *tabula.View[T, S], key string, lock AdvisoryLock) *AdvisoryLockPlan[T, S] {
	p, err := TryDefineAdvisoryLock(view, key, lock)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineAdvisoryLock registers the complete operation or returns an error.
func TryDefineAdvisoryLock[T, S any](view *tabula.View[T, S], key string, lock AdvisoryLock) (*AdvisoryLockPlan[T, S], error) {
	owner := planView(view)
	if owner == nil || owner.Entry == nil || len(owner.Columns) != 1 || owner.Columns[0].GoType != typeID(Bool()).GoType || !typedIdent(owner.Columns[0].Name) || key == "" {
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
	want := typeID(Int8())
	switch len(lock.keys) {
	case 1:
	case 2:
		want = typeID(Int4())
	default:
		return nil, errors.New("tabula: an advisory lock needs one bigint or two integers")
	}
	c, err := newTypedCompiler(false)
	if err != nil {
		return nil, err
	}
	name := physicalName{Schema: "pg_catalog", Name: function}
	params, types := make([]string, len(lock.keys)), make([]string, len(lock.keys))
	for i, key := range lock.keys {
		if key.part == nil || key.nullable || !sameSQLType(want, key.kind) {
			return nil, errors.New("tabula: advisory key needs the exact non-null PostgreSQL type")
		}
		value, err := renderTypedArgument(c, key.part, want)
		if err != nil {
			return nil, err
		}
		types[i] = want.Qualified()
		params[i] = value
	}
	col := owner.Columns[0]
	call := name.SQL() + "(" + strings.Join(params, ", ") + ")"
	query := "SELECT " + call + " AS " + typedQuote(col.Name)
	if lock.wait == Wait {
		query = "SELECT TRUE AS " + typedQuote(col.Name) + " FROM " + call
	}
	c.hasSecret = col.Secret
	sig := name.SQL() + "(" + strings.Join(types, ", ") + ")"
	shape, err := c.finishShape(key+".lock", query, catalogType{}, tabula.StatementShape{
		Effects: tabula.EffectRead | tabula.EffectAdvisoryLock, SchemaUsage: []string{"pg_catalog"},
		Privileges: []tabula.DefinedPrivilege{{Function: sig, Privilege: "EXECUTE"}},
		Result:     []tabula.ShapeField{{Name: col.Name, Type: typeID(Bool()).Qualified()}},
	})
	if err != nil {
		return nil, err
	}
	if err := registerShapes(owner, key, []*compiledShape{shape}); err != nil {
		return nil, err
	}
	return &AdvisoryLockPlan[T, S]{effectRows[T, S]{owner, shape}}, nil
}

// BoundAdvisoryLockPlan executes a transaction-level advisory lock on one session.
type BoundAdvisoryLockPlan[T, S any] struct {
	plan    *AdvisoryLockPlan[T, S]
	session tabula.Session[S]
}

// Bind attaches the advisory lock plan to a session from its registry.
func (p *AdvisoryLockPlan[T, S]) Bind(s tabula.Session[S]) *BoundAdvisoryLockPlan[T, S] {
	return &BoundAdvisoryLockPlan[T, S]{plan: p, session: s}
}

// Acquire reports whether this transaction holds the requested lock. Wait
// returns true on success. Try returns false while another transaction holds
// a conflicting lock. Direct rejects both modes.
func (b *BoundAdvisoryLockPlan[T, S]) Acquire(ctx context.Context, args ...Binding) (bool, error) {
	if b == nil || b.plan == nil || b.plan.query.shape == nil {
		return false, tabula.ErrStatementAuthority
	}
	var held bool
	err := b.plan.query.scan(ctx, b.session, tabula.CallScan, args, func() []any { return []any{&held} })
	return held, err
}
