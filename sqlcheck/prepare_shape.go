package sqlcheck

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/0xJean/tabula"
)

// A LIMIT 0 source supplies the real PostgreSQL tuple descriptor without
// evaluating target rows or expressions. The outer join yields one NULL row
// whose field types we can inspect, including domains and extension types.
func checkReadResult(ctx context.Context, tx *sql.Tx, query string, params int, fields []tabula.ShapeField) error {
	for _, field := range fields {
		if len(field.AllowedTypes) > 0 {
			return checkReadResultAllowed(ctx, tx, query, params, fields)
		}
	}
	names := make([]string, len(fields))
	checks := make([]string, len(fields))
	args := make([]any, params, params+len(fields))
	for i, field := range fields {
		names[i] = field.Name
		args = append(args, field.Type)
		checks[i] = "(pg_catalog.pg_typeof(r." + quoteIdent(field.Name) + ") = pg_catalog.to_regtype($" + strconv.Itoa(len(args)) + "))"
	}
	probe := "SELECT r.*, COALESCE(" + strings.Join(checks, " AND ") +
		", false) FROM (VALUES (1)) AS seed LEFT JOIN (SELECT * FROM (" + query + "\n) AS result LIMIT 0) AS r ON true"
	rows, err := tx.QueryContext(ctx, probe, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if len(cols) != len(fields)+1 || !slices.Equal(cols[:len(cols)-1], names) {
		return fmt.Errorf("prepared result fields differ from %v", names)
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return fmt.Errorf("missing result description")
	}
	var matches bool
	dest := make([]any, len(cols))
	for i := range fields {
		dest[i] = new(any)
	}
	dest[len(fields)] = &matches
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("prepared result types differ from declared types for %v", names)
	}
	return rows.Err()
}

func readResultColumnNames(ctx context.Context, tx *sql.Tx, query string, args []any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM ("+query+"\n) AS result LIMIT 0", args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return cols, nil
}

func checkReadResultAllowed(ctx context.Context, tx *sql.Tx, query string, params int, fields []tabula.ShapeField) error {
	args := make([]any, params)
	// First check exact positional column names without referencing column names in SQL.
	cols, err := readResultColumnNames(ctx, tx, query, args)
	if err != nil {
		return err
	}
	if len(cols) != len(fields) {
		return fmt.Errorf("prepared result has %d fields, expected %d", len(cols), len(fields))
	}
	for i, want := range fields {
		if cols[i] != want.Name {
			return fmt.Errorf("prepared result field %d is %q, expected %q", i+1, cols[i], want.Name)
		}
	}
	// Now read each column's PostgreSQL OID via pg_typeof on a single guaranteed NULL row.
	exprs := make([]string, len(fields))
	for i, field := range fields {
		exprs[i] = "pg_catalog.pg_typeof(r." + quoteIdent(field.Name) + ")::oid"
	}
	probe := "SELECT " + strings.Join(exprs, ", ") +
		" FROM (VALUES (1)) AS seed LEFT JOIN (SELECT * FROM (" + query + "\n) AS result LIMIT 0) AS r ON true"
	oids := make([]uint32, len(fields))
	dest := make([]any, len(fields))
	for i := range oids {
		dest[i] = &oids[i]
	}
	if err := tx.QueryRowContext(ctx, probe, args...).Scan(dest...); err != nil {
		return err
	}
	vtx := sqlVerificationTx{tx}
	for i, want := range fields {
		if len(want.AllowedTypes) > 0 {
			matches, actualName, err := checkAllowedTypeOID(ctx, vtx, oids[i], want.AllowedTypes)
			if err != nil {
				return err
			}
			if !matches {
				return fmt.Errorf("prepared result %q at position %d has PostgreSQL type %s, not compatible with %v", want.Name, i+1, actualName, want.AllowedTypes)
			}
			continue
		}
		var matches bool
		if err := vtx.Scan(ctx, `SELECT pg_catalog.to_regtype($1)::oid = $2::oid`, []any{want.Type, oids[i]}, &matches); err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("prepared result %q differs from %s", want.Name, want.Type)
		}
	}
	return nil
}
