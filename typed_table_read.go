package tabula

import "fmt"

// DefineTableRead applies SELECT clauses to fields of a single mapped source.
// The selection supplies the result fields and their decoders. Pass a query
// starting with Select() and supply clauses such as Where and OrderBy.
func DefineTableRead[T, S any](src RowSource[T, S], name string, columns TableSelection, query ReadSelect, observe ...StatementOption) *ReadPlan[T, S] {
	p, err := TryDefineTableRead(src, name, columns, query, observe...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineTableRead registers the complete table-backed read or returns a
// declaration error. The source and selection already define FROM and SELECT.
func TryDefineTableRead[T, S any](src RowSource[T, S], name string, columns TableSelection, query ReadSelect, observe ...StatementOption) (*ReadPlan[T, S], error) {
	if src.owner == nil || src.rel == nil {
		return nil, fmt.Errorf("tabula: DefineTableRead requires a declared row source")
	}
	if query.from != nil || len(query.project) != 0 || len(query.joins) != 0 {
		return nil, fmt.Errorf("tabula: DefineTableRead supplies its own projection and single FROM occurrence")
	}
	indexes, projections, err := src.tableProjection(columns)
	if err != nil {
		return nil, err
	}
	query.from, query.project = src, projections
	return compileReadPlan(src.owner, indexes, name, query, observe)
}
