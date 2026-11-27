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
