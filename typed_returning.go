package tabula

import (
	"fmt"
	"slices"
)

// ReturningPlan owns a complete write and its ordered result decoder. It
// offers All and Iter without rewriting the mutation with a read-style LIMIT.
type ReturningPlan[T, S any] struct {
	owner   *descriptor[T, S]
	indexes []int
	family  *rowFamily
	batch   *writeBatch
}

// DefineReturning registers a write result. A read-only SELECT is rejected,
// even if supplied through the sealed ResultStatement interface.
func DefineReturning[T, S any](out *View[T, S], name string, query ResultStatement, observe ...StatementOption) *ReturningPlan[T, S] {
	p, err := TryDefineReturning(out, name, query, observe...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineReturning atomically registers all write-result forms.
func TryDefineReturning[T, S any](out *View[T, S], name string, query ResultStatement, observe ...StatementOption) (*ReturningPlan[T, S], error) {
	if out == nil || isNilBound(query) {
		return nil, fmt.Errorf("tabula: DefineReturning requires an output view and result statement")
	}
	return compileReturning(&out.descriptor, nil, name, query.resultNode(), observe)
}

func compileReturning[T, S any](owner *descriptor[T, S], indexes []int, name string, node statementNode,
	observe []StatementOption,
) (*ReturningPlan[T, S], error) {
	nodes := map[string]statementNode{"write": node}
	var batch *writeBatch
	unscoped := false
	spec := RowFilter{Observe: observe}
	if write, ok := node.(writeDeclaration); ok {
		var err error
		nodes, batch, err = writeForms(write)
		if err != nil {
			return nil, err
		}
		unscoped = write.unscoped
	}
	if read, ok := node.(ReadSelect); ok {
		if read.list != nil || read.forms != (ReadForms{}) {
			return nil, fmt.Errorf("tabula: write results cannot contain read execution families")
		}
		spec.AllowUnscoped = read.unscoped
	}
	family, err := compileWriteFamily(owner, indexes, name, nodes, unscoped, spec, true)
	if err != nil {
		return nil, err
	}
	return &ReturningPlan[T, S]{owner: owner, indexes: slices.Clone(indexes), family: family, batch: batch}, nil
}

// DefineTableReturning decodes selected target fields into the mapped table's
// row type. Unselected fields retain their Go zero values.
func DefineTableReturning[T, S any](source RowSource[T, S], name string, query WriteStatement,
	selection TableSelection, observe ...StatementOption,
) *ReturningPlan[T, S] {
	p, err := TryDefineTableReturning(source, name, query, selection, observe...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineTableReturning registers a partial mapped write result atomically.
func TryDefineTableReturning[T, S any](source RowSource[T, S], name string, query WriteStatement,
	selection TableSelection, observe ...StatementOption,
) (*ReturningPlan[T, S], error) {
	if source.owner == nil || isNilBound(query) {
		return nil, fmt.Errorf("tabula: mapped RETURNING requires a declared source and write")
	}
	n, ok := query.writeNode().(writeDeclaration)
	if !ok || n.target.rel != source.rel {
		return nil, fmt.Errorf("tabula: mapped RETURNING source must be the write target")
	}
	indexes, projections, err := source.tableProjection(selection)
	if err != nil {
		return nil, err
	}
	n.returning = projections
	return compileReturning(source.owner, indexes, name, n, observe)
}

func (n writeDeclaration) queryDeclaredFields() []queryResultField {
	return (ReadSelect{project: n.returning}).queryDeclaredFields()
}
