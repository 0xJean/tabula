package tabula

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// UpsertSpec declares a controlled PostgreSQL INSERT ... ON CONFLICT statement.
// Conflict columns identify the unique index or constraint target. Update lists
// the columns updated on conflict (key, tenant, and once columns are forbidden);
// leaving Update empty produces ON CONFLICT (...) DO NOTHING.
type UpsertSpec struct {
	Conflict  []string
	Update    []string
	Where     string
	Returning []string
}

// UpsertPlan is a validated, registered upsert statement bound to one Table.
type UpsertPlan[T, K, S any] struct {
	t      *Table[T, K, S]
	key    string
	sql    string
	retIdx []int
	meta   stmtMeta
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
		return nil, errors.New("tabula.Upsert " + t.Name + ": table is not defined")
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf(t.prefix()+".Upsert "+t.Name+": "+format, args...)
	}
	if t.ReadOnly {
		return nil, fail("cannot define upsert on a ReadOnly table")
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
	b.WriteString(t.stmt.values)
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
			if c.role == keyRole || c.role == tenantRole {
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
		if w := strings.TrimSpace(spec.Where); w != "" {
			b.WriteString(" WHERE ")
			b.WriteString(w)
		}
	}

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

	meta := stmtMeta{key: t.Name + "." + key}
	for _, opt := range opts {
		if opt != nil {
			opt(&meta)
		}
	}
	return &UpsertPlan[T, K, S]{
		t:      t,
		key:    key,
		sql:    b.String(),
		retIdx: retIdx,
		meta:   meta,
	}, nil
}

// Upsert executes a pre-declared UpsertPlan for row. Returns (true, nil) when
// a row was inserted or updated, and (false, nil) when DO NOTHING or a DO UPDATE
// WHERE guard skipped the write. Any Returning columns are scanned into row.
func (r *Rows[T, K, S]) Upsert(ctx context.Context, plan *UpsertPlan[T, K, S], row *T) (bool, error) {
	if plan == nil || plan.t != r.t {
		panic(r.t.prefix() + ".Upsert " + r.t.Name + ": upsert plan does not belong to this table")
	}
	guard := r.guard(row)
	ctx, span := r.start(ctx, guard, "upsert", plan.meta.key)
	args := r.Args(*row)
	secret := r.t.insertSecretMask()

	if len(plan.retIdx) > 0 {
		dest := r.targets(row, plan.retIdx)
		err := r.s.Returning(ctx, guard, plan.sql, args, dest...)
		if errors.Is(err, sql.ErrNoRows) {
			span.finish(ctx, plan.sql, 0, nil, args, secret, plan.meta.secret, plan.meta.quiet)
			return false, nil
		}
		if err != nil {
			span.finish(ctx, plan.sql, 0, err, args, secret, plan.meta.secret, plan.meta.quiet)
			return false, r.fail(ctx, err)
		}
		span.finish(ctx, plan.sql, 1, nil, args, secret, plan.meta.secret, plan.meta.quiet)
		return true, nil
	}

	n, err := r.s.Exec(ctx, guard, plan.sql, args...)
	var retErr error
	if err != nil {
		retErr = r.fail(ctx, err)
	} else if n > 1 {
		retErr = r.t.reg.rowCountErr()
	}
	span.finish(ctx, plan.sql, n, logErr(err, retErr), args, secret, plan.meta.secret, plan.meta.quiet)
	if retErr != nil {
		return false, retErr
	}
	return n == 1, nil
}
