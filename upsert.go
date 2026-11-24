package tabula

import (
	"context"

	"errors"
	"fmt"
	"strings"
)

// UpsertSpec declares a controlled PostgreSQL INSERT ... ON CONFLICT statement.
// Conflict columns identify the unique index or constraint target. Update lists
// the columns updated on conflict (key, scope, and once columns are forbidden);
// leaving Update empty produces ON CONFLICT (...) DO NOTHING.
type UpsertSpec struct {
	Conflict  []string
	Update    []string
	Where     string
	Returning []string
}

// UpsertPlan is a validated, registered upsert statement bound to one Table.
type UpsertPlan[T, K, S any] struct {
	t        *Table[T, K, S]
	key      string
	sql      string
	conflict string
	retIdx   []int
	meta     stmtMeta
}

// SQL returns the derived INSERT ... ON CONFLICT statement.
func (u *UpsertPlan[T, K, S]) SQL() string { return u.sql }

// Key returns the registered statement key.
func (u *UpsertPlan[T, K, S]) Key() string { return u.key }

// DefineUpsert validates spec against t, registers its SQL statement under key
// in the Registry (so sqlcheck verifies it), and returns an executable plan.
// Panics on declaration errors at package initialization.
func (t *Table[T, K, S]) DefineUpsert(key string, spec UpsertSpec, opts ...StatementOption) *UpsertPlan[T, K, S] {
	plan, err := t.TryDefineUpsert(key, spec, opts...)
	if err != nil {
		panic(err)
	}
	t.Statement(key, plan.sql, opts...)
	return plan
}

// TryDefineUpsert validates spec and builds an UpsertPlan without recording it
// in the Registry.
func (t *Table[T, K, S]) TryDefineUpsert(key string, spec UpsertSpec, opts ...StatementOption) (*UpsertPlan[T, K, S], error) {
	if t.reg == nil {
		return nil, errors.New("tabula.Upsert " + t.name + ": table is not defined")
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf(t.prefix()+".Upsert "+t.name+": "+format, args...)
	}
	if t.readOnly {
		return nil, fail("cannot define upsert on a ReadOnly table")
	}
	if t.reg.deny&DenyUpsert != 0 {
		return nil, fmt.Errorf("%w: %s.Upsert %s: upsert is denied by registry policy", ErrCapabilityDenied, t.prefix(), t.name)
	}
	if key == "" {
		return nil, fail("statement key is required")
	}
	if len(spec.Conflict) == 0 {
		return nil, fail("conflict columns are required")
	}
	seenConflict := make(map[string]bool, len(spec.Conflict))
	for _, col := range spec.Conflict {
		if _, ok := t.byName[col]; !ok {
			return nil, fail("conflict column %s is not declared", col)
		}
		if seenConflict[col] {
			return nil, fail("conflict column %s declared twice", col)
		}
		seenConflict[col] = true
	}

	var b strings.Builder

	b.WriteString(" ON CONFLICT (")
	b.WriteString(strings.Join(spec.Conflict, ", "))
	b.WriteString(") ")

	if len(spec.Update) == 0 {
		if strings.TrimSpace(spec.Where) != "" {
			return nil, fail("Where requires at least one Update column")
		}
		b.WriteString("DO NOTHING")
	} else {
		seenUpdate := make(map[string]bool, len(spec.Update))
		sets := make([]string, 0, len(spec.Update))
		for _, col := range spec.Update {
			idx, ok := t.byName[col]
			if !ok {
				return nil, fail("update column %s is not declared", col)
			}
			if seenUpdate[col] {
				return nil, fail("update column %s declared twice", col)
			}
			c := t.cols[idx]
			if c.role == keyRole || c.role == scopeRole {
				return nil, fail("column %s (%s) cannot be updated on conflict", col, c.role)
			}
			if c.once {
				return nil, fail("insert-only column %s cannot be updated on conflict", col)
			}
			seenUpdate[col] = true
			sets = append(sets, col+" = EXCLUDED."+col)
		}
		b.WriteString("DO UPDATE SET ")
		b.WriteString(strings.Join(sets, ", "))
		where := strings.TrimSpace(spec.Where)
		if t.scope >= 0 && !seenConflict[t.cols[t.scope].name] {
			col := t.cols[t.scope].name
			scopeGuard := t.name + "." + col + " = EXCLUDED." + col
			if where != "" {
				where = "(" + where + ") AND " + scopeGuard
			} else {
				where = scopeGuard
			}
		}
		if where != "" {
			b.WriteString(" WHERE ")
			b.WriteString(where)
		}
	}

	conflict := b.String()
	var retIdx []int
	if len(spec.Returning) > 0 {
		retIdx = make([]int, len(spec.Returning))
		for i, col := range spec.Returning {
			idx, ok := t.byName[col]
			if !ok {
				return nil, fail("returning column %s is not declared", col)
			}
			retIdx[i] = idx
		}
		b.WriteString(" RETURNING ")
		b.WriteString(strings.Join(spec.Returning, ", "))
	}

	return &UpsertPlan[T, K, S]{t: t, key: key, sql: t.stmt.values + b.String(), conflict: conflict, retIdx: retIdx, meta: statementMeta(t.name+"."+key, opts)}, nil
}

// Upsert executes a pre-declared UpsertPlan for row. Returns (true, nil) when
// a row was inserted or updated, and (false, nil) when DO NOTHING or a DO UPDATE
// WHERE guard skipped the write. Any Returning columns are scanned into row.
func (r *Rows[T, K, S]) Upsert(ctx context.Context, plan *UpsertPlan[T, K, S], row *T) (bool, error) {
	if err := r.CheckWrite(row); err != nil {
		return false, err
	}
	r.checkPlan(plan)
	args, secret := r.Args(*row), r.t.insertSecretMask()
	if len(plan.retIdx) > 0 {
		return r.returning(ctx, "upsert", plan.meta, plan.sql, args, secret, nil, r.targets(row, plan.retIdx)...)
	}
	n, err := r.execWrite(ctx, "upsert", plan.meta, plan.sql, args, secret, 0, 1, r.t.reg.rowCountErr())
	return n == 1 && err == nil, err
}

// UpsertMany returns the number of inserted or updated rows, excluding
// conflicts skipped by DO NOTHING or WHERE. The batch only returns a count:
// it omits the plan's RETURNING list and leaves the input slice untouched.
// On error the count covers successful chunks in the current transaction.
func (r *Rows[T, K, S]) UpsertMany(ctx context.Context, plan *UpsertPlan[T, K, S], rows []T) (int64, error) {
	if r.bindErr != nil {
		return 0, r.bindErr
	}
	if err := r.checkWriteAccess(); err != nil {
		return 0, err
	}
	r.checkPlan(plan)
	return r.writeMany(ctx, "upsert_many", plan.meta, plan.conflict, rows)
}

func (r *Rows[T, K, S]) checkPlan(plan *UpsertPlan[T, K, S]) {
	if plan == nil || plan.t != r.t {
		panic(r.t.prefix() + ".Upsert " + r.t.name + ": upsert plan does not belong to this table")
	}
}
