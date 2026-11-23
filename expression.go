package tabula

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Assignment is an atomic update constructed by Add or SetNow.
type Assignment struct {
	column string
	delta  any // nil for SetNow
}
type number interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~float32 | ~float64
}

// Add adds a bound numeric value to a column in PostgreSQL.
func Add[N number](column string, delta N) Assignment {
	return Assignment{column: column, delta: delta}
}

// SetNow assigns PostgreSQL's transaction timestamp, now(), to a column.
func SetNow(column string) Assignment { return Assignment{column: column} }

// SetExpr applies assignments to one live row by key within the session scope.
// Like Set, it checks the affected row count and does not perform version CAS.
// Key, scope, deleted, version and once columns cannot be expression targets.
func (r *Rows[T, K, S]) SetExpr(ctx context.Context, key K, assignments ...Assignment) error {
	if r.readOnly() {
		return ErrReadOnly
	}
	if len(assignments) == 0 {
		return fmt.Errorf("tabula.SetExpr %s: an assignment is required", r.t.name)
	}
	args, scoped := r.keyArgs(key)
	where := r.t.stmt.unscopedKeyWhere
	if scoped {
		where = r.t.stmt.keyWhere
	}
	sets := make([]string, 0, len(assignments))
	seen := make(map[string]bool, len(assignments))
	secret := make([]bool, len(args))
	for _, a := range assignments {
		idx, ok := r.t.byName[a.column]
		if !ok || seen[a.column] {
			return fmt.Errorf("tabula.SetExpr %s: unknown or duplicate column %q", r.t.name, a.column)
		}
		seen[a.column] = true
		c := r.t.cols[idx]
		if c.once || c.role != plain {
			return fmt.Errorf("tabula.SetExpr %s: column %s is protected", r.t.name, c.name)
		}
		ft := c.goType
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if a.delta == nil {
			if ft != timeType {
				return fmt.Errorf("tabula.SetExpr: SetNow requires a time.Time column, got %s", ft)
			}
			sets = append(sets, c.name+" = now()")
		} else {
			if !integer(ft) && ft.Kind() != reflect.Float32 && ft.Kind() != reflect.Float64 {
				return fmt.Errorf("tabula.SetExpr: Add requires a numeric column, got %s", ft)
			}
			args = append(args, a.delta)
			secret = append(secret, c.secret)
			sets = append(sets, c.name+" = "+c.name+" + $"+strconv.Itoa(len(args)))
		}
	}
	q := "UPDATE " + r.t.name + " SET " + strings.Join(sets, ", ") + where
	_, err := r.execWrite(ctx, "set_expr", stmtMeta{key: r.t.name + ".set_expr"}, q, args, secret, 1, 1, r.t.reg.rowCountErr())
	return err
}
