package tabula

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

const maxPostgresParams = 65535

// Insert writes row; with a serial key the generated key is read back into row.
func (r *Rows[T, K, S]) Insert(ctx context.Context, row *T) error {
	if r.t.ReadOnly {
		return ErrReadOnly
	}
	guard := r.guard(row)
	ctx, span := r.start(ctx, guard, "insert", r.t.Name+".insert")
	args := r.Args(*row)
	secret := r.t.insertSecretMask()
	if !r.t.serial {
		n, err := r.s.Exec(ctx, guard, r.t.stmt.insert, args...)
		var retErr error
		if err != nil {
			retErr = r.fail(ctx, err)
		} else if n != 1 {
			retErr = r.t.reg.rowCountErr()
		}
		span.finish(ctx, r.t.stmt.insert, n, logErr(err, retErr), args, secret, false, false)
		return retErr
	}
	key := r.t.cols[r.t.keys[0]].target(row)
	err := r.s.Returning(ctx, guard, r.t.stmt.insert, args, key)
	var retErr error
	rows := int64(1)
	if err != nil {
		rows = 0
		retErr = r.fail(ctx, err)
	}
	span.finish(ctx, r.t.stmt.insert, rows, err, args, secret, false, false)
	return retErr
}

// InsertReturning writes row and scans the named columns (or all table columns
// when columns is empty) back into row via RETURNING.
func (r *Rows[T, K, S]) InsertReturning(ctx context.Context, row *T, columns ...string) error {
	if r.t.ReadOnly {
		return ErrReadOnly
	}
	guard := r.guard(row)
	ctx, span := r.start(ctx, guard, "insert_returning", r.t.Name+".insert_returning")
	retIdx := r.t.resolveReturningColumns(columns)
	query := r.t.stmt.values + " RETURNING " + r.t.columnList(retIdx)
	args := r.Args(*row)
	dest := r.targets(row, retIdx)
	err := r.s.Returning(ctx, guard, query, args, dest...)
	var retErr error
	rows := int64(1)
	if err != nil {
		rows = 0
		retErr = r.fail(ctx, err)
	}
	span.finish(ctx, query, rows, err, args, r.t.insertSecretMask(), false, false)
	return retErr
}

// InsertMany inserts all rows using multi-row INSERT ... VALUES (...), (...).
// An empty slice returns nil without issuing SQL. Large batches are chunked
// automatically to stay within PostgreSQL's 65,535 bind parameter limit.
// All rows in a non-platform tenant-scoped call must share the same tenant.
func (r *Rows[T, K, S]) InsertMany(ctx context.Context, rows []T) error {
	if r.t.ReadOnly {
		return ErrReadOnly
	}
	if len(rows) == 0 {
		return nil
	}
	perRow := len(r.t.stmt.insertColumns)
	if perRow == 0 {
		return nil
	}
	guard := r.guard(&rows[0])
	if r.t.tenant >= 0 && !r.s.Platform() {
		wantTenant := r.t.reg.tenantValue(guard)
		for i := 1; i < len(rows); i++ {
			if r.t.reg.tenantValue(r.rowTenant(&rows[i])) != wantTenant {
				return r.t.reg.rowCountErr()
			}
		}
	}
	maxBatch := max(1, maxPostgresParams/perRow)
	colSecret := r.t.insertSecretMask()
	for offset := 0; offset < len(rows); offset += maxBatch {
		end := min(offset+maxBatch, len(rows))
		chunk := rows[offset:end]
		if err := r.insertManyChunk(ctx, guard, chunk, colSecret); err != nil {
			return err
		}
	}
	return nil
}

func (r *Rows[T, K, S]) insertManyChunk(ctx context.Context, guard S, chunk []T, colSecret []bool) error {
	ctx, span := r.start(ctx, guard, "insert_many", r.t.Name+".insert_many")
	perRow := len(r.t.stmt.insertColumns)
	var b strings.Builder
	b.WriteString("INSERT INTO ")
	b.WriteString(r.t.Name)
	b.WriteString(" (")
	b.WriteString(r.t.columnList(r.t.stmt.insertColumns))
	b.WriteString(") VALUES ")

	args := make([]any, 0, len(chunk)*perRow)
	secret := make([]bool, 0, len(chunk)*perRow)
	param := 1
	for i := range chunk {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		rowArgs := r.Args(chunk[i])
		for j, a := range rowArgs {
			if j > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(param))
			param++
			args = append(args, a)
			secret = append(secret, colSecret[j])
		}
		b.WriteByte(')')
	}
	query := b.String()
	n, err := r.s.Exec(ctx, guard, query, args...)
	var retErr error
	if err != nil {
		retErr = r.fail(ctx, err)
	} else if n != int64(len(chunk)) {
		retErr = r.t.reg.rowCountErr()
	}
	span.finish(ctx, query, n, logErr(err, retErr), args, secret, false, false)
	return retErr
}

// Save writes every updatable column of the undeleted row at the previous
// version (when versioned); no row updated returns the conflict error.
func (r *Rows[T, K, S]) Save(ctx context.Context, row T) error {
	if r.t.ReadOnly {
		return ErrReadOnly
	}
	if err := r.unchanged(ctx, row); err != nil {
		return err
	}
	guard := r.guard(&row)
	ctx, span := r.start(ctx, guard, "save", r.t.Name+".update")
	args, secret := r.saveArgs(&row)
	n, err := r.s.Exec(ctx, guard, r.t.stmt.update, args...)
	var retErr error
	if err != nil {
		retErr = r.fail(ctx, err)
	} else if n != 1 {
		retErr = r.t.reg.conflictErr()
	}
	span.finish(ctx, r.t.stmt.update, n, logErr(err, retErr), args, secret, false, false)
	return retErr
}

// SaveReturning writes every updatable column of the undeleted row (honoring
// optimistic version CAS and once-column protection) and scans the named
// columns (or all columns when empty) back into row via RETURNING. No row
// matched returns the conflict error.
func (r *Rows[T, K, S]) SaveReturning(ctx context.Context, row *T, columns ...string) error {
	if r.t.ReadOnly {
		return ErrReadOnly
	}
	if err := r.unchanged(ctx, *row); err != nil {
		return err
	}
	guard := r.guard(row)
	ctx, span := r.start(ctx, guard, "save_returning", r.t.Name+".save_returning")
	retIdx := r.t.resolveReturningColumns(columns)
	query := r.t.stmt.update + " RETURNING " + r.t.columnList(retIdx)
	args, secret := r.saveArgs(row)
	dest := r.targets(row, retIdx)
	err := r.s.Returning(ctx, guard, query, args, dest...)
	var retErr error
	rows := int64(1)
	if errors.Is(err, sql.ErrNoRows) {
		rows = 0
		retErr = r.t.reg.conflictErr()
	} else if err != nil {
		rows = 0
		retErr = r.fail(ctx, err)
	}
	span.finish(ctx, query, rows, logErr(err, retErr), args, secret, false, false)
	return retErr
}

// SoftDelete is a Save of a row whose deleted column is marked deleted
// (non-nil *time.Time, true bool, or configured DeletedValue).
func (r *Rows[T, K, S]) SoftDelete(ctx context.Context, row T) error {
	if !r.t.isRowDeleted(&row) {
		return r.t.reg.notDeletedErr()
	}
	return r.Save(ctx, row)
}

// Set writes only the named columns of the undeleted row (no version check).
func (r *Rows[T, K, S]) Set(ctx context.Context, row T, columns ...string) error {
	if r.t.ReadOnly {
		return ErrReadOnly
	}
	guard := r.guard(&row)
	ctx, span := r.start(ctx, guard, "set", r.t.Name+".set")
	query, args, secret := r.buildSetQuery(&row, columns, "", nil)
	n, err := r.s.Exec(ctx, guard, query, args...)
	var retErr error
	if err != nil {
		retErr = r.fail(ctx, err)
	} else if n != 1 {
		retErr = r.t.reg.rowCountErr()
	}
	span.finish(ctx, query, n, logErr(err, retErr), args, secret, false, false)
	return retErr
}

// UpdateWhere writes every updatable column of the undeleted row matching key
// (and tenant/version when declared) plus an extra static SQL predicate over
// "?" placeholders. Reports whether a row matched; zero rows is not an error.
func (r *Rows[T, K, S]) UpdateWhere(ctx context.Context, row T, where string, values ...any) (bool, error) {
	if r.t.ReadOnly {
		return false, ErrReadOnly
	}
	if err := r.unchanged(ctx, row); err != nil {
		return false, err
	}
	guard := r.guard(&row)
	ctx, span := r.start(ctx, guard, "update_where", r.t.Name+".update_where")
	args, secret := r.saveArgs(&row)
	query := r.t.stmt.update
	if where != "" {
		f := NewFilter(args...)
		f.And(where, values...)
		query += f.Where()
		args = f.Args()
		for range values {
			secret = append(secret, false)
		}
	}
	n, err := r.s.Exec(ctx, guard, query, args...)
	var retErr error
	if err != nil {
		retErr = r.fail(ctx, err)
	} else if n > 1 {
		retErr = r.t.reg.rowCountErr()
	}
	span.finish(ctx, query, n, logErr(err, retErr), args, secret, false, false)
	if retErr != nil {
		return false, retErr
	}
	return n == 1, nil
}

// SetWhere writes only the named columns of the undeleted row matching key
// (and tenant when declared) plus an extra static SQL predicate over "?"
// placeholders. Reports whether a row matched; zero rows is not an error.
func (r *Rows[T, K, S]) SetWhere(ctx context.Context, row T, columns []string, where string, values ...any) (bool, error) {
	if r.t.ReadOnly {
		return false, ErrReadOnly
	}
	guard := r.guard(&row)
	ctx, span := r.start(ctx, guard, "set_where", r.t.Name+".set_where")
	query, args, secret := r.buildSetQuery(&row, columns, where, values)
	n, err := r.s.Exec(ctx, guard, query, args...)
	var retErr error
	if err != nil {
		retErr = r.fail(ctx, err)
	} else if n > 1 {
		retErr = r.t.reg.rowCountErr()
	}
	span.finish(ctx, query, n, logErr(err, retErr), args, secret, false, false)
	if retErr != nil {
		return false, retErr
	}
	return n == 1, nil
}

// Delete physically deletes the row identified by key within the session scope.
// Requires Table.AllowDelete = true. Returns the conflict error when zero rows
// matched (missing or already deleted).
func (r *Rows[T, K, S]) Delete(ctx context.Context, tenant S, key K) error {
	if r.t.ReadOnly {
		return ErrReadOnly
	}
	if !r.t.AllowDelete {
		panic(r.t.prefix() + ".Delete " + r.t.Name + ": table does not enable AllowDelete")
	}
	args := make([]any, 0, len(r.t.keys)+1)
	for _, bind := range r.t.keyBinders {
		args = append(args, bind(&key))
	}
	query := r.t.stmt.platformDelete
	if r.t.tenant >= 0 && !r.s.Platform() {
		query = r.t.stmt.deleteStmt
		args = append(args, r.t.reg.tenantValue(tenant))
	}
	ctx, span := r.start(ctx, tenant, "delete", r.t.Name+".delete")
	n, err := r.s.Exec(ctx, tenant, query, args...)
	var retErr error
	if err != nil {
		retErr = r.fail(ctx, err)
	} else if n != 1 {
		retErr = r.t.reg.conflictErr()
	}
	span.finish(ctx, query, n, logErr(err, retErr), args, nil, false, false)
	return retErr
}

// DeleteWhere physically deletes rows matching the session scope and a required
// non-empty static predicate over "?" placeholders. Requires Table.AllowDelete = true.
// Returns the affected row count; zero rows matched is not an error.
func (r *Rows[T, K, S]) DeleteWhere(ctx context.Context, tenant S, where string, values ...any) (int64, error) {
	if r.t.ReadOnly {
		return 0, ErrReadOnly
	}
	if !r.t.AllowDelete {
		panic(r.t.prefix() + ".DeleteWhere " + r.t.Name + ": table does not enable AllowDelete")
	}
	if strings.TrimSpace(where) == "" {
		panic(r.t.prefix() + ".DeleteWhere " + r.t.Name + ": non-empty where predicate is required")
	}
	f := r.Scope()
	f.And(where, values...)
	query := "DELETE FROM " + r.t.Name + " WHERE true" + f.Where()
	args := f.Args()
	ctx, span := r.start(ctx, tenant, "delete_where", r.t.Name+".delete_where")
	n, err := r.s.Exec(ctx, tenant, query, args...)
	var retErr error
	if err != nil {
		retErr = r.fail(ctx, err)
	}
	span.finish(ctx, query, n, logErr(err, retErr), args, nil, false, false)
	if retErr != nil {
		return 0, retErr
	}
	return n, nil
}

func (r *Rows[T, K, S]) saveArgs(row *T) ([]any, []bool) {
	addrs := r.addrs(row)
	_, args, secret := r.where(row, addrs)
	for _, i := range r.t.stmt.setColumns {
		args = append(args, addrs[i])
		secret = append(secret, r.t.cols[i].secret)
	}
	return args, secret
}

func (r *Rows[T, K, S]) buildSetQuery(row *T, columns []string, extraWhere string, extraValues []any) (string, []any, []bool) {
	if len(columns) == 0 {
		panic(r.t.prefix() + ".Table " + r.t.Name + ": at least one column is required")
	}
	addrs := r.addrs(row)
	where, args, secret := r.where(row, addrs)
	sets := make([]string, 0, len(columns))
	for _, name := range columns {
		i, ok := r.t.byName[name]
		if !ok {
			panic(r.t.prefix() + ".Table " + r.t.Name + ": column not declared: " + name)
		}
		if r.t.cols[i].once {
			panic(r.t.prefix() + ".Table " + r.t.Name + ": column is insert-only: " + name)
		}
		if r.t.cols[i].role == keyRole || r.t.cols[i].role == tenantRole {
			panic(r.t.prefix() + ".Table " + r.t.Name + ": column cannot be updated: " + name)
		}
		args = append(args, addrs[i])
		secret = append(secret, r.t.cols[i].secret)
		sets = append(sets, name+" = $"+strconv.Itoa(len(args)))
	}
	query := "UPDATE " + r.t.Name + " SET " + strings.Join(sets, ", ") + where
	if extraWhere != "" {
		f := NewFilter(args...)
		f.And(extraWhere, extraValues...)
		query += f.Where()
		args = f.Args()
		for range extraValues {
			secret = append(secret, false)
		}
	}
	return query, args, secret
}

func (r *Rows[T, K, S]) targets(row *T, idx []int) []any {
	out := make([]any, len(idx))
	for n, i := range idx {
		out[n] = r.t.cols[i].target(row)
	}
	return out
}

func logErr(raw, ret error) error {
	if raw != nil {
		return raw
	}
	return ret
}
