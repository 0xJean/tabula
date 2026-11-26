package tabula

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
)

// ScalarAggregates declares the columns to verify for each PostgreSQL aggregate.
// Only listed pairs enter the definition and SQL snapshot. Runtime calls may
// still use any declared column compatible with the PostgreSQL function.
type ScalarAggregates struct {
	Max, Min, Sum, Avg []string
}

func (a ScalarAggregates) columns(yield func(string, string) bool) {
	for _, group := range []struct {
		fn   string
		cols []string
	}{
		{"MAX", a.Max}, {"MIN", a.Min}, {"SUM", a.Sum}, {"AVG", a.Avg},
	} {
		for _, col := range group.cols {
			if !yield(group.fn, col) {
				return
			}
		}
	}
}

func scalarSQL(fn, col, table string) string {
	return "SELECT " + fn + "(" + col + ") FROM " + table + " WHERE true"
}

// Max evaluates SELECT MAX(col) over live rows matching the session scope and
// where filter. Choose V for PostgreSQL's result type; registered registry
// codecs are reused automatically. SQL NULL (empty set or all NULL values)
// returns (zero, false, nil); a genuine zero returns (zero, true, nil).
func Max[V, T, K, S any](ctx context.Context, rows *Rows[T, K, S], col string, where func(*Filter)) (V, bool, error) {
	return scalarAggregate[V](ctx, rows, "max", "MAX", col, where)
}

// Min evaluates SELECT MIN(col) over live rows matching the session scope and
// where filter. See Max for NULL semantics and result-type selection.
func Min[V, T, K, S any](ctx context.Context, rows *Rows[T, K, S], col string, where func(*Filter)) (V, bool, error) {
	return scalarAggregate[V](ctx, rows, "min", "MIN", col, where)
}

// Sum evaluates SELECT SUM(col) over live rows matching the session scope and
// where filter. See Max for NULL semantics and result-type selection.
func Sum[V, T, K, S any](ctx context.Context, rows *Rows[T, K, S], col string, where func(*Filter)) (V, bool, error) {
	return scalarAggregate[V](ctx, rows, "sum", "SUM", col, where)
}

// Avg evaluates SELECT AVG(col) over live rows matching the session scope and
// where filter. See Max for NULL semantics and result-type selection.
func Avg[V, T, K, S any](ctx context.Context, rows *Rows[T, K, S], col string, where func(*Filter)) (V, bool, error) {
	return scalarAggregate[V](ctx, rows, "avg", "AVG", col, where)
}

// CountBy evaluates SELECT col, count(*) ... GROUP BY col over live rows
// matching the session scope and where filter. col must be declared on the
// table and whitelisted in TableSpec.GroupBy. An empty result returns a
// non-nil empty map. Grouping keys must be non-null.
func CountBy[V comparable, T, K, S any](ctx context.Context, rows *Rows[T, K, S], col string, where func(*Filter)) (map[V]int64, error) {
	if rows == nil || rows.t == nil {
		return nil, fmt.Errorf("tabula.CountBy: rows binding is required")
	}
	if err := rows.checkOperation(selectOp); err != nil {
		return nil, err
	}
	if where != nil {
		if err := rows.rejectDynamic(); err != nil {
			return nil, err
		}
	}
	if _, ok := rows.t.byName[col]; !ok {
		return nil, fmt.Errorf("%s.CountBy %s: column not declared: %s", rows.t.prefix(), rows.t.name, col)
	}
	if !rows.t.groupBy[col] {
		return nil, fmt.Errorf("%s.CountBy %s: column %s is not whitelisted in TableSpec.GroupBy", rows.t.prefix(), rows.t.name, col)
	}
	wrap, err := rows.t.reg.resolveAggregateValue(reflect.TypeFor[V]())
	if err != nil {
		return nil, fmt.Errorf("%s.CountBy %s: %w", rows.t.prefix(), rows.t.name, err)
	}
	f := rows.Scope()
	if where != nil {
		where(f)
	}
	query := "SELECT " + col + ", count(*) FROM " + rows.t.name + " WHERE true" + f.Where() + " GROUP BY " + col
	args := f.Args()
	action := "count_by"
	meta := rows.fixedMeta("count_by_"+col, true)
	opCtx, span := rows.start(ctx, action, meta, nil)
	out := make(map[V]int64)
	var rowCount int64
	execErr := rows.executor(meta, nil).Each(opCtx, query, args, func(scan func(...any) error) error {
		var k V
		var n int64
		if err := scan(wrap(&k), &n); err != nil {
			return err
		}
		out[k] = n
		rowCount++
		return nil
	})
	span.finish(opCtx, query, rowCount, execErr, args, nil, meta.secret, false)
	if execErr != nil {
		return nil, rows.fail(opCtx, execErr)
	}
	return out, nil
}

func scalarAggregate[V, T, K, S any](ctx context.Context, rows *Rows[T, K, S], action, fnName, col string, where func(*Filter)) (V, bool, error) {
	var zero V
	if rows == nil || rows.t == nil {
		return zero, false, fmt.Errorf("tabula.%s: rows binding is required", fnName)
	}
	if err := rows.checkOperation(selectOp); err != nil {
		return zero, false, err
	}
	if where != nil {
		if err := rows.rejectDynamic(); err != nil {
			return zero, false, err
		}
	}
	if _, ok := rows.t.byName[col]; !ok {
		return zero, false, fmt.Errorf("%s.%s %s: column not declared: %s", rows.t.prefix(), fnName, rows.t.name, col)
	}
	target, readResult, err := nullableAggregateTarget[V](rows.t.reg)
	if err != nil {
		return zero, false, fmt.Errorf("%s.%s %s: %w", rows.t.prefix(), fnName, rows.t.name, err)
	}
	f := rows.Scope()
	if where != nil {
		where(f)
	}
	query := scalarSQL(fnName, col, rows.t.name) + f.Where()
	args := f.Args()
	meta := rows.fixedMeta(action+"_"+col, true)
	if err := rows.scan(ctx, action, meta, query, args, target); err != nil {
		return zero, false, err
	}
	val, present := readResult()
	return val, present, nil
}

func (r *Registry[S]) resolveAggregateValue(ft reflect.Type) (func(any) any, error) {
	if ft.Kind() == reflect.Pointer {
		return nil, fmt.Errorf("aggregate target type %s must not be a pointer", ft)
	}
	r.mu.RLock()
	wrap, ok := r.types[ft]
	r.mu.RUnlock()
	if ok {
		return wrap, nil
	}
	return func(p any) any { return p }, nil
}

type nullableCodecScanner struct {
	inner sql.Scanner
	valid bool
}

func (s *nullableCodecScanner) Scan(src any) error {
	if src == nil {
		s.valid = false
		return nil
	}
	s.valid = true
	return s.inner.Scan(src)
}

func nullableAggregateTarget[V, S any](r *Registry[S]) (any, func() (V, bool), error) {
	ft := reflect.TypeFor[V]()
	if ft.Kind() == reflect.Pointer {
		return nil, nil, fmt.Errorf("aggregate target type %s must not be a pointer", ft)
	}
	r.mu.RLock()
	wrap, hasCustom := r.types[ft]
	r.mu.RUnlock()
	if hasCustom {
		var val V
		wrapped := wrap(&val)
		sc, ok := wrapped.(sql.Scanner)
		if !ok {
			return nil, nil, fmt.Errorf("registered codec for %s does not implement sql.Scanner", ft)
		}
		ns := &nullableCodecScanner{inner: sc}
		return ns, func() (V, bool) {
			if !ns.valid {
				var zero V
				return zero, false
			}
			return val, true
		}, nil
	}
	var dst sql.Null[V]
	return &dst, func() (V, bool) {
		if !dst.Valid {
			var zero V
			return zero, false
		}
		return dst.V, true
	}, nil
}
