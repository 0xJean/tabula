// Package sqlcheck verifies PostgreSQL descriptors, handwritten SQL and role
// workloads. It checks schema alignment, snapshots database catalogs, compares
// direct grants, trials revocations, classifies policies and tests RLS behavior.
package sqlcheck

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
)

// ViewsToDefinitions converts ViewDefinitions into Definitions so they can be
// passed to RenderSnapshot, VerifyStatements, VerifySchema, or CollectPrivilegeNeeds.
func ViewsToDefinitions(views []tabula.ViewDefinition) []tabula.Definition {
	out := make([]tabula.Definition, len(views))
	for i, v := range views {
		out[i] = tabula.Definition{
			Name:             v.Name,
			Columns:          slices.Clone(v.Columns),
			Statements:       maps.Clone(v.Statements),
			Privileges:       slices.Clone(v.Privileges),
			StatementQueries: maps.Clone(v.StatementQueries),
		}
	}
	return out
}

// RenderSnapshot formats every statement across defs deterministically (sorted
// by table name, then statement key), prefixed by headerLines (each written as
// a "-- " comment line). A header records the owning query of each
// composed-query statement so role bindings survive a snapshot round trip.
func RenderSnapshot(defs []tabula.Definition, headerLines ...string) string {
	sorted := slices.Clone(defs)
	slices.SortFunc(sorted, func(a, b tabula.Definition) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	for _, line := range headerLines {
		b.WriteString("-- " + line + "\n")
	}
	for _, d := range sorted {
		if len(d.StatementQueries) > 0 {
			writeQueryHeader(&b, d)
		}
	}
	for _, d := range sorted {
		if len(d.Statements) == 0 {
			continue
		}
		b.WriteString("\n-- " + d.Name + "\n")
		keys := make([]string, 0, len(d.Statements))
		for key := range d.Statements {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			b.WriteString("-- " + key + "\n" + d.Statements[key] + ";\n")
		}
	}
	return b.String()
}

// RenderAllSnapshot renders both table Definitions and ViewDefinitions into one
// deterministic SQL snapshot.
func RenderAllSnapshot(tables []tabula.Definition, views []tabula.ViewDefinition, headerLines ...string) string {
	combined := make([]tabula.Definition, 0, len(tables)+len(views))
	combined = append(combined, tables...)
	combined = append(combined, ViewsToDefinitions(views)...)
	return RenderSnapshot(combined, headerLines...)
}

// FirstDifference returns up to 40 diff lines showing the first mismatches
// between want and got.
func FirstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	var out []string
	for i := 0; i < max(len(w), len(g)) && len(out) < 40; i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			out = append(out, "- "+a, "+ "+b)
		}
	}
	return strings.Join(out, "\n")
}

// ColumnInfo is one PostgreSQL column as read from information_schema.columns
// or pg_catalog.pg_attribute (for materialized views).
type ColumnInfo struct {
	DataType string
	Nullable bool
}

// DefaultTypeFamilies returns a fresh copy of the built-in Go-type to
// PostgreSQL information_schema data_type family map. Callers may extend the
// returned map with application-specific named types before calling
// CheckSchemaColumns or VerifySchema.
func DefaultTypeFamilies() map[string][]string {
	return maps.Clone(map[string][]string{
		"string":            {"character varying", "text", "character"},
		"*string":           {"character varying", "text", "character"},
		"int":               {"integer", "smallint", "bigint"},
		"int16":             {"smallint"},
		"int32":             {"integer", "smallint"},
		"int64":             {"integer", "smallint", "bigint"},
		"*int":              {"integer", "smallint", "bigint"},
		"*int64":            {"integer", "smallint", "bigint"},
		"bool":              {"boolean"},
		"*bool":             {"boolean"},
		"float64":           {"double precision", "real", "numeric"},
		"*float64":          {"double precision", "real", "numeric"},
		"time.Time":         {"timestamp with time zone", "timestamp without time zone"},
		"*time.Time":        {"timestamp with time zone", "timestamp without time zone"},
		"map[string]string": {"jsonb", "json"},
		"map[string]any":    {"jsonb", "json"},
		"json.RawMessage":   {"jsonb", "json"},
		"[]byte":            {"bytea"},
		"[]string":          {"ARRAY"},
		"[]int32":           {"ARRAY"},
		"[]float64":         {"ARRAY"},
		"[]bool":            {"ARRAY"},
		"netip.Addr":        {"inet"},
		"*netip.Addr":       {"inet"},
		"netip.Prefix":      {"cidr", "inet"},
		"*netip.Prefix":     {"cidr", "inet"},
		"[]int64":           {"ARRAY"},
	})
}

// CheckSchemaColumns compares one descriptor Definition against the installed
// PostgreSQL columns for its table or view and returns human-readable mismatch lines.
func CheckSchemaColumns(def tabula.Definition, installed map[string]ColumnInfo, families map[string][]string) []string {
	if len(installed) == 0 {
		return []string{def.Name + ": table not installed"}
	}
	var failures []string
	for _, c := range def.Columns {
		info, ok := installed[c.Name]
		family, known := families[c.GoType]
		switch {
		case !ok:
			failures = append(failures, fmt.Sprintf("%s.%s: column missing from schema", def.Name, c.Name))
		case !known:
			failures = append(failures, fmt.Sprintf("%s.%s: Go type %s has no registered PostgreSQL family (schema is %s)", def.Name, c.Name, c.GoType, info.DataType))
		case !slices.Contains(family, info.DataType):
			failures = append(failures, fmt.Sprintf("%s.%s: Go type %s expects %v, schema is %s", def.Name, c.Name, c.GoType, family, info.DataType))
		case info.Nullable && !c.Nullable:
			failures = append(failures, fmt.Sprintf("%s.%s: schema is nullable, Go type %s does not read NULL", def.Name, c.Name, c.GoType))
		}
	}
	return failures
}

// VerifySchema inspects PostgreSQL columns on db for every Definition in defs
// (supporting tables, partitioned tables, regular views, and materialized views)
// and returns all column mismatches.
func VerifySchema(ctx context.Context, db *sql.DB, defs []tabula.Definition, families map[string][]string) ([]string, error) {
	var failures []string
	for _, d := range defs {
		schema, table, ok := strings.Cut(d.Name, ".")
		if !ok {
			failures = append(failures, d.Name+": table name is not schema-qualified")
			continue
		}
		installed, err := loadInstalledColumns(ctx, db, schema, table)
		if err != nil {
			return nil, fmt.Errorf("query schema for %s: %w", d.Name, err)
		}
		failures = append(failures, CheckSchemaColumns(d, installed, families)...)
	}
	return failures, nil
}

// VerifyViewSchema verifies column alignment for views (including PostgreSQL
// regular views and materialized views) against db.
func VerifyViewSchema(ctx context.Context, db *sql.DB, views []tabula.ViewDefinition, families map[string][]string) ([]string, error) {
	return VerifySchema(ctx, db, ViewsToDefinitions(views), families)
}

func loadInstalledColumns(ctx context.Context, db *sql.DB, schema, relation string) (map[string]ColumnInfo, error) {
	type column struct {
		name string
		ColumnInfo
	}
	scan := func(r *sql.Rows) (column, error) {
		var c column
		err := r.Scan(&c.name, &c.DataType, &c.Nullable)
		return c, err
	}
	cols, err := readRows(ctx, db, `SELECT column_name, data_type, is_nullable = 'YES' FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2`, scan, schema, relation)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		// PostgreSQL omits materialized views from information_schema.columns.
		cols, err = readRows(ctx, db, `
		SELECT a.attname,
		       CASE
		           WHEN t.typelem <> 0 AND t.typlen = -1 THEN 'ARRAY'
		           ELSE format_type(a.atttypid, NULL)
		       END,
		       NOT a.attnotnull
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_type t ON t.oid = a.atttypid
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind = 'm'
		  AND a.attnum > 0 AND NOT a.attisdropped`, scan, schema, relation)
		if err != nil {
			return nil, err
		}
	}
	installed := make(map[string]ColumnInfo, len(cols))
	for _, c := range cols {
		installed[c.name] = c.ColumnInfo
	}
	return installed, nil
}

// PrivilegeNeeds aggregates column, table-DELETE, sequence, and function EXECUTE
// privileges required across a set of descriptor Definitions.
type PrivilegeNeeds struct {
	Columns     map[string]map[string]map[string]bool
	TableDelete map[string]bool
	Sequences   [][2]string
	Functions   []string
}

// CollectPrivilegeNeeds merges the Privileges declared on defs and also
// reports any definition names that declared zero privileges.
func CollectPrivilegeNeeds(defs []tabula.Definition) (PrivilegeNeeds, []string) {
	n := PrivilegeNeeds{
		Columns:     map[string]map[string]map[string]bool{},
		TableDelete: map[string]bool{},
	}
	var empty []string
	seenFunc := map[string]bool{}
	for _, d := range defs {
		if len(d.Privileges) == 0 {
			empty = append(empty, d.Name)
			continue
		}
		for _, p := range d.Privileges {
			switch p.Privilege {
			case "USAGE":
				n.Sequences = append(n.Sequences, [2]string{p.Table, p.Sequence})
			case "DELETE":
				n.TableDelete[p.Table] = true
			case "EXECUTE":
				if p.Function != "" && !seenFunc[p.Function] {
					seenFunc[p.Function] = true
					n.Functions = append(n.Functions, p.Function)
				}
			default:
				if n.Columns[p.Table] == nil {
					n.Columns[p.Table] = map[string]map[string]bool{}
				}
				if n.Columns[p.Table][p.Privilege] == nil {
					n.Columns[p.Table][p.Privilege] = map[string]bool{}
				}
				for _, col := range p.Columns {
					n.Columns[p.Table][p.Privilege][col] = true
				}
			}
		}
	}
	slices.Sort(n.Functions)
	return n, empty
}

// CheckRolePrivileges compares a PostgreSQL role's actual grants on db against
// needs, returning sorted missing and extra privilege descriptions.
func CheckRolePrivileges(ctx context.Context, db *sql.DB, needs PrivilegeNeeds, role string) (missing, extra []string, err error) {
	for table, privileges := range needs.Columns {
		for privilege, columns := range privileges {
			for column := range columns {
				var held bool
				if err := db.QueryRowContext(ctx,
					`SELECT has_column_privilege($1::name, $2::regclass, $3, $4)`,
					role, table, column, privilege).Scan(&held); err != nil {
					return nil, nil, fmt.Errorf("%s %s(%s): %w", table, privilege, column, err)
				}
				if !held {
					missing = append(missing, fmt.Sprintf("%s %s(%s)", table, privilege, column))
				}
			}
		}
		schema, name, _ := strings.Cut(table, ".")
		type columnGrant struct{ privilege, column string }
		grants, err := readRows(ctx, db, `SELECT privilege_type, column_name FROM information_schema.role_column_grants
			 WHERE grantee = $1 AND table_schema = $2 AND table_name = $3 AND privilege_type IN ('SELECT', 'INSERT', 'UPDATE')`, func(r *sql.Rows) (columnGrant, error) {
			var g columnGrant
			err := r.Scan(&g.privilege, &g.column)
			return g, err
		}, role, schema, name)
		if err != nil {
			return nil, nil, err
		}
		for _, g := range grants {
			if !privileges[g.privilege][g.column] {
				extra = append(extra, fmt.Sprintf("%s %s(%s)", table, g.privilege, g.column))
			}
		}
	}
	for table := range needs.TableDelete {
		var held bool
		if err := db.QueryRowContext(ctx,
			`SELECT has_table_privilege($1::name, $2::regclass, 'DELETE')`,
			role, table).Scan(&held); err != nil {
			return nil, nil, fmt.Errorf("%s DELETE: %w", table, err)
		}
		if !held {
			missing = append(missing, fmt.Sprintf("%s DELETE", table))
		}
	}
	for _, seq := range needs.Sequences {
		var held bool
		if err := db.QueryRowContext(ctx,
			`SELECT has_sequence_privilege($1::name, pg_get_serial_sequence($2, $3)::regclass, 'USAGE')`,
			role, seq[0], seq[1]).Scan(&held); err != nil {
			return nil, nil, fmt.Errorf("%s sequence(%s): %w", seq[0], seq[1], err)
		}
		if !held {
			missing = append(missing, fmt.Sprintf("%s USAGE(seq %s)", seq[0], seq[1]))
		}
	}
	for _, fn := range needs.Functions {
		var held bool
		if err := db.QueryRowContext(ctx,
			`SELECT has_function_privilege($1::name, $2::regprocedure, 'EXECUTE')`,
			role, fn).Scan(&held); err != nil {
			return nil, nil, fmt.Errorf("function %s EXECUTE: %w", fn, err)
		}
		if !held {
			missing = append(missing, fmt.Sprintf("EXECUTE(%s)", fn))
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)
	return missing, extra, nil
}
