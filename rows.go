package tabula

import (
	"bytes"
	"context"
	"database/sql/driver"
	"iter"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rows is a Table bound to one Session. Bind it once per
// transaction: it remembers the once columns of rows it locked, and Save /
// UpdateWhere refuse a locked row whose once columns changed.
type Rows[T, K, S any] struct {
	readBinding[T, S]
	t     *Table[T, K, S]
	locks *lockedRows
}

type lockedRows struct {
	mu     sync.Mutex
	locked map[any][]driver.Value
}

// WithLogger returns a Rows handle sharing the same Table and Session with a
// per-binding logger override.
func (r *Rows[T, K, S]) WithLogger(logger *slog.Logger) *Rows[T, K, S] {
	clone := *r
	clone.logger = logger
	return &clone
}

// Listing is a List/Find page request: Order (table default when empty),
// Size/Offset (all rows when Size <= 0), Lock (a PostgreSQL row-lock mode), and
// Count (CountExact default, CountNone, or CountEstimate).
type Listing struct {
	Order        string
	Size, Offset int
	Lock         LockMode
	Count        CountMode
}

func (r *Rows[T, K, S]) name(i int) string { return r.t.cols[i].name }

func (r *Rows[T, K, S]) keyArgs(key K) (args []any, scoped bool) {
	args = make([]any, 0, len(r.t.keys)+1)
	for _, bind := range r.t.keyBinders {
		args = append(args, bind(&key))
	}
	scoped = r.t.scope >= 0 && !r.s.Unscoped()
	if scoped {
		args = append(args, r.t.reg.scopeValue(r.s.Scope()))
	}
	return args, scoped
}

func (r *Rows[T, K, S]) byKey(key K, lock bool) (string, []any) {
	args, scoped := r.keyArgs(key)
	if scoped {
		if lock {
			return r.t.stmt.lock, args
		}
		return r.t.stmt.get, args
	}
	if lock {
		return r.t.stmt.unscopedLock, args
	}
	return r.t.stmt.unscopedGet, args
}

// Get reads the undeleted row key without a lock; nil when missing.
func (r *Rows[T, K, S]) Get(ctx context.Context, key K) (*T, error) {
	if r.bindErr != nil {
		return nil, r.bindErr
	}
	query, args := r.byKey(key, false)
	return r.one(ctx, "get", stmtMeta{key: r.t.name + ".get"}, query, args)
}

// Lock reads the undeleted row key FOR UPDATE; nil when missing.
func (r *Rows[T, K, S]) Lock(ctx context.Context, key K) (*T, error) {
	if r.bindErr != nil {
		return nil, r.bindErr
	}
	if r.readOnly() {
		return nil, ErrReadOnly
	}
	query, args := r.byKey(key, true)
	row, err := r.one(ctx, "lock", stmtMeta{key: r.t.name + ".lock"}, query, args)
	if row != nil {
		r.remember(*row)
	}
	return row, err
}

// GetBy reads the first undeleted row (in Order) matching where; nil when
// missing. where is a static predicate over bound "?" markers.
func (r *Rows[T, K, S]) GetBy(ctx context.Context, where string, values ...any) (*T, error) {
	if r.bindErr != nil {
		return nil, r.bindErr
	}
	f := r.Scope()
	f.And(where, values...)
	query := r.t.stmt.list + f.Where() + r.orderBy("") + " LIMIT 1"
	return r.one(ctx, "get_by", stmtMeta{key: r.t.name + ".get_by"}, query, f.Args())
}

// LockBy reads and locks (FOR UPDATE) the first undeleted row (in Order)
// matching where; nil when missing. The row's once columns are remembered.
func (r *Rows[T, K, S]) LockBy(ctx context.Context, where string, values ...any) (*T, error) {
	if r.bindErr != nil {
		return nil, r.bindErr
	}
	if r.readOnly() {
		return nil, ErrReadOnly
	}
	f := r.Scope()
	f.And(where, values...)
	query := r.t.stmt.list + f.Where() + r.orderBy("") + " LIMIT 1 FOR UPDATE"
	row, err := r.one(ctx, "lock_by", stmtMeta{key: r.t.name + ".lock_by"}, query, f.Args())
	if row != nil {
		r.remember(*row)
	}
	return row, err
}

// GetMany reads all undeleted rows whose key is in keys within the session
// scope. An empty keys slice returns nil, nil without issuing SQL.
func (r *Rows[T, K, S]) GetMany(ctx context.Context, keys []K) ([]T, error) {
	if r.bindErr != nil {
		return nil, r.bindErr
	}
	if len(keys) == 0 {
		return nil, nil
	}
	f := r.Scope()
	if err := r.appendKeyInFilter(f, keys); err != nil {
		return nil, r.fail(ctx, err)
	}
	query := r.t.stmt.list + f.Where() + r.orderBy("")
	return r.selectOp(ctx, "get_many", stmtMeta{key: r.t.name + ".get_many"}, query, f.Args())
}

// LockMany reads and locks (FOR UPDATE) all undeleted rows whose key is in
// keys in deterministic ascending key order to avoid deadlocks across
// concurrent transactions. Once columns of every locked row are remembered.
func (r *Rows[T, K, S]) LockMany(ctx context.Context, keys []K) ([]T, error) {
	if r.bindErr != nil {
		return nil, r.bindErr
	}
	if r.readOnly() {
		return nil, ErrReadOnly
	}
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
	rows, err := r.selectOp(ctx, "lock_many", stmtMeta{key: r.t.name + ".lock_many"}, query, f.Args())
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

// Scope starts a Filter seeded with the session scope predicate (none on an
// unscoped session) and soft-delete predicate. A rejected binding yields a
// FALSE predicate without consulting the session.
func (r *Rows[T, K, S]) Scope() *Filter {
	if r.bindErr != nil {
		f := NewFilter()
		f.And("FALSE")
		return f
	}
	if r.t.scope >= 0 && !r.s.Unscoped() {
		return r.t.liveFilter(true, r.t.reg.scopeValue(r.s.Scope()))
	}
	return r.t.liveFilter(false, nil)
}

func (t *Table[T, K, S]) liveFilter(scoped bool, scopeValue any) *Filter {
	f := NewFilter()
	if scoped {
		f.And(t.cols[t.scope].name+" = ?", scopeValue)
	}
	if t.deleted >= 0 {
		f.And(t.del.aliveSQL)
	}
	return f
}

func (r *Rows[T, K, S]) orderBy(order string) string {
	if order == "" {
		order = r.t.order
	}
	if order == "" {
		return ""
	}
	return " ORDER BY " + order
}

// Exists reports whether any undeleted row in the session's row scope matches
// where (a Filter builder callback; nil checks whether any live row exists).
func (r *Rows[T, K, S]) Exists(ctx context.Context, where func(*Filter)) (bool, error) {
	if r.bindErr != nil {
		return false, r.bindErr
	}
	f := r.Scope()
	if where != nil {
		where(f)
	}
	return r.execExists(ctx, f)
}

// ExistsBy reports whether any undeleted row in the session's row scope
// matches where (a static predicate over bound "?" markers).
func (r *Rows[T, K, S]) ExistsBy(ctx context.Context, where string, values ...any) (bool, error) {
	if r.bindErr != nil {
		return false, r.bindErr
	}
	f := r.Scope()
	if where != "" {
		f.And(where, values...)
	}
	return r.execExists(ctx, f)
}

func (r *Rows[T, K, S]) execExists(ctx context.Context, f *Filter) (bool, error) {
	ctx, span := r.start(ctx, "exists", stmtMeta{key: r.t.name + ".exists"}, nil)
	var found bool
	query := "SELECT EXISTS (SELECT 1 FROM " + r.t.name + " WHERE true" + f.Where() + ")"
	args := f.Args()
	err := r.s.Scan(ctx, query, args, &found)
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
func (r *Rows[T, K, S]) List(ctx context.Context, where func(*Filter), l Listing) ([]T, int64, error) {
	if r.bindErr != nil {
		return nil, 0, r.bindErr
	}
	if l.Lock != LockNone && r.readOnly() {
		return nil, 0, ErrReadOnly
	}
	_ = l.Lock.clause()
	f := r.Scope()
	if where != nil {
		where(f)
	}
	total := int64(-1)
	if l.Count != CountNone {
		action, query, args := "count", r.t.stmt.count+f.Where(), f.Args()
		if l.Count == CountEstimate {
			action = "count_estimate"
			query = "SELECT greatest(reltuples::bigint, 0) FROM pg_class WHERE oid = $1::regclass"
			args = []any{r.t.name}
		}
		if err := r.scan(ctx, action, stmtMeta{key: r.t.name + "." + action}, query, args, &total); err != nil {
			return nil, 0, err
		}
	}
	rows, err := collect(r.iterListing(ctx, "list", f, l))
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// Find reads undeleted rows matching where without running COUNT(*).
func (r *Rows[T, K, S]) Find(ctx context.Context, where func(*Filter), l Listing) ([]T, error) {
	if r.bindErr != nil {
		return nil, r.bindErr
	}
	f := r.Scope()
	if where != nil {
		where(f)
	}
	return collect(r.iterListing(ctx, "find", f, l))
}

// Iter streams a Find query, applying the same scope, ordering, paging and lock
// mode. Breaking the range closes the cursor. Locked rows retain once-column snapshots.
func (r *Rows[T, K, S]) Iter(ctx context.Context, where func(*Filter), l Listing) iter.Seq2[T, error] {
	if r.bindErr != nil {
		return failedIter[T](r.bindErr)
	}
	return func(yield func(T, error) bool) {
		f := r.Scope()
		if where != nil {
			where(f)
		}
		r.iterListing(ctx, "iter", f, l)(yield)
	}
}

func (r *Rows[T, K, S]) iterListing(ctx context.Context, action string, f *Filter, l Listing) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		if l.Lock != LockNone && r.readOnly() {
			var zero T
			yield(zero, ErrReadOnly)
			return
		}
		query := r.t.stmt.list + f.Where() + r.orderBy(l.Order)
		if l.Size > 0 {
			query += f.Page(l.Size, l.Offset)
		}
		query += l.Lock.clause()
		seq := r.stream(ctx, action, stmtMeta{key: r.t.name + "." + action}, query, f.Args())
		if l.Lock == LockNone {
			seq(yield)
			return
		}
		for row, err := range seq {
			if err == nil {
				r.remember(row)
			}
			if !yield(row, err) {
				return
			}
		}
	}
}

// Scan runs a hand-written single-row SELECT.
func (r *Rows[T, K, S]) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	return r.scan(ctx, "scan", r.t.lookupMeta(query, "scan"), query, args, dest...)
}

// Select scans every row of a hand-written SELECT over Table.Cols().
func (r *Rows[T, K, S]) Select(ctx context.Context, query string, args ...any) ([]T, error) {
	return r.selectOp(ctx, "select", r.t.lookupMeta(query, "select"), query, args)
}

// SelectIter streams a hand-written SELECT over Table.Cols().
func (r *Rows[T, K, S]) SelectIter(ctx context.Context, query string, args ...any) iter.Seq2[T, error] {
	return r.stream(ctx, "select_iter", r.t.lookupMeta(query, "select_iter"), query, args)
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
func (r *Rows[T, K, S]) Exec(ctx context.Context, want int64, query string, args ...any) error {
	if err := r.checkWriteAccess(); err != nil {
		return err
	}
	_, err := r.execWrite(ctx, "exec", r.t.lookupMeta(query, "exec"), query, args, nil, want, want, r.t.reg.rowCountErr())
	return err
}

func (r *Rows[T, K, S]) readOnly() bool {
	return r.t.readOnly || r.directReadOnly
}

func (r *Rows[T, K, S]) checkWriteAccess() error {
	if r.bindErr != nil {
		return r.bindErr
	}
	if r.readOnly() {
		return ErrReadOnly
	}
	if r.t.reg != nil && r.t.reg.deny&DenyUnscopedWrites != 0 && r.s.Unscoped() {
		return ErrCapabilityDenied
	}
	return nil
}

// CheckCopy validates session provenance, write mode, DenyCopy and
// DenyUnscopedWrites before bulk COPY protocols (such as pgxdriver.CopyFrom)
// take any empty-input shortcut or touch the driver.
func (r *Rows[T, K, S]) CheckCopy() error {
	if r.bindErr != nil {
		return r.bindErr
	}
	if r.readOnly() {
		return ErrReadOnly
	}
	if r.t.reg != nil && r.t.reg.deny&DenyCopy != 0 {
		return ErrCapabilityDenied
	}
	if r.t.reg != nil && r.t.reg.deny&DenyUnscopedWrites != 0 && r.s.Unscoped() {
		return ErrCapabilityDenied
	}
	return nil
}

// CheckWrite validates session provenance, table mode, unscoped-write policy
// and row scope for a write. Driver adapters use it when sending rows through
// a bulk protocol such as COPY.
func (r *Rows[T, K, S]) CheckWrite(row *T) error {
	if err := r.checkWriteAccess(); err != nil {
		return err
	}
	if r.t.scope < 0 || r.s.Unscoped() {
		return nil
	}
	want := r.t.reg.scopeValue(r.s.Scope())
	got := r.t.reg.scopeValue(r.rowScope(row))
	if !reflect.DeepEqual(want, got) {
		return ErrScopeMismatch
	}
	return nil
}

func (r *Rows[T, K, S]) rowScope(row *T) S {
	return *r.t.cols[r.t.scope].field(row).(*S)
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
	if r.t.scope >= 0 {
		args = append(args, r.t.reg.scopeValue(r.rowScope(row)))
		secret = append(secret, r.t.cols[r.t.scope].secret)
		where += " AND " + r.name(r.t.scope) + " = $" + strconv.Itoa(len(args))
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
	r.locks.mu.Lock()
	defer r.locks.mu.Unlock()
	if r.locks.locked == nil {
		r.locks.locked = map[any][]driver.Value{}
	}
	r.locks.locked[r.key(&row)] = values
}

func (r *Rows[T, K, S]) unchanged(ctx context.Context, row T) error {
	if r.locks == nil {
		return nil
	}
	r.locks.mu.Lock()
	before, ok := r.locks.locked[r.key(&row)]
	r.locks.mu.Unlock()
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
