package sqlcheck

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// RLSExpectation configures domain-agnostic PostgreSQL Row-Level Security
// structural checks against pg_class and pg_policy.
type RLSExpectation struct {
	// Schema is the PostgreSQL schema to inspect (defaults to "public").
	Schema string
	// Tables lists unqualified table names in Schema that must have RLS enabled.
	// When empty, every table in Schema that already has RLS enabled is audited.
	Tables []string
	// RequireForce fails any checked table without FORCE ROW LEVEL SECURITY.
	RequireForce bool
	// RequireRestrictive fails any checked table that has no RESTRICTIVE policy.
	RequireRestrictive bool
	// ForbiddenPolicyRoles fails any policy in Schema whose polroles directly
	// names one of these roles (e.g. a login or grant-holder role).
	ForbiddenPolicyRoles []string
}

// CheckRLS audits PostgreSQL RLS configuration in exp.Schema against exp and
// returns sorted human-readable failure lines.
func CheckRLS(ctx context.Context, db *sql.DB, exp RLSExpectation) ([]string, error) {
	schema := exp.Schema
	if schema == "" {
		schema = "public"
	}

	type tableState struct {
		rlsEnabled bool
		rlsForced  bool
	}
	tables := map[string]tableState{}
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')`, schema)
	if err != nil {
		return nil, fmt.Errorf("sqlcheck.CheckRLS: list tables in %s: %w", schema, err)
	}
	for rows.Next() {
		var name string
		var st tableState
		if err := rows.Scan(&name, &st.rlsEnabled, &st.rlsForced); err != nil {
			_ = rows.Close()
			return nil, err
		}
		tables[name] = st
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	// Collect policies for tables in schema.
	type policyRow struct {
		table      string
		name       string
		permissive bool
		roles      []string
	}
	pRows, err := db.QueryContext(ctx, `
		SELECT c.relname, p.polname, p.polpermissive,
		       COALESCE(array_to_string(ARRAY(
		           SELECT CASE WHEN r = 0 THEN 'public' ELSE pg_get_userbyid(r)::text END
		           FROM unnest(p.polroles) r ORDER BY 1
		       ), ','), '')
		FROM pg_policy p
		JOIN pg_class c ON c.oid = p.polrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1`, schema)
	if err != nil {
		return nil, fmt.Errorf("sqlcheck.CheckRLS: list policies in %s: %w", schema, err)
	}
	hasRestrictive := map[string]bool{}
	var policies []policyRow
	for pRows.Next() {
		var pr policyRow
		var roleCSV string
		if err := pRows.Scan(&pr.table, &pr.name, &pr.permissive, &roleCSV); err != nil {
			_ = pRows.Close()
			return nil, err
		}
		if roleCSV != "" {
			pr.roles = strings.Split(roleCSV, ",")
		}
		if !pr.permissive {
			hasRestrictive[pr.table] = true
		}
		policies = append(policies, pr)
	}
	if err := pRows.Err(); err != nil {
		_ = pRows.Close()
		return nil, err
	}
	if err := pRows.Close(); err != nil {
		return nil, err
	}

	targetTables := exp.Tables
	if len(targetTables) == 0 {
		for name, st := range tables {
			if st.rlsEnabled {
				targetTables = append(targetTables, name)
			}
		}
	}

	var failures []string
	for _, tbl := range targetTables {
		st, ok := tables[tbl]
		if !ok {
			failures = append(failures, fmt.Sprintf("%s.%s: table not found", schema, tbl))
			continue
		}
		if !st.rlsEnabled {
			failures = append(failures, fmt.Sprintf("%s.%s: ROW LEVEL SECURITY is not enabled", schema, tbl))
			continue
		}
		if exp.RequireForce && !st.rlsForced {
			failures = append(failures, fmt.Sprintf("%s.%s: FORCE ROW LEVEL SECURITY is not enabled", schema, tbl))
		}
		if exp.RequireRestrictive && !hasRestrictive[tbl] {
			failures = append(failures, fmt.Sprintf("%s.%s: no RESTRICTIVE policy defined", schema, tbl))
		}
	}

	if len(exp.ForbiddenPolicyRoles) > 0 {
		forbidden := map[string]bool{}
		for _, r := range exp.ForbiddenPolicyRoles {
			forbidden[r] = true
		}
		for _, p := range policies {
			for _, role := range p.roles {
				if forbidden[role] {
					failures = append(failures, fmt.Sprintf("%s.%s.%s: policy references forbidden role %s", schema, p.table, p.name, role))
				}
			}
		}
	}

	slices.Sort(failures)
	return failures, nil
}
