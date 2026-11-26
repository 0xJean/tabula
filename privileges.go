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

func (t *Table[T, K, S]) derivePrivileges() []DefinedPrivilege {
	var out []DefinedPrivilege
	if t.mode.allows(selectOp) {
		names := make([]string, len(t.cols))
		for i, c := range t.cols {
			names[i] = c.name
		}
		out = append(out, DefinedPrivilege{Table: t.name, Privilege: "SELECT", Columns: names})
	}
	if t.readOnly {
		return out
	}
	if t.mode.allows(insertOp) {
		names := t.InsertColumns()
		if t.mode == WriteInsertOnly && len(names) == 0 {
			// The INSERT names the serial key with a literal DEFAULT.
			names = []string{t.cols[t.keys[0]].name}
		}
		out = append(out, DefinedPrivilege{Table: t.name, Privilege: "INSERT", Columns: names})
	}
	if t.mode.allows(updateOp) {
		names := make([]string, len(t.stmt.setColumns))
		for i, col := range t.stmt.setColumns {
			names[i] = t.cols[col].name
		}
		out = append(out, DefinedPrivilege{Table: t.name, Privilege: "UPDATE", Columns: names})
	}
	if t.allowDelete {
		out = append(out, DefinedPrivilege{Table: t.name, Privilege: "DELETE"})
	}
	if t.serial && t.mode.allows(insertOp) {
		out = append(out, DefinedPrivilege{Table: t.name, Privilege: "USAGE", Sequence: t.cols[t.keys[0]].name})
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
