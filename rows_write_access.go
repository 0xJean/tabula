package tabula

import (
	"bytes"
	"context"
	"database/sql/driver"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Args returns bound values matching Table.Insert().
func (r *Rows[T, K, S]) Args(row T) []any {
	return r.targets(&row, r.t.stmt.insertColumns)
}

func (r *Rows[T, K, S]) readOnly() bool {
	return r.t.readOnly || r.directReadOnly
}

func (r *Rows[T, K, S]) checkWriteAccess(op tableOperation) error {
	if r.bindErr != nil {
		return r.bindErr
	}
	if r.readOnly() {
		return ErrReadOnly
	}
	if !r.t.mode.allows(op) {
		return ErrOperationDenied
	}
	if op == rawOp && r.authorized != nil {
		return ErrLegacySQL
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
	if err := r.checkWriteAccess(insertOp); err != nil {
		return err
	}
	if r.t.mode == WriteInsertOnly && len(r.t.stmt.insertColumns) == 0 {
		return ErrOperationDenied
	}
	if r.t.reg != nil && r.t.reg.deny&DenyCopy != 0 {
		return ErrCapabilityDenied
	}
	return nil
}

// CheckWrite validates session provenance, permission to insert, unscoped-write
// policy and row scope for the supplied row. Driver adapters use it when
// sending rows through a bulk protocol such as COPY.
func (r *Rows[T, K, S]) CheckWrite(row *T) error {
	return r.checkRowWrite(insertOp, row)
}

func (r *Rows[T, K, S]) checkRowWrite(op tableOperation, row *T, columns ...string) error {
	if err := r.checkWriteAccess(op); err != nil {
		return err
	}
	for _, col := range columns {
		if !r.t.allowsUpdateColumn(col) {
			return ErrOperationDenied
		}
	}
	return r.checkRowScope(row)
}

func (r *Rows[T, K, S]) checkRowScope(row *T) error {
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
