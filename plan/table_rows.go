package plan

import (
	"fmt"
	"slices"

	"github.com/0xJean/tabula"
)

// TableSelection is an ordered projection into a physical row struct.
// Construct it with SelectAll or SelectColumns.
type TableSelection struct {
	all     bool
	columns []RowColumn
}

// SelectAll projects every physical column in descriptor order.
func SelectAll() TableSelection { return TableSelection{all: true} }

// SelectColumns projects distinct columns of one occurrence in the supplied
// order. Each returned row has zero values for its unselected fields.
func SelectColumns(first RowColumn, rest ...RowColumn) TableSelection {
	return TableSelection{columns: slices.Concat([]RowColumn{first}, rest)}
}

// DefineTableRows registers a read owned by its physical table and decodes
// into that table's row type. Supply at most one ReadForms declaration.
// It panics on a declaration error.
func DefineTableRows[T, S any](src RowSource[T, S], name string, spec RowsSpec, columns TableSelection, forms ...ReadForms) *RowsPlan[T, S] {
	p, err := TryDefineTableRows(src, name, spec, columns, forms...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineTableRows registers every shape of a table-backed read together.
// An invalid source, selection or plan leaves the registry untouched.
func TryDefineTableRows[T, S any](src RowSource[T, S], name string, spec RowsSpec, columns TableSelection, forms ...ReadForms) (*RowsPlan[T, S], error) {
	if src.owner == nil || src.rel == nil {
		return nil, fmt.Errorf("plan.DefineTableRows: a declared row source is required")
	}
	var policy ReadForms
	switch len(forms) {
	case 0:
	case 1:
		policy = forms[0]
	default:
		return nil, fmt.Errorf("plan.DefineTableRows: supply at most one ReadForms declaration")
	}
	indexes, project, err := src.tableProjection(columns)
	if err != nil {
		return nil, err
	}
	return compileRowsPlan(src.owner, indexes, name, src, spec, policy, project)
}

func (src RowSource[T, S]) tableProjection(selection TableSelection) ([]int, []Projection, error) {
	columns := selection.columns
	if selection.all {
		columns = make([]RowColumn, len(src.owner.Columns))
		for i, col := range src.owner.Columns {
			kind := src.columnType(i)
			if kind.GoType == nil || !typedIdent(col.Name) {
				return nil, nil, fmt.Errorf("plan.DefineTableRows: column %q needs a catalog name and a declared PostgreSQL type", col.Name)
			}
			columns[i] = RowColumn{ref: columnTypedRef(col, src.rel, kind)}
		}
	}
	if len(columns) == 0 {
		return nil, nil, fmt.Errorf("plan.DefineTableRows: a table selection is required")
	}
	seen := make(map[string]bool, len(columns))
	indexes := make([]int, len(columns))
	project := make([]Projection, len(columns))
	for j, selected := range columns {
		f := selected.ref
		if f == nil || f.source != src.rel || seen[f.name] {
			return nil, nil, fmt.Errorf("plan.DefineTableRows: select distinct columns of the source occurrence")
		}
		seen[f.name] = true
		i := src.owner.ByName[f.name]
		if src.types != nil && !selection.all {
			mapped := src.types[i].Kind
			if mapped.GoType != nil {
				if !mapped.CompatibleColumn(f.kind) {
					return nil, nil, fmt.Errorf("plan.DefineTableRows: selected type conflicts with the layout for %q", f.name)
				}
				if f.kind.VarcharLength == 0 {
					f = columnTypedRef(src.owner.Columns[i], src.rel, mapped)
				}
			}
		}
		indexes[j] = i
		project[j] = Projection{target: src.owner.Entry, index: i, compatible: true, part: f, kind: f.kind, secret: f.secret,
			field: tabula.ShapeField{Name: f.name, Type: f.kind.Qualified(), Nullable: f.nullable}}
	}
	return indexes, project, nil
}

func (src RowSource[T, S]) columnType(i int) catalogType {
	if src.types != nil {
		return src.types[i].Kind
	}
	col := src.owner.Columns[i]
	switch {
	case src.rel.scope == tabula.ByScopeColumn() && col.Name == src.rel.scopeColumn:
		return src.scopeType
	case src.deleted != nil && col.Name == src.deleted.name:
		return src.deleted.kind
	default:
		return columnDefaultType(col, src.owner.DefaultTypes).Kind
	}
}
