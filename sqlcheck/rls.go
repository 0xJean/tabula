package sqlcheck

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
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
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	type table struct {
		name string
		tableState
	}
	installed, err := readRows(ctx, tx, `SELECT c.relname,c.relrowsecurity,c.relforcerowsecurity FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','p')`, func(rows *sql.Rows) (table, error) {
		var t table
		err := rows.Scan(&t.name, &t.rlsEnabled, &t.rlsForced)
		return t, err
	}, schema)
	if err != nil {
		return nil, fmt.Errorf("list RLS tables in %s: %w", schema, err)
	}
	for _, t := range installed {
		tables[t.name] = t.tableState
	}
	schemas, err := scopeArray([]string{schema})
	if err != nil {
		return nil, err
	}
	policies, err := readPolicies(ctx, tx, schemas)
	if err != nil {
		return nil, err
	}
	hasRestrictive := map[string]bool{}
	for _, p := range policies {
		if !p.Permissive {
			hasRestrictive[p.Table] = true
		}
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
			for _, role := range p.Roles {
				if role == "PUBLIC" && forbidden["public"] {
					role = "public"
				}
				if forbidden[role] {
					failures = append(failures, fmt.Sprintf("%s.%s.%s: policy references forbidden role %s", schema, p.Table, p.Name, role))
				}
			}
		}
	}

	slices.Sort(failures)
	return failures, nil
}
