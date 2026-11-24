package sqlcheck

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// LoginRoleExpectation configures PostgreSQL login-role hygiene verification
// (A8). A compliant application login role has LOGIN NOINHERIT, no privileged
// attributes, exact expected role memberships (the NOLOGIN roles it switches
// into via SET LOCAL ROLE) with per-grant INHERIT disabled on PostgreSQL 16+,
// no direct table or column grants, owns no database objects, and cannot CREATE
// in the selected schemas.
type LoginRoleExpectation struct {
	// Role is the PostgreSQL login role name to audit (required).
	Role string
	// MemberOf is the exact set of roles that Role is allowed to be a direct
	// member of.
	MemberOf []string
	// Schemas lists the schemas where Role must not have CREATE privilege.
	// Defaults to ["public"] when empty.
	Schemas []string
}

// CheckLoginRole audits exp.Role on db and returns sorted human-readable
// violation lines. An empty slice indicates a compliant login role.
func CheckLoginRole(ctx context.Context, db *sql.DB, exp LoginRoleExpectation) ([]string, error) {
	if strings.TrimSpace(exp.Role) == "" {
		return nil, errors.New("sqlcheck.CheckLoginRole: Role is required")
	}
	schemas := exp.Schemas
	if len(schemas) == 0 {
		schemas = []string{"public"}
	}
	if _, err := scopeArray(schemas); err != nil {
		return nil, fmt.Errorf("sqlcheck.CheckLoginRole schemas: %w", err)
	}
	expectedMembers := map[string]bool{}
	for _, m := range exp.MemberOf {
		if strings.TrimSpace(m) == "" {
			return nil, errors.New("sqlcheck.CheckLoginRole: empty role name in MemberOf")
		}
		expectedMembers[m] = true
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return nil, err
	}

	var (
		canLogin, inherit, super, bypassRLS, createRole, createDB, repl bool
	)
	err = tx.QueryRowContext(ctx, `
		SELECT rolcanlogin, rolinherit, rolsuper, rolbypassrls, rolcreaterole, rolcreatedb, rolreplication
		FROM pg_catalog.pg_roles
		WHERE rolname = $1`, exp.Role).Scan(
		&canLogin, &inherit, &super, &bypassRLS, &createRole, &createDB, &repl,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{fmt.Sprintf("role %s: not found", exp.Role)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read pg_roles for %s: %w", exp.Role, err)
	}

	var failures []string
	if !canLogin {
		failures = append(failures, fmt.Sprintf("role %s: missing LOGIN attribute", exp.Role))
	}
	if inherit {
		failures = append(failures, fmt.Sprintf("role %s: must be NOINHERIT", exp.Role))
	}
	if super {
		failures = append(failures, fmt.Sprintf("role %s: must not have SUPERUSER", exp.Role))
	}
	if bypassRLS {
		failures = append(failures, fmt.Sprintf("role %s: must not have BYPASSRLS", exp.Role))
	}
	if createRole {
		failures = append(failures, fmt.Sprintf("role %s: must not have CREATEROLE", exp.Role))
	}
	if createDB {
		failures = append(failures, fmt.Sprintf("role %s: must not have CREATEDB", exp.Role))
	}
	if repl {
		failures = append(failures, fmt.Sprintf("role %s: must not have REPLICATION", exp.Role))
	}

	// 1. Exact direct role memberships (and PG 16+ per-membership inherit_option).
	var hasInheritOption bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_catalog.pg_attribute
			WHERE attrelid = 'pg_catalog.pg_auth_members'::regclass
			  AND attname = 'inherit_option'
			  AND NOT attisdropped
		)`).Scan(&hasInheritOption); err != nil {
		return nil, fmt.Errorf("check pg_auth_members.inherit_option: %w", err)
	}
	type memberRow struct {
		name          string
		inheritOption bool
	}
	var members []memberRow
	if hasInheritOption {
		members, err = readRows(ctx, tx, `
			SELECT r.rolname, bool_or(m.inherit_option)
			FROM pg_catalog.pg_auth_members m
			JOIN pg_catalog.pg_roles r ON r.oid = m.roleid
			JOIN pg_catalog.pg_roles member ON member.oid = m.member
			WHERE member.rolname = $1
			GROUP BY r.rolname`,
			func(r *sql.Rows) (memberRow, error) {
				var m memberRow
				return m, r.Scan(&m.name, &m.inheritOption)
			}, exp.Role)
	} else {
		members, err = readRows(ctx, tx, `
			SELECT DISTINCT r.rolname
			FROM pg_catalog.pg_auth_members m
			JOIN pg_catalog.pg_roles r ON r.oid = m.roleid
			JOIN pg_catalog.pg_roles member ON member.oid = m.member
			WHERE member.rolname = $1`,
			func(r *sql.Rows) (memberRow, error) {
				var m memberRow
				return m, r.Scan(&m.name)
			}, exp.Role)
	}
	if err != nil {
		return nil, fmt.Errorf("read memberships for %s: %w", exp.Role, err)
	}
	actualMembers := map[string]bool{}
	for _, m := range members {
		actualMembers[m.name] = true
		if !expectedMembers[m.name] {
			failures = append(failures, fmt.Sprintf("role %s: unexpected membership in %s", exp.Role, m.name))
		}
		if m.inheritOption {
			failures = append(failures, fmt.Sprintf("role %s: membership in %s has INHERIT enabled", exp.Role, m.name))
		}
	}
	for m := range expectedMembers {
		if !actualMembers[m] {
			failures = append(failures, fmt.Sprintf("role %s: missing membership in %s", exp.Role, m))
		}
	}

	// 2. Direct table/view/materialized-view/foreign-table privileges.
	type relGrant struct{ schema, rel, priv string }
	relGrants, err := readRows(ctx, tx, `
		SELECT n.nspname, c.relname, a.privilege_type
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL pg_catalog.aclexplode(c.relacl) AS a
		JOIN pg_catalog.pg_roles g ON g.oid = a.grantee
		WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg_toast%'
		  AND g.rolname = $1`,
		func(r *sql.Rows) (relGrant, error) {
			var g relGrant
			return g, r.Scan(&g.schema, &g.rel, &g.priv)
		}, exp.Role)
	if err != nil {
		return nil, fmt.Errorf("read direct table privileges for %s: %w", exp.Role, err)
	}
	for _, g := range relGrants {
		failures = append(failures, fmt.Sprintf("role %s: holds direct %s privilege on %s.%s", exp.Role, g.priv, g.schema, g.rel))
	}

	// Direct column privileges.
	type colGrant struct{ schema, rel, col, priv string }
	colGrants, err := readRows(ctx, tx, `
		SELECT n.nspname, c.relname, att.attname, a.privilege_type
		FROM pg_catalog.pg_attribute att
		JOIN pg_catalog.pg_class c ON c.oid = att.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		CROSS JOIN LATERAL pg_catalog.aclexplode(att.attacl) AS a
		JOIN pg_catalog.pg_roles g ON g.oid = a.grantee
		WHERE att.attnum > 0 AND NOT att.attisdropped
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg_toast%'
		  AND g.rolname = $1`,
		func(r *sql.Rows) (colGrant, error) {
			var g colGrant
			return g, r.Scan(&g.schema, &g.rel, &g.col, &g.priv)
		}, exp.Role)
	if err != nil {
		return nil, fmt.Errorf("read direct column privileges for %s: %w", exp.Role, err)
	}
	for _, g := range colGrants {
		failures = append(failures, fmt.Sprintf("role %s: holds direct %s privilege on %s.%s(%s)", exp.Role, g.priv, g.schema, g.rel, g.col))
	}

	// 3. Owns no database objects (relations, schemas, routines, custom types, current database).
	type ownedObj struct{ kind, name string }
	owned, err := readRows(ctx, tx, `
		SELECT 'relation', n.nspname || '.' || c.relname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_roles r ON r.oid = c.relowner
		WHERE r.rolname = $1 AND n.nspname NOT LIKE 'pg_toast%'
		UNION ALL
		SELECT 'schema', n.nspname
		FROM pg_catalog.pg_namespace n
		JOIN pg_catalog.pg_roles r ON r.oid = n.nspowner
		WHERE r.rolname = $1
		UNION ALL
		SELECT 'routine', n.nspname || '.' || p.proname
		FROM pg_catalog.pg_proc p
		JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		JOIN pg_catalog.pg_roles r ON r.oid = p.proowner
		WHERE r.rolname = $1
		UNION ALL
		SELECT 'type', n.nspname || '.' || t.typname
		FROM pg_catalog.pg_type t
		JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
		JOIN pg_catalog.pg_roles r ON r.oid = t.typowner
		WHERE r.rolname = $1
		  AND t.typrelid = 0
		  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_type e WHERE e.typarray = t.oid)
		UNION ALL
		SELECT 'database', d.datname
		FROM pg_catalog.pg_database d
		JOIN pg_catalog.pg_roles r ON r.oid = d.datdba
		WHERE r.rolname = $1 AND d.datname = current_database()`,
		func(r *sql.Rows) (ownedObj, error) {
			var o ownedObj
			return o, r.Scan(&o.kind, &o.name)
		}, exp.Role)
	if err != nil {
		return nil, fmt.Errorf("read owned objects for %s: %w", exp.Role, err)
	}
	for _, o := range owned {
		failures = append(failures, fmt.Sprintf("role %s: owns %s %s", exp.Role, o.kind, o.name))
	}

	// 4. Cannot CREATE in selected schemas.
	for _, schema := range schemas {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil {
			return nil, fmt.Errorf("check schema %s: %w", schema, err)
		}
		if !exists {
			failures = append(failures, fmt.Sprintf("schema %s: not found", schema))
			continue
		}
		var canCreate bool
		if err := tx.QueryRowContext(ctx,
			`SELECT pg_catalog.has_schema_privilege($1::name, $2::text, 'CREATE')`, exp.Role, schema).Scan(&canCreate); err != nil {
			return nil, fmt.Errorf("check CREATE privilege on %s for %s: %w", schema, exp.Role, err)
		}
		if canCreate {
			failures = append(failures, fmt.Sprintf("role %s: has CREATE privilege on schema %s", exp.Role, schema))
		}
	}

	slices.Sort(failures)
	return failures, nil
}
