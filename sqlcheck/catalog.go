package sqlcheck

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// CatalogScope names the complete set of schemas and direct grantees in an audit.
// PUBLIC includes privileges granted to everyone.
type CatalogScope struct{ Schemas, Roles []string }

// Relation identifies one schema-qualified PostgreSQL relation.
type Relation struct{ Schema, Table string }

// PolicyID identifies a policy within its table.
type PolicyID struct {
	Relation
	Name string
}

// Policy contains the server-deparsed conditions and explicit target roles.
type Policy struct {
	PolicyID
	Command      string
	Permissive   bool
	Roles        []string
	Using, Check string
}

// Definer records the owner, signature, body and settings of a SECURITY DEFINER routine.
type Definer struct{ Schema, Name, Arguments, Owner, Definition string }

// Catalog holds one repeatable-read snapshot. Policies, force flags and
// definers cover all objects in the selected schemas, regardless of grantee.
type Catalog struct {
	Policies []Policy
	ForceRLS []Relation
	Grants   []Grant
	Definers []Definer
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readRows[T any](ctx context.Context, db queryer, query string, scan func(*sql.Rows) (T, error), args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// scopeArray validates names before passing them to PostgreSQL as JSON strings.
func scopeArray(names []string) (string, error) {
	if len(names) == 0 {
		return "", fmt.Errorf("catalog scope must be explicit and nonempty")
	}
	seen := map[string]bool{}
	for _, s := range names {
		if s == "" || strings.ContainsRune(s, 0) || seen[s] {
			return "", fmt.Errorf("invalid or duplicate catalog name %q", s)
		}
		seen[s] = true
	}
	raw, err := json.Marshal(names)
	return string(raw), err
}
func (s CatalogScope) bind(ctx context.Context, tx *sql.Tx) (schemas, roles string, err error) {
	schemas, err = scopeArray(s.Schemas)
	if err != nil {
		return
	}
	roles, err = scopeArray(s.Roles)
	if err != nil {
		return
	}
	var missing string
	err = tx.QueryRowContext(ctx, `SELECT coalesce(min(name),'') FROM (
 SELECT name FROM pg_catalog.jsonb_array_elements_text($1) s(name) WHERE NOT EXISTS(SELECT FROM pg_catalog.pg_namespace WHERE nspname=name)
 UNION ALL SELECT name FROM pg_catalog.jsonb_array_elements_text($2) r(name) WHERE name<>'PUBLIC' AND NOT EXISTS(SELECT FROM pg_catalog.pg_roles WHERE rolname=name)) missing`, schemas, roles).Scan(&missing)
	if err == nil && missing != "" {
		err = fmt.Errorf("unknown catalog schema or role %q", missing)
	}
	return
}

const schemaFilter = ` IN (SELECT pg_catalog.jsonb_array_elements_text($1)) `
const routineArgs = `coalesce((SELECT pg_catalog.string_agg(pg_catalog.format('%I.%I',tn.nspname,t.typname), ', ' ORDER BY a.i)
 FROM pg_catalog.unnest(p.proargtypes) WITH ORDINALITY a(typ,i) JOIN pg_catalog.pg_type t ON t.oid=a.typ JOIN pg_catalog.pg_namespace tn ON tn.oid=t.typnamespace),'') COLLATE pg_catalog."C"`

// ReadCatalog reads the scoped PostgreSQL catalogs with a fixed deparser
// search path. Definers includes SECURITY DEFINER functions and procedures.
// Grants includes overloaded functions and explicit column grants.
func ReadCatalog(ctx context.Context, db *sql.DB, scope CatalogScope) (Catalog, error) {
	var c Catalog
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return c, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return c, err
	}
	schemas, roles, err := scope.bind(ctx, tx)
	if err != nil {
		return c, err
	}
	c.Policies, err = readPolicies(ctx, tx, schemas)
	if err != nil {
		return c, err
	}
	c.ForceRLS, err = readRows(ctx, tx, `SELECT n.nspname,t.relname FROM pg_catalog.pg_class t JOIN pg_catalog.pg_namespace n ON n.oid=t.relnamespace
 WHERE t.relforcerowsecurity AND n.nspname`+schemaFilter+`ORDER BY n.nspname COLLATE "C",t.relname COLLATE "C"`, func(r *sql.Rows) (Relation, error) { var v Relation; err := r.Scan(&v.Schema, &v.Table); return v, err }, schemas)
	if err != nil {
		return c, err
	}
	c.Grants, err = readGrants(ctx, tx, schemas, roles)
	if err != nil {
		return c, err
	}
	c.Definers, err = readRows(ctx, tx, `SELECT n.nspname,p.proname,`+routineArgs+` AS args,pg_catalog.pg_get_userbyid(p.proowner),pg_catalog.pg_get_functiondef(p.oid)
 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE p.prosecdef AND p.prokind IN ('f','p','w') AND n.nspname`+schemaFilter+`ORDER BY n.nspname COLLATE "C",p.proname COLLATE "C",3`, func(r *sql.Rows) (Definer, error) {
		var v Definer
		err := r.Scan(&v.Schema, &v.Name, &v.Arguments, &v.Owner, &v.Definition)
		return v, err
	}, schemas)
	return c, err
}

func readPolicies(ctx context.Context, tx *sql.Tx, schemas string) ([]Policy, error) {
	return readRows(ctx, tx, `SELECT n.nspname,t.relname,p.polname,
 CASE p.polcmd WHEN '*' THEN 'ALL' WHEN 'r' THEN 'SELECT' WHEN 'a' THEN 'INSERT' WHEN 'w' THEN 'UPDATE' WHEN 'd' THEN 'DELETE' END,
 p.polpermissive, array_to_json(ARRAY(SELECT CASE WHEN id=0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(id)::text END FROM pg_catalog.unnest(polroles) AS id)),
 coalesce(pg_catalog.pg_get_expr(polqual,polrelid),''),coalesce(pg_catalog.pg_get_expr(polwithcheck,polrelid),'')
 FROM pg_catalog.pg_policy p JOIN pg_catalog.pg_class t ON t.oid=p.polrelid JOIN pg_catalog.pg_namespace n ON n.oid=t.relnamespace
 WHERE n.nspname`+schemaFilter+`ORDER BY n.nspname COLLATE "C",t.relname COLLATE "C",p.polname COLLATE "C"`, func(r *sql.Rows) (Policy, error) {
		var p Policy
		var raw []byte
		err := r.Scan(&p.Schema, &p.Table, &p.Name, &p.Command, &p.Permissive, &raw, &p.Using, &p.Check)
		if err == nil {
			err = json.Unmarshal(raw, &p.Roles)
			slices.Sort(p.Roles)
		}
		return p, err
	}, schemas)
}

// Snapshots renders each catalog section as independently comparable JSON.
func (c Catalog) Snapshots() map[string]string {
	out := map[string]string{}
	for name, section := range map[string]any{"policies.json": c.Policies, "force-rls.json": c.ForceRLS, "grants.json": c.Grants, "security-definers.json": c.Definers} {
		data, _ := json.MarshalIndent(section, "", "  ")
		out[name] = string(data) + "\n"
	}
	return out
}
