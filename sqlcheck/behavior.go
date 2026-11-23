package sqlcheck

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/pgstmt"
)

// BehaviorStatement executes once per role and scope, with bound arguments.
type BehaviorStatement struct {
	SQL  string
	Args []any
}

// BehaviorCase expects exactly one outcome. Rows are PostgreSQL values scanned
// as text, with nil for SQL NULL. Unordered comparison preserves duplicates.
type BehaviorCase struct {
	Role, Scope, Statement string
	Rows                   [][]*string
	Ordered                bool              `json:",omitempty"`
	Affected               *int64            `json:",omitempty"`
	Denied                 tabula.ErrorClass `json:",omitempty"`
}

// RLSMatrix requires one expectation for every role × scope × statement cell.
// Scopes contain application-owned custom GUC values.
type RLSMatrix struct {
	Roles      []string
	Scopes     map[string]map[string]string
	Statements map[string]BehaviorStatement
	Cases      []BehaviorCase
}

func validateVars(vars map[string]string) error {
	custom := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)+$`)
	for k := range vars {
		if !custom.MatchString(k) {
			return fmt.Errorf("expected a custom GUC, got %q", k)
		}
	}
	return nil
}

func (m RLSMatrix) validate() error {
	if _, err := scopeArray(m.Roles); err != nil {
		return err
	}
	if len(m.Scopes) == 0 || len(m.Statements) == 0 {
		return fmt.Errorf("scope and statement sets must be nonempty")
	}
	for n, vars := range m.Scopes {
		if n == "" {
			return errors.New("empty scope name")
		}
		if err := validateVars(vars); err != nil {
			return err
		}
	}
	for n, s := range m.Statements {
		if _, ok := pgstmt.DML(s.SQL); !ok || n == "" {
			return fmt.Errorf("invalid behavior statement %q", n)
		}
	}
	seen := map[[3]string]bool{}
	for _, c := range m.Cases {
		key := [3]string{c.Role, c.Scope, c.Statement}
		_, scope := m.Scopes[c.Scope]
		_, stmt := m.Statements[c.Statement]
		if !slices.Contains(m.Roles, c.Role) || !scope || !stmt || seen[key] {
			return fmt.Errorf("unexpected or duplicate behavior cell %v", key)
		}
		seen[key] = true
		wants := 0
		if c.Rows != nil {
			wants++
		}
		if c.Affected != nil {
			wants++
		}
		if c.Denied != "" {
			wants++
			if c.Denied != tabula.ClassRLSDenied && c.Denied != tabula.ClassInsufficientPrivilege {
				return fmt.Errorf("unknown denial %q", c.Denied)
			}
		}
		if wants != 1 {
			return fmt.Errorf("%v requires exactly one expectation", key)
		}
	}
	if len(seen) != len(m.Roles)*len(m.Scopes)*len(m.Statements) {
		return errors.New("incomplete behavior matrix")
	}
	return nil
}

func rowStrings(rows [][]*string, ordered bool) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		values := make([]string, len(row))
		for j, v := range row {
			if v == nil {
				values[j] = "NULL"
			} else {
				values[j] = strconv.Quote(*v)
			}
		}
		out[i] = "[" + strings.Join(values, ", ") + "]"
	}
	if !ordered {
		slices.Sort(out)
	}
	return out
}

// RunRLSMatrix executes application SQL against a disposable fixture and rolls
// back each cell independently. Findings are sorted. Transaction setup and
// rollback errors are returned separately from assertion failures.
func RunRLSMatrix(ctx context.Context, db *sql.DB, m RLSMatrix) ([]string, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	var findings []string
	for _, c := range m.Cases {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		failure, checkErr := behaviorCell(ctx, tx, c, m.Scopes[c.Scope], m.Statements[c.Statement])
		if err := errors.Join(checkErr, tx.Rollback()); err != nil {
			return nil, err
		}
		if failure != "" {
			findings = append(findings, fmt.Sprintf("%s/%s/%s: %s", c.Role, c.Scope, c.Statement, failure))
		}
	}
	slices.Sort(findings)
	return findings, nil
}

func setIdentity(ctx context.Context, tx *sql.Tx, role string, vars map[string]string) error {
	if err := auditRole(ctx, tx, role); err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.set_config($1,$2,true)", k, vars[k]); err != nil {
			return err
		}
	}
	return nil
}

func behaviorCell(ctx context.Context, tx *sql.Tx, c BehaviorCase, vars map[string]string, s BehaviorStatement) (string, error) {
	if err := setIdentity(ctx, tx, c.Role, vars); err != nil {
		return "", err
	}
	var err error
	if c.Affected != nil {
		var r sql.Result
		if r, err = tx.ExecContext(ctx, s.SQL, s.Args...); err == nil {
			n, e := r.RowsAffected()
			if e != nil {
				return "", e
			}
			if n != *c.Affected {
				return fmt.Sprintf("affected %d rows, expected %d", n, *c.Affected), nil
			}
		}
	} else {
		var rows *sql.Rows
		if rows, err = tx.QueryContext(ctx, s.SQL, s.Args...); err == nil {
			var got [][]*string
			got, err = scanBehaviorRows(rows)
			if err == nil && c.Rows != nil {
				want, actual := rowStrings(c.Rows, c.Ordered), rowStrings(got, c.Ordered)
				if !slices.Equal(want, actual) {
					return fmt.Sprintf("rows %v, expected %v", actual, want), nil
				}
			}
		}
	}
	if err != nil {
		if class, _ := tabula.ClassifyError(ctx, err); c.Denied != "" && class == c.Denied {
			return "", nil
		}
		return err.Error(), ctx.Err()
	}
	if c.Denied != "" {
		return "expected " + string(c.Denied) + ", statement succeeded", nil
	}
	return "", nil
}

func scanBehaviorRows(rows *sql.Rows) ([][]*string, error) {
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var got [][]*string
	for rows.Next() {
		row := make([]*string, len(cols))
		dest := make([]any, len(cols))
		for i := range row {
			dest[i] = &row[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		got = append(got, row)
	}
	return got, rows.Err()
}
