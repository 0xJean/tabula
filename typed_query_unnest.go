package tabula

import (
	"fmt"
	"slices"
	"strings"
)

// UnnestInput is a sealed array-column or ordinality declaration.
type UnnestInput interface {
	unnestInput() (*unnestColumn, queryResultField)
}
type unnestColumn struct {
	array   scalarOperand
	ordinal bool
}

func (f Exported[V]) unnestInput() (*unnestColumn, queryResultField) {
	return f.unnest, queryResultField{id: f.id, name: f.name, value: f.value.scalarOperand()}
}

// UnnestColumn declares one array and its exact scalar element mapping.
// Outputs are nullable because parallel arrays pad shorter inputs with NULL.
func UnnestColumn[A, V any](name string, array Expr[A], element PGType[V]) Exported[V] {
	f := Export(name, Expr[V]{kind: element, nullable: true})
	f.unnest = &unnestColumn{array: array.scalarOperand()}
	return f
}

// UnnestOrdinality declares the non-null int8 position within one expansion.
// Ordinality does not prove uniqueness of values or persistent row identity.
func UnnestOrdinality(name string) Exported[int64] {
	f := Export(name, Expr[int64]{kind: Int8()})
	f.unnest = &unnestColumn{ordinal: true}
	return f
}

// Unnest expands parallel heterogeneous arrays using qualified native overloads.
// It retains SQL NULL padding, nullable elements and optional ordinality.
func Unnest(alias string, first UnnestInput, rest ...UnnestInput) QuerySource {
	n := unnestQuery{}
	for _, input := range slices.Concat([]UnnestInput{first}, rest) {
		if isNilBound(input) {
			n.err = fmt.Errorf("tabula: undeclared UNNEST input")
			continue
		}
		column, field := input.unnestInput()
		n.columns = append(n.columns, column)
		n.fields = append(n.fields, field)
	}
	return QuerySource{&queryOccurrence{alias: alias, node: n, fields: cloneQueryFields(n.fields)}}
}

type unnestQuery struct {
	columns []*unnestColumn
	fields  []queryResultField
	err     error
}

func (n unnestQuery) statementSources() ([]rowSource, error) {
	var parts []expressionPart
	for _, column := range n.columns {
		if column != nil && !column.ordinal {
			parts = append(parts, column.array.part)
		}
	}
	return queryExpressionSources(parts...)
}
func (n unnestQuery) renderStatement(ctx *statementContext) (statementResult, error) {
	if n.err != nil {
		return statementResult{}, n.err
	}
	var calls []string
	ordinal := -1
	fields := cloneQueryFields(n.fields)
	ordinalProof := queryValueProof{valid: true, exact: true}
	for i, column := range n.columns {
		if column == nil {
			return statementResult{}, fmt.Errorf("tabula: UNNEST accepts only array and ordinality declarations")
		}
		if column.ordinal {
			if ordinal >= 0 {
				return statementResult{}, fmt.Errorf("tabula: duplicate UNNEST ordinality")
			}
			ordinal = i
			continue
		}
		if column.array.kind.element == nil || !sameQueryType(*column.array.kind.element, fields[i].value.kind) {
			return statementResult{}, fmt.Errorf("tabula: UNNEST requires its exact scalar element type and codec")
		}
		if err := checkRowExpr(column.array.part, nil); err != nil {
			return statementResult{}, err
		}
		sql, err := column.array.part.render(ctx.compiler)
		if err != nil {
			return statementResult{}, err
		}
		calls = append(calls, "pg_catalog.unnest("+sql+")")
		fields[i].origins, fields[i].secret, fields[i].stability = queryResolvedProvenance(ctx.compiler, column.array.part)
		fields[i].proof = queryUnnestValueProof(ctx.compiler, column.array)
		ordinalProof = ordinalProof.combine(fields[i].proof)
	}
	if len(calls) == 0 {
		return statementResult{}, fmt.Errorf("tabula: UNNEST requires at least one array")
	}
	sql := "ROWS FROM (" + strings.Join(calls, ", ") + ")"
	if ordinal >= 0 {
		sql += " WITH ORDINALITY"
		fields[ordinal].proof = ordinalProof
		for _, field := range fields {
			fields[ordinal].stability = max(fields[ordinal].stability, field.stability)
			fields[ordinal].secret = fields[ordinal].secret || field.secret
		}
	}
	ctx.compiler.requireNative("pg_catalog.unnest(anyarray)")
	return statementResult{sql: sql, fields: fields, effects: statementEffects{bits: EffectRead}}, nil
}
