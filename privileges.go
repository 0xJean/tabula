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

// Requires records a table or column privilege a hand-written statement of this
// table needs beyond the derived ones, on this or another table. SELECT, INSERT,
// and UPDATE require column names; DELETE is table-level and takes no columns.
func (t *Table[T, K, S]) Requires(table, privilege string, columns ...string) *Table[T, K, S] {
	p, err := buildPrivilege(table, privilege, columns)
	if err != nil {
		panic(fmt.Sprintf("%s.Requires %s: %v", t.prefix(), t.Name, err))
	}
	if t.reg == nil || t.defID <= 0 {
		panic(t.prefix() + ".Requires " + t.Name + ": table is not defined")
	}
	t.reg.mu.Lock()
	defer t.reg.mu.Unlock()
	if t.defID > len(t.reg.defs) {
		panic(t.prefix() + ".Requires " + t.Name + ": table is not defined")
	}
	t.reg.defs[t.defID-1].Privileges = append(t.reg.defs[t.defID-1].Privileges, p)
	return t
}

// RequiresFunction records an EXECUTE privilege need on a PostgreSQL function
// signature (e.g. "public.apply_credit(uuid, numeric)") for sqlcheck.
func (t *Table[T, K, S]) RequiresFunction(signature string) *Table[T, K, S] {
	sig := strings.TrimSpace(signature)
	if sig == "" || !strings.Contains(sig, "(") || !strings.HasSuffix(sig, ")") {
		panic(fmt.Sprintf("%s.RequiresFunction %s: invalid function signature %q", t.prefix(), t.Name, signature))
	}
	if t.reg == nil || t.defID <= 0 {
		panic(t.prefix() + ".RequiresFunction " + t.Name + ": table is not defined")
	}
	t.reg.mu.Lock()
	defer t.reg.mu.Unlock()
	if t.defID > len(t.reg.defs) {
		panic(t.prefix() + ".RequiresFunction " + t.Name + ": table is not defined")
	}
	t.reg.defs[t.defID-1].Privileges = append(t.reg.defs[t.defID-1].Privileges, DefinedPrivilege{
		Privilege: "EXECUTE",
		Function:  sig,
	})
	return t
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
