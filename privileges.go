package tabula

import (
	"fmt"
	"slices"
)

// DefinedPrivilege is one privilege a defined table's statements need on
// table columns (SELECT, INSERT, UPDATE) or USAGE on a serial key's sequence.
type DefinedPrivilege struct {
	Table, Privilege string
	Columns          []string
	Sequence         string
}

func derivePrivileges(name string, columns []DefinedColumn, s statements) []DefinedPrivilege {
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
		{Table: name, Privilege: "INSERT", Columns: pick(s.insertColumns)},
		{Table: name, Privilege: "UPDATE", Columns: pick(s.setColumns)},
	}
	for i, c := range columns {
		if c.Role == string(keyRole) && !slices.Contains(s.insertColumns, i) {
			out = append(out, DefinedPrivilege{Table: name, Privilege: "USAGE", Sequence: c.Name})
		}
	}
	return out
}

// Requires records a column privilege a hand-written statement of this table
// needs beyond the derived ones, on this or another table.
func (t *Table[T, K, S]) Requires(table, privilege string, columns ...string) *Table[T, K, S] {
	if table == "" || len(columns) == 0 || (privilege != "SELECT" && privilege != "INSERT" && privilege != "UPDATE") {
		panic(fmt.Sprintf("%s.Requires %s: %s %s %v is not a column privilege", t.prefix(), t.Name, table, privilege, columns))
	}
	if t.reg == nil || t.defID <= 0 {
		panic(t.prefix() + ".Requires " + t.Name + ": table is not defined")
	}
	t.reg.mu.Lock()
	defer t.reg.mu.Unlock()
	if t.defID > len(t.reg.defs) {
		panic(t.prefix() + ".Requires " + t.Name + ": table is not defined")
	}
	t.reg.defs[t.defID-1].Privileges = append(t.reg.defs[t.defID-1].Privileges, DefinedPrivilege{
		Table: table, Privilege: privilege, Columns: slices.Clone(columns),
	})
	return t
}
