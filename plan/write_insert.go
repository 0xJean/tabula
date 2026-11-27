package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Version applies an optimistic version premise to an ON CONFLICT update.
// The insertion's initializer remains an explicit assignment or database default.
func (q InsertStatement) Version(version RowVersion) InsertStatement {
	q.node.version = version
	return q
}

func (n writeDeclaration) renderInsert(ctx *statementContext, meta *tabula.StatementShape) (string, error) {
	c := ctx.compiler
	forms := 0
	if len(n.rows) != 0 {
		forms++
	}
	if n.defaults {
		forms++
	}
	if n.selection != nil {
		forms++
	}
	if forms != 1 || len(n.insert) != 0 && n.selection == nil {
		return "", fmt.Errorf("tabula: INSERT requires exactly one VALUES, DEFAULT VALUES or SELECT input")
	}
	var columns []*fieldRef
	if n.selection != nil {
		for _, col := range n.insert {
			columns = append(columns, col.ref)
		}
	} else if len(n.rows) != 0 {
		for _, a := range n.rows[0] {
			columns = append(columns, a.field)
		}
	}
	scoped := c.params.scoped && n.target.rel.scope == tabula.ByScopeColumn()
	names, err := n.insertColumns(c, columns, scoped)
	if err != nil {
		return "", err
	}
	if scoped {
		// The scope is an insertion value, not a target read predicate.
		c.sources[n.target.rel] = true
		c.scopeType = n.target.scopeType
		c.hasSecret = c.hasSecret || n.target.rel.scopeSecret
		if err := (physicalColumn{Table: n.target.rel.table, Name: n.target.rel.scopeColumn}).Record(c.reads, n.target.scopeType); err != nil {
			return "", err
		}
		names = append([]string{n.target.rel.scopeColumn}, names...)
	}
	query := "INSERT INTO " + n.targetSQL(c)
	if len(names) != 0 {
		quoted := make([]string, len(names))
		for i, name := range names {
			quoted[i] = typedQuote(name)
		}
		query += " (" + strings.Join(quoted, ", ") + ")"
	}
	meta.Privileges = append(meta.Privileges, tabula.DefinedPrivilege{Table: n.target.rel.table.SQL(), Privilege: "INSERT", Columns: slices.Clone(names)})
	var input string
	switch {
	case n.selection != nil:
		input, err = n.renderInsertSelect(ctx, columns, scoped)
	case n.defaults:
		input = "DEFAULT VALUES"
		if scoped {
			input = "VALUES (" + n.target.scopeType.Cast("$1") + ")"
		}
	default:
		input, err = n.renderInsertValues(c, columns, scoped)
	}
	if err != nil {
		return "", err
	}
	var defaults []string
	for _, col := range n.columns {
		if !slices.Contains(names, col.name) {
			defaults = append(defaults, col.name)
		}
	}
	for _, row := range n.rows {
		for _, a := range row {
			if _, def := a.part.(defaultAssignment); def && !slices.Contains(defaults, a.field.name) {
				defaults = append(defaults, a.field.name)
			}
		}
	}
	meta.Defaults = append(meta.Defaults, tabula.DefaultRequirement{Relation: n.target.rel.table.SQL(), Columns: defaults,
		IncludesOmitted: true, InsertColumns: slices.Clone(names)})
	conflict, err := n.renderConflict(c, meta)
	return query + " " + input + conflict, err
}

func (n writeDeclaration) insertColumns(c *typedCompiler, columns []*fieldRef, scoped bool) ([]string, error) {
	if !n.defaults && len(columns) == 0 {
		return nil, fmt.Errorf("tabula: insertion requires ordered target columns")
	}
	var names []string
	for _, field := range columns {
		if field == nil || field.source != n.target.rel || slices.Contains(names, field.name) {
			return nil, fmt.Errorf("tabula: insertion requires distinct columns of its target")
		}
		if scoped && field.name == n.target.rel.scopeColumn {
			return nil, fmt.Errorf("%w: scoped insertion supplies the session scope", tabula.ErrOperationDenied)
		}
		if err := n.recordTarget(c, field); err != nil {
			return nil, err
		}
		names = append(names, field.name)
	}
	if n.target.rel.scope == tabula.ByScopeColumn() && !scoped &&
		(!n.unscoped || !slices.Contains(names, n.target.rel.scopeColumn)) {
		return nil, fmt.Errorf("tabula: unscoped insertion requires UnscopedOnly and an explicit scope value")
	}
	return names, nil
}

func (n writeDeclaration) renderInsertValues(c *typedCompiler, columns []*fieldRef, scoped bool) (string, error) {
	rows := make([]string, len(n.rows))
	prior := c.inputAliases
	prefix := c.inputPrefix
	previousBlock := c.block
	block := *previousBlock
	block.physical, block.results = make(map[*occurrence]bool), make(map[*queryOccurrence]bool)
	c.block = &block
	defer func() {
		c.inputAliases, c.inputPrefix = prior, prefix
		c.block = previousBlock
	}()
	for i, row := range n.rows {
		if len(row) != len(columns) {
			return "", fmt.Errorf("tabula: VALUES rows require identical ordered targets")
		}
		if len(n.aliases) != 0 {
			c.inputAliases = n.aliases[i]
			c.inputPrefix = "@row." + strconv.Itoa(i+1) + "."
		}
		var parts []string
		if scoped {
			parts = append(parts, n.target.scopeType.Cast("$1"))
		}
		for j, a := range row {
			if !a.valid || a.field == nil || !sameExpression(a.field, columns[j]) {
				return "", fmt.Errorf("tabula: invalid value or mismatched insertion target")
			}
			value := "DEFAULT"
			if _, def := a.part.(defaultAssignment); def {
				if n.target.rel.scope == tabula.ByScopeColumn() && a.field.name == n.target.rel.scopeColumn {
					return "", fmt.Errorf("tabula: scope insertion requires an explicit value, not DEFAULT")
				}
			} else {
				if err := checkRowExpr(a.part, func(p expressionPart) error {
					traits, _ := expressionStructure(p)
					if _, field := p.(*fieldRef); field || traits.aggregate {
						return fmt.Errorf("tabula: VALUES cannot read the target row; use a typed subquery")
					}
					return nil
				}); err != nil {
					return "", err
				}
				if err := checkRowExpr(a.part, nil); err != nil {
					return "", err
				}
				var err error
				value, err = a.part.render(c)
				if err != nil {
					return "", err
				}
			}
			parts = append(parts, value)
		}
		rows[i] = "(" + strings.Join(parts, ", ") + ")"
	}
	return "VALUES " + strings.Join(rows, ", "), nil
}

func (n writeDeclaration) renderInsertSelect(ctx *statementContext, columns []*fieldRef, scoped bool) (string, error) {
	child := *ctx
	child.output, child.indexes, child.position = nil, nil, statementDerived
	// WITH bindings remain available, but the INSERT target is not a correlatable
	// enclosing input of its SELECT. Its own explicit read occurrence is legal.
	block := *ctx.block
	block.physical, block.results = make(map[*occurrence]bool), make(map[*queryOccurrence]bool)
	child.block = &block
	result, err := n.selection.renderStatement(&child)
	if err != nil {
		return "", err
	}
	if tabula.ShapeEffects(result.effects.Bits)&tabula.EffectWrite != 0 || len(result.fields) != len(columns) {
		return "", fmt.Errorf("tabula: INSERT SELECT requires a read with the target arity")
	}
	ctx.compiler.queryShapes = append(ctx.compiler.queryShapes, result.shape)
	for i, field := range result.fields {
		target := columns[i]
		if !sameQueryType(target.kind, field.value.kind) ||
			field.value.nullable && !target.nullable {
			return "", fmt.Errorf("tabula: INSERT SELECT output %d has incompatible type or nullability", i+1)
		}
	}
	if !scoped {
		return result.sql, nil
	}
	parts := []string{n.target.scopeType.Cast("$1")}
	for _, field := range result.fields {
		parts = append(parts, `"@insert".`+typedQuote(field.name))
	}
	return "SELECT " + strings.Join(parts, ", ") + " FROM (" + result.sql + `) AS "@insert"`, nil
}
