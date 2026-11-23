package tabula

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rows is a Table bound to one transaction Session. Bind it once per
// transaction: it remembers the once columns of rows it locked, and Save /
// UpdateWhere refuse a locked row whose once columns changed.
type Rows[T, K, S any] struct {
	t      *Table[T, K, S]
	s      Session[S]
	logger *slog.Logger

	mu     sync.Mutex
	locked map[any][]driver.Value
}

// WithLogger returns a Rows handle sharing the same Table and Session with a
// per-binding logger override.
func (r *Rows[T, K, S]) WithLogger(logger *slog.Logger) *Rows[T, K, S] {
	return &Rows[T, K, S]{t: r.t, s: r.s, logger: logger}
}

// Listing is a List/Find page request: Order (table default when empty),
// Size/Offset (all rows when Size <= 0), Lock (SELECT ... FOR UPDATE), and
// Count (CountExact default, CountNone, or CountEstimate).
type Listing struct {
	Order        string
	Size, Offset int
	Lock         bool
	Count        CountMode
}

func (r *Rows[T, K, S]) fillAddrs(dst []any, row *T) []any {
	for i, c := range r.t.cols {
		dst[i] = c.target(row)
	}
	return dst
}

func (r *Rows[T, K, S]) addrs(row *T) []any {
	return r.fillAddrs(make([]any, len(r.t.cols)), row)
}

func (r *Rows[T, K, S]) name(i int) string { return r.t.cols[i].name }

func (r *Rows[T, K, S]) fail(ctx context.Context, err error) error {
	return r.t.reg.classifyErr(ctx, err)
}

func (r *Rows[T, K, S]) start(ctx context.Context, scope S, action, key string) (context.Context, opSpan[S]) {
	return r.t.reg.startOp(ctx, r.logger, scope, r.t.Name, action, key)
}

func (r *Rows[T, K, S]) one(ctx context.Context, tenant S, action, key, query string, args []any) (*T, error) {
	ctx, span := r.start(ctx, tenant, action, key)
	var row T
	err := r.s.Scan(ctx, tenant, query, args, r.addrs(&row)...)
	if errors.Is(err, sql.ErrNoRows) {
		span.finish(ctx, query, 0, nil, args, nil, false, false)
		return nil, nil
	}
	if err != nil {
		span.finish(ctx, query, 0, err, args, nil, false, false)
		return nil, r.fail(ctx, err)
	}
	span.finish(ctx, query, 1, nil, args, nil, false, false)
	return &row, nil
}

func (r *Rows[T, K, S]) byKey(tenant S, key K, lock bool) (string, []any) {
	args := make([]any, 0, len(r.t.keys)+1)
	for _, bind := range r.t.keyBinders {
		args = append(args, bind(&key))
	}
	if r.t.tenant < 0 || r.s.Platform() {
		if lock {
			return r.t.stmt.platformLock, args
		}
		return r.t.stmt.platformGet, args
	}
	args = append(args, r.t.reg.tenantValue(tenant))
	if lock {
		return r.t.stmt.lock, args
	}
	return r.t.stmt.get, args
}

// Get reads the undeleted row key without a lock; nil when missing.
func (r *Rows[T, K, S]) Get(ctx context.Context, tenant S, key K) (*T, error) {
	query, args := r.byKey(tenant, key, false)
	return r.one(ctx, tenant, "get", r.t.Name+".get", query, args)
}

// Lock reads the undeleted row key FOR UPDATE; nil when missing.
func (r *Rows[T, K, S]) Lock(ctx context.Context, tenant S, key K) (*T, error) {
	query, args := r.byKey(tenant, key, true)
	row, err := r.one(ctx, tenant, "lock", r.t.Name+".lock", query, args)
	if row != nil {
		r.remember(*row)
	}
	return row, err
}

// GetBy reads the first undeleted row (in Order) matching where; nil when
// missing. where is a static predicate over bound "?" markers.
func (r *Rows[T, K, S]) GetBy(ctx context.Context, tenant S, where string, values ...any) (*T, error) {
	f := r.Scope()
	f.And(where, values...)
	query := r.t.stmt.list + f.Where() + r.orderBy("") + " LIMIT 1"
	return r.one(ctx, tenant, "get_by", r.t.Name+".get_by", query, f.Args())
}

// LockBy reads and locks (FOR UPDATE) the first undeleted row (in Order)
// matching where; nil when missing. The row's once columns are remembered.
func (r *Rows[T, K, S]) LockBy(ctx context.Context, tenant S, where string, values ...any) (*T, error) {
	f := r.Scope()
	f.And(where, values...)
	query := r.t.stmt.list + f.Where() + r.orderBy("") + " LIMIT 1 FOR UPDATE"
	row, err := r.one(ctx, tenant, "lock_by", r.t.Name+".lock_by", query, f.Args())
	if row != nil {
		r.remember(*row)
	}
	return row, err
}

// GetMany reads all undeleted rows whose key is in keys within the session
// scope. An empty keys slice returns nil, nil without issuing SQL.
func (r *Rows[T, K, S]) GetMany(ctx context.Context, tenant S, keys []K) ([]T, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	f := r.Scope()
	if err := r.appendKeyInFilter(f, keys); err != nil {
		return nil, r.fail(ctx, err)
	}
	query := r.t.stmt.list + f.Where() + r.orderBy("")
	return r.selectOp(ctx, tenant, "get_many", r.t.Name+".get_many", query, f.Args(), false, false)
}

// LockMany reads and locks (FOR UPDATE) all undeleted rows whose key is in
// keys in deterministic ascending key order to avoid deadlocks across
// concurrent transactions. Once columns of every locked row are remembered.
func (r *Rows[T, K, S]) LockMany(ctx context.Context, tenant S, keys []K) ([]T, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	f := r.Scope()
	if err := r.appendKeyInFilter(f, keys); err != nil {
		return nil, r.fail(ctx, err)
	}
	orderCols := make([]string, len(r.t.keys))
	for i, keyIdx := range r.t.keys {
		orderCols[i] = r.name(keyIdx) + " ASC"
	}
	query := r.t.stmt.list + f.Where() + " ORDER BY " + strings.Join(orderCols, ", ") + " FOR UPDATE"
	rows, err := r.selectOp(ctx, tenant, "lock_many", r.t.Name+".lock_many", query, f.Args(), false, false)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		r.remember(row)
	}
	return rows, nil
}

func (r *Rows[T, K, S]) appendKeyInFilter(f *Filter, keys []K) error {
	if len(r.t.keys) == 1 {
		arg, err := r.bindSingleKeys(keys)
		if err != nil {
			return err
		}
		f.And(r.name(r.t.keys[0])+" = ANY(?)", arg)
		return nil
	}
	cols := make([]string, len(r.t.keys))
	for i, keyIdx := range r.t.keys {
		cols[i] = r.name(keyIdx)
	}
	tuplePlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(r.t.keys)), ", ") + ")"
	tuples := make([]string, len(keys))
	vals := make([]any, 0, len(keys)*len(r.t.keys))
	for i := range keys {
		tuples[i] = tuplePlaceholder
		k := keys[i]
		for _, bind := range r.t.keyBinders {
			vals = append(vals, bind(&k))
		}
	}
	f.And("("+strings.Join(cols, ", ")+") IN ("+strings.Join(tuples, ", ")+")", vals...)
	return nil
}

func (r *Rows[T, K, S]) bindSingleKeys(keys []K) (any, error) {
	bind := r.t.keyBinders[0]
	vals := make([]driver.Value, len(keys))
	for i := range keys {
		k := keys[i]
		bound := bind(&k)
		if valuer, ok := bound.(driver.Valuer); ok {
			v, err := valuer.Value()
			if err != nil {
				return nil, err
			}
			vals[i] = v
			continue
		}
		v, err := driver.DefaultParameterConverter.ConvertValue(bound)
		if err != nil {
			return nil, err
		}
		vals[i] = v
	}
	switch vals[0].(type) {
	case string:
		out := make([]string, len(vals))
		for i, v := range vals {
			out[i] = v.(string)
		}
		return r.t.reg.arrayArg(out), nil
	case int64:
		out := make([]int64, len(vals))
		for i, v := range vals {
			out[i] = v.(int64)
		}
		return r.t.reg.arrayArg(out), nil
	default:
		return r.t.reg.arrayArg(keys), nil
	}
}

// Scope starts a Filter seeded with the session tenant predicate (none on a
// platform session) and soft-delete predicate.
func (r *Rows[T, K, S]) Scope() *Filter {
	f := NewFilter()
	if r.t.tenant >= 0 && !r.s.Platform() {
		f.And(r.name(r.t.tenant)+" = ?", r.t.reg.tenantValue(r.s.Tenant()))
	}
	if r.t.deleted >= 0 {
		f.And(r.t.del.aliveSQL)
	}
	return f
}

func (r *Rows[T, K, S]) orderBy(order string) string {
	if order == "" {
		order = r.t.Order
	}
	if order == "" {
		return ""
	}
	return " ORDER BY " + order
}

// ExistsBy reports whether any undeleted row in the session's row scope
// matches where (a static predicate over bound "?" markers).
func (r *Rows[T, K, S]) ExistsBy(ctx context.Context, tenant S, where string, values ...any) (bool, error) {
	ctx, span := r.start(ctx, tenant, "exists", r.t.Name+".exists")
	f := r.Scope()
	if where != "" {
		f.And(where, values...)
	}
	var found bool
	query := "SELECT EXISTS (SELECT 1 FROM " + r.t.Name + " WHERE true" + f.Where() + ")"
	args := f.Args()
	err := r.s.Scan(ctx, tenant, query, args, &found)
	rows := int64(0)
	if err == nil && found {
		rows = 1
	}
	span.finish(ctx, query, rows, err, args, nil, false, false)
	if err != nil {
		return false, r.fail(ctx, err)
	}
	return found, nil
}

// List reads undeleted rows matching where for the listing plus total count
// according to l.Count (CountExact default, CountNone returns -1, CountEstimate
// reads PostgreSQL pg_class.reltuples).
func (r *Rows[T, K, S]) List(ctx context.Context, tenant S, where func(*Filter), l Listing) ([]T, int64, error) {
	f := r.Scope()
	if where != nil {
		where(f)
	}
	var total int64
	switch l.Count {
	case CountNone:
		total = -1
	case CountEstimate:
		estCtx, estSpan := r.start(ctx, tenant, "count_estimate", r.t.Name+".count_estimate")
		estQuery := "SELECT greatest(reltuples::bigint, 0) FROM pg_class WHERE oid = $1::regclass"
		estArgs := []any{r.t.Name}
		err := r.s.Scan(estCtx, tenant, estQuery, estArgs, &total)
		estSpan.finish(estCtx, estQuery, -1, err, estArgs, nil, false, false)
		if err != nil {
			return nil, 0, r.fail(ctx, err)
		}
	default:
		cntCtx, cntSpan := r.start(ctx, tenant, "count", r.t.Name+".count")
		countQuery := r.t.stmt.count + f.Where()
		countArgs := f.Args()
		err := r.s.Scan(cntCtx, tenant, countQuery, countArgs, &total)
		cntSpan.finish(cntCtx, countQuery, -1, err, countArgs, nil, false, false)
		if err != nil {
			return nil, 0, r.fail(ctx, err)
		}
	}
	rows, err := r.findWithFilter(ctx, tenant, "list", f, l)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// Find reads undeleted rows matching where without running COUNT(*).
func (r *Rows[T, K, S]) Find(ctx context.Context, tenant S, where func(*Filter), l Listing) ([]T, error) {
	f := r.Scope()
	if where != nil {
		where(f)
	}
	return r.findWithFilter(ctx, tenant, "find", f, l)
}

func (r *Rows[T, K, S]) findWithFilter(ctx context.Context, tenant S, action string, f *Filter, l Listing) ([]T, error) {
	query := r.t.stmt.list + f.Where() + r.orderBy(l.Order)
	if l.Size > 0 {
		query += f.Page(l.Size, l.Offset)
	}
	if l.Lock {
		query += " FOR UPDATE"
	}
	rows, err := r.selectOp(ctx, tenant, action, r.t.Name+"."+action, query, f.Args(), false, false)
	if l.Lock {
		for _, row := range rows {
			r.remember(row)
		}
	}
	return rows, err
}

// Scan runs a hand-written single-row SELECT.
func (r *Rows[T, K, S]) Scan(ctx context.Context, tenant S, query string, args []any, dest ...any) error {
	meta := r.t.lookupMeta(query, "scan")
	ctx, span := r.start(ctx, tenant, "scan", meta.key)
	err := r.s.Scan(ctx, tenant, query, args, dest...)
	span.finish(ctx, query, -1, err, args, nil, meta.secret, meta.quiet)
	if err != nil {
		return r.fail(ctx, err)
	}
	return nil
}

// Select scans every row of a hand-written SELECT over Table.Cols().
func (r *Rows[T, K, S]) Select(ctx context.Context, tenant S, query string, args ...any) ([]T, error) {
	meta := r.t.lookupMeta(query, "select")
	return r.selectOp(ctx, tenant, "select", meta.key, query, args, meta.secret, meta.quiet)
}

func (r *Rows[T, K, S]) selectOp(ctx context.Context, tenant S, action, key, query string, args []any, allSecret, quiet bool) ([]T, error) {
	ctx, span := r.start(ctx, tenant, action, key)
	var out []T
	dst := make([]any, len(r.t.cols))
	err := r.s.Each(ctx, tenant, query, args, func(scan func(...any) error) error {
		var row T
		if err := scan(r.fillAddrs(dst, &row)...); err != nil {
			return err
		}
		out = append(out, row)
		return nil
	})
	span.finish(ctx, query, int64(len(out)), err, args, nil, allSecret, quiet)
	if err != nil {
		return nil, r.fail(ctx, err)
	}
	return out, nil
}

// Args returns bound values matching Table.Insert().
func (r *Rows[T, K, S]) Args(row T) []any {
	addrs := r.addrs(&row)
	out := make([]any, len(r.t.stmt.insertColumns))
	for n, i := range r.t.stmt.insertColumns {
		out[n] = addrs[i]
	}
	return out
}

// Exec runs a hand-written write and confirms its row count (when want >= 0).
func (r *Rows[T, K, S]) Exec(ctx context.Context, tenant S, want int64, query string, args ...any) error {
	meta := r.t.lookupMeta(query, "exec")
	ctx, span := r.start(ctx, tenant, "exec", meta.key)
	n, err := r.s.Exec(ctx, tenant, query, args...)
	var retErr error
	if err != nil {
		retErr = r.fail(ctx, err)
	} else if want >= 0 && n != want {
		retErr = r.t.reg.rowCountErr()
	}
	span.finish(ctx, query, n, logErr(err, retErr), args, nil, meta.secret, meta.quiet)
	return retErr
}

func (r *Rows[T, K, S]) guard(row *T) S {
	if r.t.tenant < 0 || r.s.Platform() {
		return r.s.Tenant()
	}
	return r.rowTenant(row)
}

func (r *Rows[T, K, S]) rowTenant(row *T) S {
	return *r.t.cols[r.t.tenant].field(row).(*S)
}

func (r *Rows[T, K, S]) where(row *T, addrs []any) (string, []any, []bool) {
	args := make([]any, 0, len(r.t.keys)+1)
	secret := make([]bool, 0, len(r.t.keys)+1)
	parts := make([]string, len(r.t.keys))
	for pos, keyIdx := range r.t.keys {
		args = append(args, addrs[keyIdx])
		secret = append(secret, r.t.cols[keyIdx].secret)
		parts[pos] = r.name(keyIdx) + " = $" + strconv.Itoa(pos+1)
	}
	where := " WHERE " + strings.Join(parts, " AND ")
	if r.t.tenant >= 0 {
		args = append(args, r.t.reg.tenantValue(r.rowTenant(row)))
		secret = append(secret, r.t.cols[r.t.tenant].secret)
		where += " AND " + r.name(r.t.tenant) + " = $" + strconv.Itoa(len(args))
	}
	if r.t.deleted >= 0 {
		where += " AND " + r.t.del.aliveSQL
	}
	return where, args, secret
}

func (r *Rows[T, K, S]) onceValues(row *T) ([]driver.Value, error) {
	out := make([]driver.Value, len(r.t.stmt.onceColumns))
	for n, i := range r.t.stmt.onceColumns {
		v, err := driver.DefaultParameterConverter.ConvertValue(r.t.cols[i].target(row))
		if err != nil {
			return nil, err
		}
		out[n] = v
	}
	return out, nil
}

func (r *Rows[T, K, S]) key(row *T) any { return r.t.keyBuilder(row) }

func (r *Rows[T, K, S]) remember(row T) {
	if len(r.t.stmt.onceColumns) == 0 {
		return
	}
	values, err := r.onceValues(&row)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locked == nil {
		r.locked = map[any][]driver.Value{}
	}
	r.locked[r.key(&row)] = values
}

func (r *Rows[T, K, S]) unchanged(ctx context.Context, row T) error {
	r.mu.Lock()
	before, ok := r.locked[r.key(&row)]
	r.mu.Unlock()
	if !ok {
		return nil
	}
	after, err := r.onceValues(&row)
	if err != nil {
		return r.fail(ctx, err)
	}
	for n := range before {
		if !sameValue(before[n], after[n]) {
			return r.t.reg.onceErr()
		}
	}
	return nil
}

func sameValue(a, b driver.Value) bool {
	switch x := a.(type) {
	case time.Time:
		y, ok := b.(time.Time)
		return ok && x.Equal(y)
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return a == b
}
