// Package sqlcheck is the reusable verification toolkit for tabula
// descriptors: SQL snapshot rendering/diffing, Go-to-PostgreSQL schema
// alignment checks, column privilege gap checks, and real-PostgreSQL
// PREPARE/EXPLAIN verification.
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

// RenderSnapshot formats every statement across defs deterministically (sorted
// by table name, then statement key), prefixed by headerLines (each written as
// a "-- " comment line).
func RenderSnapshot(defs []tabula.Definition, headerLines ...string) string {
	sorted := slices.Clone(defs)
	slices.SortFunc(sorted, func(a, b tabula.Definition) int { return strings.Compare(a.Name, b.Name) })
	var b strings.Builder
	for _, line := range headerLines {
		b.WriteString("-- " + line + "\n")
	}
	for _, d := range sorted {
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

// ColumnInfo is one PostgreSQL column as read from information_schema.columns.
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
	})
}

// CheckSchemaColumns compares one descriptor Definition against the installed
// PostgreSQL columns for its table and returns human-readable mismatch lines.
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

// VerifySchema inspects information_schema.columns on db for every Definition
// in defs and returns all column mismatches.
func VerifySchema(ctx context.Context, db *sql.DB, defs []tabula.Definition, families map[string][]string) ([]string, error) {
	var failures []string
	for _, d := range defs {
		schema, table, ok := strings.Cut(d.Name, ".")
		if !ok {
			failures = append(failures, d.Name+": table name is not schema-qualified")
			continue
		}
		rows, err := db.QueryContext(ctx,
			`SELECT column_name, data_type, is_nullable = 'YES' FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2`,
			schema, table)
		if err != nil {
			return nil, fmt.Errorf("query schema for %s: %w", d.Name, err)
		}
		installed := map[string]ColumnInfo{}
		for rows.Next() {
			var name string
			var info ColumnInfo
			if err := rows.Scan(&name, &info.DataType, &info.Nullable); err != nil {
				_ = rows.Close()
				return nil, err
			}
			installed[name] = info
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		failures = append(failures, CheckSchemaColumns(d, installed, families)...)
	}
	return failures, nil
}

// PrivilegeNeeds aggregates column and sequence privileges required across a
// set of descriptor Definitions.
type PrivilegeNeeds struct {
	Columns   map[string]map[string]map[string]bool
	Sequences [][2]string
}

// CollectPrivilegeNeeds merges the Privileges declared on defs and also
// reports any definition names that declared zero privileges.
func CollectPrivilegeNeeds(defs []tabula.Definition) (PrivilegeNeeds, []string) {
	n := PrivilegeNeeds{Columns: map[string]map[string]map[string]bool{}}
	var empty []string
	for _, d := range defs {
		if len(d.Privileges) == 0 {
			empty = append(empty, d.Name)
			continue
		}
		for _, p := range d.Privileges {
			if p.Privilege == "USAGE" {
				n.Sequences = append(n.Sequences, [2]string{p.Table, p.Sequence})
				continue
			}
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
		rows, err := db.QueryContext(ctx,
			`SELECT privilege_type, column_name FROM information_schema.role_column_grants
			 WHERE grantee = $1 AND table_schema = $2 AND table_name = $3 AND privilege_type IN ('SELECT', 'INSERT', 'UPDATE')`,
			role, schema, name)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var privilege, column string
			if err := rows.Scan(&privilege, &column); err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			if !privileges[privilege][column] {
				extra = append(extra, fmt.Sprintf("%s %s(%s)", table, privilege, column))
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, nil, err
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
	slices.Sort(missing)
	slices.Sort(extra)
	return missing, extra, nil
}
