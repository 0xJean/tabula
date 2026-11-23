package pgxdriver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/0xJean/tabula"
)

// CopyFrom bulk-inserts rows into tbl using PostgreSQL's binary COPY protocol
// via pgx.Tx.CopyFrom. It uses the table's derived INSERT column list and
// value codecs (JSON, Zero, Array, custom Scanner/Valuer) and skips serial key
// and timestamp soft-delete columns automatically, matching Rows.Insert.
//
// An empty rows slice returns 0, nil without touching the database.
func CopyFrom[T, K, S any](ctx context.Context, sess *Session[S], tbl *tabula.Table[T, K, S], rows []T) (int64, error) {
	if sess == nil || tbl == nil {
		return 0, errors.New("pgxdriver.CopyFrom: session and table are required")
	}
	if tbl.ReadOnly() || sess.ReadOnly() {
		return 0, tabula.ErrReadOnly
	}
	if len(rows) == 0 {
		return 0, nil
	}
	bound := tbl.Bind(sess)
	for i := range rows {
		if err := bound.CheckWrite(&rows[i]); err != nil {
			return 0, err
		}
	}
	cols := tbl.InsertColumns()
	if len(cols) == 0 {
		return 0, fmt.Errorf("pgxdriver.CopyFrom %s: table has no insertable columns", tbl.Name())
	}
	tableIdent := parseIdentifier(tbl.Name())
	if err := sess.check("COPY " + tbl.Name()); err != nil {
		return 0, err
	}
	src := &copySource[T, K, S]{
		rows:  rows,
		bound: bound,
		idx:   -1,
	}
	n, err := sess.tx.CopyFrom(ctx, tableIdent, cols, src)
	if err != nil {
		return n, err
	}
	return n, nil
}

func parseIdentifier(name string) pgx.Identifier {
	parts := strings.Split(name, ".")
	ident := make(pgx.Identifier, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			ident = append(ident, p)
		}
	}
	return ident
}

type copySource[T, K, S any] struct {
	rows  []T
	bound *tabula.Rows[T, K, S]
	idx   int
	err   error
}

func (c *copySource[T, K, S]) Next() bool {
	if c.err != nil {
		return false
	}
	c.idx++
	return c.idx < len(c.rows)
}

func (c *copySource[T, K, S]) Values() ([]any, error) {
	raw := c.bound.Args(c.rows[c.idx])
	vals := make([]any, len(raw))
	for i, a := range raw {
		if valuer, ok := a.(driver.Valuer); ok {
			v, err := valuer.Value()
			if err != nil {
				c.err = err
				return nil, err
			}
			vals[i] = v
			continue
		}
		vals[i] = a
	}
	return vals, nil
}

func (c *copySource[T, K, S]) Err() error { return c.err }
