package tabula

import (
	"fmt"
	"slices"
	"strings"
)

// DefinedPrivilege is one privilege a defined table or view needs:
//   - SELECT / INSERT / UPDATE on table columns
//   - DELETE on a table
//   - USAGE on a serial key's sequence
//   - EXECUTE on a PostgreSQL function signature (e.g. "public.fn(uuid, text)")
type DefinedPrivilege struct {
	Table, Privilege string
	Columns          []string
	Sequence         string
	Function         string
}

func derivePrivileges(name string, columns []DefinedColumn, s statements, readOnly, allowDelete bool) []DefinedPrivilege {
	pick := func(indexes []int) []string {
		out := make([]string, len(indexes))
		for i, index := range indexes {
			out[i] = columns[index].Name
		}
		return out
	}
	all := make([]string, len(columns))
	for i, c := range columns {
		all[i] = c.Name
	}
	out := []DefinedPrivilege{
		{Table: name, Privilege: "SELECT", Columns: all},
	}
	if readOnly {
		return out
	}
	out = append(out,
		DefinedPrivilege{Table: name, Privilege: "INSERT", Columns: pick(s.insertColumns)},
		DefinedPrivilege{Table: name, Privilege: "UPDATE", Columns: pick(s.setColumns)},
	)
	if allowDelete {
		out = append(out, DefinedPrivilege{Table: name, Privilege: "DELETE"})
	}
	for i, c := range columns {
		if c.Role == string(keyRole) && !slices.Contains(s.insertColumns, i) {
			out = append(out, DefinedPrivilege{Table: name, Privilege: "USAGE", Sequence: c.Name})
		}
	}
	return out
}

// Requires registers an additional privilege required by this descriptor.
func (t *Table[T, K, S]) Requires(table, privilege string, columns ...string) *Table[T, K, S] {
	t.require(buildPrivilege(table, privilege, columns))
	return t
}

// RequiresFunction registers an additional privilege required by this descriptor.
func (t *Table[T, K, S]) RequiresFunction(signature string) *Table[T, K, S] {
	t.require(functionPrivilege(signature))
	return t
}

// Requires registers an additional privilege required by this descriptor.
func (v *View[T, S]) Requires(table, privilege string, columns ...string) *View[T, S] {
	v.require(buildPrivilege(table, privilege, columns))
	return v
}

// RequiresFunction registers an additional privilege required by this descriptor.
func (v *View[T, S]) RequiresFunction(signature string) *View[T, S] {
	v.require(functionPrivilege(signature))
	return v
}

func functionPrivilege(signature string) (DefinedPrivilege, error) {
	sig := strings.TrimSpace(signature)
	if sig == "" || !strings.Contains(sig, "(") || !strings.HasSuffix(sig, ")") {
		return DefinedPrivilege{}, fmt.Errorf("invalid function signature %q", sig)
	}
	return DefinedPrivilege{Privilege: "EXECUTE", Function: sig}, nil
}

func buildPrivilege(table, privilege string, columns []string) (DefinedPrivilege, error) {
	switch privilege {
	case "SELECT", "INSERT", "UPDATE":
		if table == "" || len(columns) == 0 {
			return DefinedPrivilege{}, fmt.Errorf("%s %s %v is not a column privilege", table, privilege, columns)
		}
		return DefinedPrivilege{Table: table, Privilege: privilege, Columns: slices.Clone(columns)}, nil
	case "DELETE":
		if table == "" || len(columns) > 0 {
			return DefinedPrivilege{}, fmt.Errorf("%s DELETE %v is a table privilege and takes no columns", table, columns)
		}
		return DefinedPrivilege{Table: table, Privilege: "DELETE"}, nil
	default:
		return DefinedPrivilege{}, fmt.Errorf("%s %s %v is not a supported privilege", table, privilege, columns)
	}
}
