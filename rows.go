package tabula

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rows is a Table bound to one transaction Session. Bind it once per
// transaction: it remembers the once columns of rows it locked, and Save
// refuses a locked row whose once columns changed.
type Rows[T, K, S any] struct {
	t *Table[T, K, S]
	s Session[S]

	mu     sync.Mutex
	locked map[any][]driver.Value
}

// Listing is a List/Find page request: Order (table default when empty),
// Size/Offset (all rows when Size <= 0), and Lock (SELECT ... FOR UPDATE).
type Listing struct {
	Order        string
	Size, Offset int
	Lock         bool
}

func (r *Rows[T, K, S]) addrs(row *T) []any {
	out := make([]any, len(r.t.cols))
	for i, c := range r.t.cols {
		out[i] = c.target(row)
	}
	return out
}

func (r *Rows[T, K, S]) name(i int) string { return r.t.cols[i].name }

func (r *Rows[T, K, S]) fail(ctx context.Context, err error) error {
	return r.t.reg.classifyErr(ctx, err)
}

func (r *Rows[T, K, S]) one(ctx context.Context, tenant S, query string, args []any) (*T, error) {
	var row T
	err := r.s.Scan(ctx, tenant, query, args, r.addrs(&row)...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, r.fail(ctx, err)
	}
	return &row, nil
}

func (r *Rows[T, K, S]) byKey(tenant S, key K, lock bool) (string, []any) {
	k := r.t.cols[r.t.key].wrap(&key)
	if r.t.tenant < 0 || r.s.Platform() {
		if lock {
			return r.t.stmt.platformLock, []any{k}
		}
		return r.t.stmt.platformGet, []any{k}
	}
	tid := r.t.reg.tenantValue(tenant)
	if lock {
		return r.t.stmt.lock, []any{k, tid}
	}
	return r.t.stmt.get, []any{k, tid}
}

// Get reads the undeleted row key without a lock; nil when missing.
func (r *Rows[T, K, S]) Get(ctx context.Context, tenant S, key K) (*T, error) {
	query, args := r.byKey(tenant, key, false)
	return r.one(ctx, tenant, query, args)
}

// Lock reads the undeleted row key FOR UPDATE; nil when missing.
func (r *Rows[T, K, S]) Lock(ctx context.Context, tenant S, key K) (*T, error) {
	query, args := r.byKey(tenant, key, true)
	row, err := r.one(ctx, tenant, query, args)
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
	return r.one(ctx, tenant, query, f.Args())
}

// LockBy reads and locks (FOR UPDATE) the first undeleted row (in Order)
// matching where; nil when missing. The row's once columns are remembered.
func (r *Rows[T, K, S]) LockBy(ctx context.Context, tenant S, where string, values ...any) (*T, error) {
	f := r.Scope()
	f.And(where, values...)
	query := r.t.stmt.list + f.Where() + r.orderBy("") + " LIMIT 1 FOR UPDATE"
	row, err := r.one(ctx, tenant, query, f.Args())
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
	arg, err := r.bindKeys(keys)
	if err != nil {
		return nil, r.fail(ctx, err)
	}
	f := r.Scope()
	f.And(r.name(r.t.key)+" = ANY(?)", arg)
	query := r.t.stmt.list + f.Where() + r.orderBy("")
	return r.Select(ctx, tenant, query, f.Args()...)
}

// LockMany reads and locks (FOR UPDATE) all undeleted rows whose key is in
// keys in deterministic ascending key order to avoid deadlocks across
// concurrent transactions. Once columns of every locked row are remembered.
func (r *Rows[T, K, S]) LockMany(ctx context.Context, tenant S, keys []K) ([]T, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	arg, err := r.bindKeys(keys)
	if err != nil {
		return nil, r.fail(ctx, err)
	}
	f := r.Scope()
	f.And(r.name(r.t.key)+" = ANY(?)", arg)
	query := r.t.stmt.list + f.Where() + " ORDER BY " + r.name(r.t.key) + " ASC FOR UPDATE"
	rows, err := r.Select(ctx, tenant, query, f.Args()...)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		r.remember(row)
	}
	return rows, nil
}

func (r *Rows[T, K, S]) bindKeys(keys []K) (any, error) {
	wrap := r.t.cols[r.t.key].wrap
	vals := make([]driver.Value, len(keys))
	for i := range keys {
		k := keys[i]
		bound := wrap(&k)
		if valuer, ok := bound.(driver.Valuer); ok {
			v, err := valuer.Value()
			if err != nil {
				return nil, err
			}
			vals[i] = v
			continue
		}
		v, err := driver.DefaultParameterConverter.ConvertValue(k)
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
		f.And(r.name(r.t.deleted) + " IS NULL")
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
	f := r.Scope()
	if where != "" {
		f.And(where, values...)
	}
	var found bool
	query := "SELECT EXISTS (SELECT 1 FROM " + r.t.Name + " WHERE true" + f.Where() + ")"
	if err := r.Scan(ctx, tenant, query, f.Args(), &found); err != nil {
		return false, err
	}
	return found, nil
}

// List reads undeleted rows matching where for the listing plus total count.
func (r *Rows[T, K, S]) List(ctx context.Context, tenant S, where func(*Filter), l Listing) ([]T, int64, error) {
	f := r.Scope()
	if where != nil {
		where(f)
	}
	var total int64
	if err := r.s.Scan(ctx, tenant, r.t.stmt.count+f.Where(), f.Args(), &total); err != nil {
		return nil, 0, r.fail(ctx, err)
	}
	rows, err := r.findWithFilter(ctx, tenant, f, l)
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
	return r.findWithFilter(ctx, tenant, f, l)
}

func (r *Rows[T, K, S]) findWithFilter(ctx context.Context, tenant S, f *Filter, l Listing) ([]T, error) {
	query := r.t.stmt.list + f.Where() + r.orderBy(l.Order)
	if l.Size > 0 {
		query += f.Page(l.Size, l.Offset)
	}
	if l.Lock {
		query += " FOR UPDATE"
	}
	rows, err := r.Select(ctx, tenant, query, f.Args()...)
	if l.Lock {
		for _, row := range rows {
			r.remember(row)
		}
	}
	return rows, err
}

// Scan runs a hand-written single-row SELECT.
func (r *Rows[T, K, S]) Scan(ctx context.Context, tenant S, query string, args []any, dest ...any) error {
	if err := r.s.Scan(ctx, tenant, query, args, dest...); err != nil {
		return r.fail(ctx, err)
	}
	return nil
}

// Select scans every row of a hand-written SELECT over Table.Cols().
func (r *Rows[T, K, S]) Select(ctx context.Context, tenant S, query string, args ...any) ([]T, error) {
	var out []T
	err := r.s.Each(ctx, tenant, query, args, func(scan func(...any) error) error {
		var row T
		if err := scan(r.addrs(&row)...); err != nil {
			return err
		}
		out = append(out, row)
		return nil
	})
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
	n, err := r.s.Exec(ctx, tenant, query, args...)
	if err != nil {
		return r.fail(ctx, err)
	}
	if want >= 0 && n != want {
		return r.t.reg.rowCountErr()
	}
	return nil
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

// Insert writes row; with a serial key the generated key is read back into row.
func (r *Rows[T, K, S]) Insert(ctx context.Context, row *T) error {
	guard := r.guard(row)
	if !r.t.serial {
		return r.Exec(ctx, guard, 1, r.t.stmt.insert, r.Args(*row)...)
	}
	key := r.t.cols[r.t.key].target(row)
	if err := r.s.Returning(ctx, guard, r.t.stmt.insert, r.Args(*row), key); err != nil {
		return r.fail(ctx, err)
	}
	return nil
}

func (r *Rows[T, K, S]) where(row *T, addrs []any) (string, []any) {
	args := []any{addrs[r.t.key]}
	where := " WHERE " + r.name(r.t.key) + " = $1"
	if r.t.tenant >= 0 {
		args = append(args, r.t.reg.tenantValue(r.rowTenant(row)))
		where += " AND " + r.name(r.t.tenant) + " = $2"
	}
	if r.t.deleted >= 0 {
		where += " AND " + r.name(r.t.deleted) + " IS NULL"
	}
	return where, args
}

// Save writes every updatable column of the undeleted row at the previous
// version (when versioned); no row updated returns the conflict error.
func (r *Rows[T, K, S]) Save(ctx context.Context, row T) error {
	if err := r.unchanged(ctx, row); err != nil {
		return err
	}
	addrs := r.addrs(&row)
	_, args := r.where(&row, addrs)
	for _, i := range r.t.stmt.setColumns {
		args = append(args, addrs[i])
	}
	n, err := r.s.Exec(ctx, r.guard(&row), r.t.stmt.update, args...)
	if err != nil {
		return r.fail(ctx, err)
	}
	if n != 1 {
		return r.t.reg.conflictErr()
	}
	return nil
}

// SoftDelete is a Save of a row whose deleted column is set.
func (r *Rows[T, K, S]) SoftDelete(ctx context.Context, row T) error {
	if r.t.deleted < 0 {
		return r.t.reg.notDeletedErr()
	}
	if at := r.t.cols[r.t.deleted].field(&row).(**time.Time); *at == nil {
		return r.t.reg.notDeletedErr()
	}
	return r.Save(ctx, row)
}

// Set writes only the named columns of the undeleted row (no version check).
func (r *Rows[T, K, S]) Set(ctx context.Context, row T, columns ...string) error {
	addrs := r.addrs(&row)
	where, args := r.where(&row, addrs)
	sets := make([]string, 0, len(columns))
	for _, name := range columns {
		i, ok := r.t.byName[name]
		if !ok {
			panic(r.t.prefix() + ".Table " + r.t.Name + ": column not declared: " + name)
		}
		if r.t.cols[i].once {
			panic(r.t.prefix() + ".Table " + r.t.Name + ": column is insert-only: " + name)
		}
		args = append(args, addrs[i])
		sets = append(sets, name+" = $"+strconv.Itoa(len(args)))
	}
	return r.Exec(ctx, r.guard(&row), 1, "UPDATE "+r.t.Name+" SET "+strings.Join(sets, ", ")+where, args...)
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

func (r *Rows[T, K, S]) key(row *T) any { return *r.t.cols[r.t.key].field(row).(*K) }

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
