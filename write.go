package tabula

import (
	"context"
	"database/sql"
	"slices"
	"strconv"
	"strings"
)

const maxPostgresParams = 65535

// Insert writes row; with a serial key the generated key is read back into row.
func (r *Rows[T, K, S]) Insert(ctx context.Context, row *T) error {
	if err := r.CheckWrite(row); err != nil {
		return err
	}
	meta := stmtMeta{key: r.t.name + ".insert"}
	args, secret := r.Args(*row), r.t.insertSecretMask()
	if r.t.serial {
		_, err := r.returning(ctx, "insert", meta, r.t.stmt.insert, args, secret, sql.ErrNoRows, r.t.cols[r.t.keys[0]].target(row))
		return err
	}
	_, err := r.execWrite(ctx, "insert", meta, r.t.stmt.insert, args, secret, 1, 1, r.t.reg.rowCountErr())
	return err
}

// InsertReturning writes row and scans the named columns (or all table columns
// when columns is empty) back into row via RETURNING.
func (r *Rows[T, K, S]) InsertReturning(ctx context.Context, row *T, columns ...string) error {
	if err := r.CheckWrite(row); err != nil {
		return err
	}
	idx := r.t.resolveReturningColumns(columns)
	query := r.t.stmt.values + " RETURNING " + r.t.columnList(idx)
	_, err := r.returning(ctx, "insert_returning", stmtMeta{key: r.t.name + ".insert_returning"}, query, r.Args(*row), r.t.insertSecretMask(), sql.ErrNoRows, r.targets(row, idx)...)
	return err
}

// InsertMany inserts all rows using multi-row INSERT ... VALUES (...), (...).
// An empty slice returns nil without issuing SQL. Large batches are chunked
// automatically to stay within PostgreSQL's 65,535 bind parameter limit.
// All rows in a scoped call must share the session's scope.
func (r *Rows[T, K, S]) InsertMany(ctx context.Context, rows []T) error {
	_, err := r.writeMany(ctx, "insert_many", stmtMeta{key: r.t.name + ".insert_many"}, "", rows)
	return err
}

// writeMany checks every row before sending any chunk. conflict is the
// precompiled ON CONFLICT clause, without a single-row RETURNING list.
func (r *Rows[T, K, S]) writeMany(ctx context.Context, action string, meta stmtMeta, conflict string, rows []T) (int64, error) {
	if err := r.checkWriteAccess(); err != nil {
		return 0, err
	}
	for i := range rows {
		if err := r.CheckWrite(&rows[i]); err != nil {
			return 0, err
		}
	}
	perRow := len(r.t.stmt.insertColumns)
	size := 1
	if perRow > 0 {
		size = maxPostgresParams / perRow
	}
	mask := r.t.insertSecretMask()
	var total int64
	for chunk := range slices.Chunk(rows, size) {
		query, args, secret := r.batchValues(chunk, mask)
		minRows := int64(len(chunk))
		if conflict != "" {
			minRows = 0
		}
		n, err := r.execWrite(ctx, action, meta, query+conflict, args, secret, minRows, int64(len(chunk)), r.t.reg.rowCountErr())
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (r *Rows[T, K, S]) batchValues(chunk []T, mask []bool) (string, []any, []bool) {
	if len(mask) == 0 {
		return r.t.stmt.values, nil, nil
	}
	var b strings.Builder
	b.WriteString(r.t.stmt.insertPrefix)
	args := make([]any, 0, len(chunk)*len(mask))
	secrets := make([]bool, 0, cap(args))
	for i := range chunk {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for j, col := range r.t.stmt.insertColumns {
			if j > 0 {
				b.WriteString(", ")
			}
			args = append(args, r.t.cols[col].target(&chunk[i]))
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(len(args)))
		}
		b.WriteByte(')')
		secrets = append(secrets, mask...)
	}
	return b.String(), args, secrets
}

// Save writes every updatable column of the undeleted row at the previous
// version (when versioned); no row updated returns the conflict error.
func (r *Rows[T, K, S]) Save(ctx context.Context, row T) error {
	if err := r.CheckWrite(&row); err != nil {
		return err
	}
	if err := r.unchanged(ctx, row); err != nil {
		return err
	}
	args, secret := r.saveArgs(&row)
	_, err := r.execWrite(ctx, "save", stmtMeta{key: r.t.name + ".update"}, r.t.stmt.update, args, secret, 1, 1, r.t.reg.conflictErr())
	return err
}

// SaveReturning writes every updatable column of the undeleted row (honoring
// optimistic version CAS and once-column protection) and scans the named
// columns (or all columns when empty) back into row via RETURNING. No row
// matched returns the conflict error.
func (r *Rows[T, K, S]) SaveReturning(ctx context.Context, row *T, columns ...string) error {
	if err := r.CheckWrite(row); err != nil {
		return err
	}
	if err := r.unchanged(ctx, *row); err != nil {
		return err
	}
	idx := r.t.resolveReturningColumns(columns)
	query := r.t.stmt.update + " RETURNING " + r.t.columnList(idx)
	args, secret := r.saveArgs(row)
	_, err := r.returning(ctx, "save_returning", stmtMeta{key: r.t.name + ".save_returning"}, query, args, secret, r.t.reg.conflictErr(), r.targets(row, idx)...)
	return err
}

// SoftDelete is a Save of a row whose deleted column is marked deleted
// (non-nil *time.Time, true bool, or configured DeletedValue).
func (r *Rows[T, K, S]) SoftDelete(ctx context.Context, row T) error {
	if err := r.checkWriteAccess(); err != nil {
		return err
	}
	if !r.t.isRowDeleted(&row) {
		return r.t.reg.notDeletedErr()
	}
	return r.Save(ctx, row)
}

// Set writes only the named columns of the undeleted row (no version check).
func (r *Rows[T, K, S]) Set(ctx context.Context, row T, columns ...string) error {
	if err := r.CheckWrite(&row); err != nil {
		return err
	}
	query, args, secret := r.buildSetQuery(&row, columns, "", nil)
	_, err := r.execWrite(ctx, "set", stmtMeta{key: r.t.name + ".set"}, query, args, secret, 1, 1, r.t.reg.rowCountErr())
	return err
}

// UpdateWhere writes every updatable column of the undeleted row matching key
// (and scope/version when declared) plus an extra static SQL predicate over
// "?" placeholders. Reports whether a row matched; zero rows is not an error.
func (r *Rows[T, K, S]) UpdateWhere(ctx context.Context, row T, where string, values ...any) (bool, error) {
	if err := r.CheckWrite(&row); err != nil {
		return false, err
	}
	if err := r.unchanged(ctx, row); err != nil {
		return false, err
	}
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
	n, err := r.execWrite(ctx, "update_where", stmtMeta{key: r.t.name + ".update_where"}, query, args, secret, 0, 1, r.t.reg.rowCountErr())
	return n == 1 && err == nil, err
}

// SetWhere writes only the named columns of the undeleted row matching key
// (and scope when declared) plus an extra static SQL predicate over "?"
// placeholders. Reports whether a row matched; zero rows is not an error.
func (r *Rows[T, K, S]) SetWhere(ctx context.Context, row T, columns []string, where string, values ...any) (bool, error) {
	if err := r.CheckWrite(&row); err != nil {
		return false, err
	}
	query, args, secret := r.buildSetQuery(&row, columns, where, values)
	n, err := r.execWrite(ctx, "set_where", stmtMeta{key: r.t.name + ".set_where"}, query, args, secret, 0, 1, r.t.reg.rowCountErr())
	return n == 1 && err == nil, err
}

// Delete physically deletes the row identified by key within the session scope.
// Requires TableSpec.AllowDelete = true. Returns the conflict error when zero rows
// matched (missing or already deleted).
func (r *Rows[T, K, S]) Delete(ctx context.Context, key K) error {
	if err := r.checkWriteAccess(); err != nil {
		return err
	}
	if !r.t.allowDelete {
		panic(r.t.prefix() + ".Delete " + r.t.name + ": table does not enable AllowDelete")
	}
	args, scoped := r.keyArgs(key)
	query := r.t.stmt.unscopedDelete
	if scoped {
		query = r.t.stmt.deleteStmt
	}
	_, err := r.execWrite(ctx, "delete", stmtMeta{key: r.t.name + ".delete"}, query, args, nil, 1, 1, r.t.reg.conflictErr())
	return err
}

// DeleteWhere physically deletes rows matching the session scope and a required
// non-empty static predicate over "?" placeholders. Requires TableSpec.AllowDelete = true.
// Returns the affected row count; zero rows matched is not an error.
func (r *Rows[T, K, S]) DeleteWhere(ctx context.Context, where string, values ...any) (int64, error) {
	if err := r.checkWriteAccess(); err != nil {
		return 0, err
	}
	if !r.t.allowDelete {
		panic(r.t.prefix() + ".DeleteWhere " + r.t.name + ": table does not enable AllowDelete")
	}
	if strings.TrimSpace(where) == "" {
		panic(r.t.prefix() + ".DeleteWhere " + r.t.name + ": non-empty where predicate is required")
	}
	f := r.Scope()
	f.And(where, values...)
	query := "DELETE FROM " + r.t.name + " WHERE true" + f.Where()
	args := f.Args()
	n, err := r.execWrite(ctx, "delete_where", stmtMeta{key: r.t.name + ".delete_where"}, query, args, nil, 0, -1, nil)
	if err != nil {
		return 0, err
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
		panic(r.t.prefix() + ".Table " + r.t.name + ": at least one column is required")
	}
	addrs := r.addrs(row)
	where, args, secret := r.where(row, addrs)
	sets := make([]string, 0, len(columns))
	for _, name := range columns {
		i, ok := r.t.byName[name]
		if !ok {
			panic(r.t.prefix() + ".Table " + r.t.name + ": column not declared: " + name)
		}
		if r.t.cols[i].once {
			panic(r.t.prefix() + ".Table " + r.t.name + ": column is insert-only: " + name)
		}
		if r.t.cols[i].role == keyRole || r.t.cols[i].role == scopeRole {
			panic(r.t.prefix() + ".Table " + r.t.name + ": column cannot be updated: " + name)
		}
		args = append(args, addrs[i])
		secret = append(secret, r.t.cols[i].secret)
		sets = append(sets, name+" = $"+strconv.Itoa(len(args)))
	}
	query := "UPDATE " + r.t.name + " SET " + strings.Join(sets, ", ") + where
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
