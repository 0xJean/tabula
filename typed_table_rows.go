package tabula

import (
	"fmt"
	"slices"
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
		return nil, fmt.Errorf("tabula.DefineTableRows: a declared row source is required")
	}
	var policy ReadForms
	switch len(forms) {
	case 0:
	case 1:
		policy = forms[0]
	default:
		return nil, fmt.Errorf("tabula.DefineTableRows: supply at most one ReadForms declaration")
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
		columns = make([]RowColumn, len(src.owner.cols))
		for i, col := range src.owner.cols {
			kind := src.columnType(i)
			if kind.goType == nil || !typedIdent(col.name) {
				return nil, nil, fmt.Errorf("tabula.DefineTableRows: column %q needs a catalog name and a declared PostgreSQL type", col.name)
			}
			columns[i] = RowColumn{ref: col.typedRef(src.rel, kind)}
		}
	}
	if len(columns) == 0 {
		return nil, nil, fmt.Errorf("tabula.DefineTableRows: a table selection is required")
	}
	seen := make(map[string]bool, len(columns))
	indexes := make([]int, len(columns))
	project := make([]Projection, len(columns))
	for j, selected := range columns {
		f := selected.ref
		if f == nil || f.source != src.rel || seen[f.name] {
			return nil, nil, fmt.Errorf("tabula.DefineTableRows: select distinct columns of the source occurrence")
		}
		seen[f.name] = true
		i := src.owner.byName[f.name]
		if src.types != nil && !selection.all {
			mapped := src.types[i].kind
			if mapped.goType != nil {
				if !mapped.compatibleColumn(f.kind) {
					return nil, nil, fmt.Errorf("tabula.DefineTableRows: selected type conflicts with the layout for %q", f.name)
				}
				if f.kind.varcharLength == 0 {
					f = src.owner.cols[i].typedRef(src.rel, mapped)
				}
			}
		}
		indexes[j] = i
		project[j] = Projection{target: src.owner.entry, index: i, compatible: true, part: f, kind: f.kind,
			field: ShapeField{Name: f.name, Type: f.kind.qualified(), Nullable: f.nullable}}
	}
	return indexes, project, nil
}

func (src RowSource[T, S]) columnType(i int) catalogType {
	if src.types != nil {
		return src.types[i].kind
	}
	col := src.owner.cols[i]
	switch {
	case src.rel.scope == ByScopeColumn() && col.name == src.rel.scopeColumn:
		return src.scopeType
	case src.deleted != nil && col.name == src.deleted.name:
		return src.deleted.kind
	default:
		return col.defaultType(src.owner.reg.defaultTypes).kind
	}
}
