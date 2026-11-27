package sqlcheck

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/pgstmt"
)

// Statement is application-owned SQL. Access supplies the complete relation and
// command list for complex statements when checking structural policy coverage.
// NoAccess marks a reviewed complex statement (such as a SECURITY DEFINER
// routine call) that touches no caller-context RLS relations. Coverage rejects
// an assertion that conflicts with inferred table access. The reviewer checks
// dependencies inside routines and other SQL outside the inference rules.
type Statement struct {
	Name     string
	SQL      string
	Access   []PolicyAccess `json:",omitempty"`
	NoAccess bool           `json:",omitempty"`
	// Shape carries per-statement types and readiness requirements exported by a typed plan.
	Shape *tabula.StatementShape `json:",omitempty"`
}

// RoleStatements maps each execution role to its own named probes.
type RoleStatements map[string][]Statement

// AddDefinitions appends every statement from defs (in deterministic sorted
// order, including C1 count_by_<col> statements) to role's workload.
func (r RoleStatements) AddDefinitions(role string, defs ...tabula.Definition) {
	sorted := slices.Clone(defs)
	slices.SortFunc(sorted, func(a, b tabula.Definition) int { return strings.Compare(a.Name, b.Name) })
	for _, d := range sorted {
		for _, k := range slices.Sorted(maps.Keys(d.Statements)) {
			r[role] = append(r[role], definitionStatement(d, k, d.Name+"/"+k))
		}
	}
}

// AddViews appends every registered statement from views (including all C3
// composed-query variants) to role's workload without manual listing.
func (r RoleStatements) AddViews(role string, views ...tabula.ViewDefinition) {
	r.AddDefinitions(role, ViewsToDefinitions(views)...)
}

// The lexer and PostgreSQL must agree on string boundaries. Check the
// effective role as PostgreSQL also accepts special role setting values.
func auditRole(ctx context.Context, tx *sql.Tx, role string) error {
	if role != "" {
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+quoteIdent(role)); err != nil {
			return err
		}
	}
	var effective, setting string
	if err := tx.QueryRowContext(ctx, "SELECT current_user, pg_catalog.set_config('standard_conforming_strings','on',true)").Scan(&effective, &setting); err != nil {
		return err
	}
	if role != "" && effective != role {
		return fmt.Errorf("requested role %q, PostgreSQL selected %q", role, effective)
	}
	return nil
}

func (r RoleStatements) validate() error {
	if len(r) == 0 {
		return fmt.Errorf("empty role workload")
	}
	for _, role := range slices.Sorted(maps.Keys(r)) {
		if strings.TrimSpace(role) == "" || len(r[role]) == 0 {
			return fmt.Errorf("role %q requires a nonempty workload", role)
		}
		names := map[string]bool{}
		for _, s := range r[role] {
			if strings.TrimSpace(s.Name) == "" || names[s.Name] {
				return fmt.Errorf("%s: missing or duplicate statement name %q", role, s.Name)
			}
			if _, ok := pgstmt.DML(s.SQL); !ok {
				return fmt.Errorf("%s/%s: expected one data statement", role, s.Name)
			}
			if s.NoAccess && len(s.Access) > 0 {
				return fmt.Errorf("%s/%s: cannot combine NoAccess with Access", role, s.Name)
			}
			names[s.Name] = true
		}
	}
	return nil
}

// VerifyRoleStatements prepares and explains each role's SQL in a rolled-back
// transaction. Findings identify the role and statement and are sorted.
func VerifyRoleStatements(ctx context.Context, db *sql.DB, work RoleStatements) ([]string, error) {
	return VerifyRoleStatementsUsing(ctx, sqlVerifier{db}, work)
}
