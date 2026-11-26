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

func checkShapeTypes(ctx context.Context, tx *sql.Tx, name, query string, params int, s *tabula.StatementShape) error {
	if len(s.Inputs) != params {
		return fmt.Errorf("prepared statement has %d inputs, expected %d", params, len(s.Inputs))
	}
	for i, in := range s.Inputs {
		var matches bool
		err := tx.QueryRowContext(ctx, `SELECT parameter_types[$2] IS NOT DISTINCT FROM pg_catalog.to_regtype($3)
FROM pg_catalog.pg_prepared_statements WHERE name = $1`, name, i+1, in.Type).Scan(&matches)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("prepared input %q at $%d differs from %s", in.Name, i+1, in.Type)
		}
	}
	for _, col := range s.PhysicalColumns {
		var matches bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM pg_catalog.pg_attribute
WHERE attrelid = pg_catalog.to_regclass($1) AND attname = $2 AND attnum > 0 AND NOT attisdropped
  AND atttypid = pg_catalog.to_regtype($3))`, col.Relation, col.Name, col.Type).Scan(&matches)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("physical column %s.%s differs from %s", col.Relation, col.Name, col.Type)
		}
	}
	if len(s.Result) == 0 {
		return nil
	}
	return checkReadResult(ctx, tx, query, params, s.Result)
}

// A LIMIT 0 source supplies the real PostgreSQL tuple descriptor without
// evaluating target rows or expressions. The outer join yields one NULL row
// whose field types we can inspect, including domains and extension types.
func checkReadResult(ctx context.Context, tx *sql.Tx, query string, params int, fields []tabula.ShapeField) error {
	names := make([]string, len(fields))
	checks := make([]string, len(fields))
	args := make([]any, params, params+len(fields))
	for i, field := range fields {
		names[i] = field.Name
		args = append(args, field.Type)
		checks[i] = "(pg_catalog.pg_typeof(r." + quoteIdent(field.Name) + ") = pg_catalog.to_regtype($" + strconv.Itoa(len(args)) + "))"
	}
	probe := "SELECT r.*, COALESCE(" + strings.Join(checks, " AND ") +
		", false) FROM (VALUES (1)) AS seed LEFT JOIN (SELECT * FROM (" + query + ") AS result LIMIT 0) AS r ON true"
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
