package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"maps"
	"slices"
	"strings"
)

func (n writeDeclaration) statementSources() ([]rowSource, error) {
	if n.err != nil {
		return nil, n.err
	}
	sources, err := commonTableSources(n.tables)
	if err != nil {
		return nil, err
	}
	sources = append(sources, n.target)
	if n.selection != nil {
		more, err := n.selection.statementSources()
		if err != nil {
			return nil, err
		}
		sources = append(sources, more...)
	}
	for _, match := range n.matches {
		more, err := Select().From(match.source).statementSources()
		if err != nil {
			return nil, err
		}
		sources = append(sources, more...)
	}
	more, err := queryExpressionSources(n.expressionParts()...)
	return append(sources, more...), err
}

func (n writeDeclaration) expressionParts() []expressionPart {
	var parts []expressionPart
	assignments := slices.Concat(n.set, n.conflict.set)
	for _, row := range n.rows {
		assignments = append(assignments, row...)
	}
	for _, a := range assignments {
		if _, def := a.part.(defaultAssignment); !def {
			parts = append(parts, a.part)
		}
	}
	for _, pred := range slices.Concat(n.where, n.conflict.where) {
		parts = append(parts, pred.part)
	}
	for _, match := range n.matches {
		parts = append(parts, match.on.part)
	}
	for _, projection := range n.returning {
		parts = append(parts, projection.part)
	}
	return parts
}

func (n writeDeclaration) renderStatement(ctx *statementContext) (statementResult, error) {
	if n.err != nil {
		return statementResult{}, n.err
	}
	if n.target.rel == nil || ctx == nil || ctx.compiler == nil {
		return statementResult{}, fmt.Errorf("tabula: a write target and compiler are required")
	}
	if ctx.position != statementRoot && ctx.position != statementCommonTable {
		return statementResult{}, fmt.Errorf("tabula: a modifying statement requires the top-level statement or WITH")
	}
	if ctx.position != statementRoot && (len(n.batch) != 0 || n.unscoped) {
		return statementResult{}, fmt.Errorf("tabula: nested writes cannot contain batch or unscoped execution families")
	}
	if err := n.target.rel.checkCommand(n.command); err != nil {
		return statementResult{}, err
	}
	if n.command != "INSERT" || n.conflict.mode == "UPDATE" {
		if err := n.checkVersion(); err != nil {
			return statementResult{}, err
		}
	}
	c := ctx.compiler
	shapeStart := len(c.queryShapes)
	block, err := newQueryBlock(ctx.block, n.target.rel.issuer)
	if err != nil {
		return statementResult{}, err
	}
	local := *ctx
	local.block = block
	prior := c.block
	c.block = block
	defer func() { c.block = prior }()
	prefix, effects, err := renderCommonTables(&local, n.tables)
	if err != nil {
		return statementResult{}, err
	}
	if err := block.addPhysical(n.target.rel); err != nil {
		return statementResult{}, err
	}
	block.physicalAliases[n.target.rel] = c.queryAlias()
	meta := tabula.StatementShape{Effects: tabula.EffectWrite, Access: []tabula.ShapeAccess{{Relation: n.target.rel.table.SQL(), Command: n.command}}}
	var query string
	if n.command == "INSERT" {
		query, err = n.renderInsert(&local, &meta)
	} else {
		query, err = n.renderMutation(&local, &meta)
	}
	if err != nil {
		return statementResult{}, err
	}
	var fields []queryResultField
	if len(n.returning) != 0 {
		if n.target.rel.mode == tabula.WriteInsertOnly {
			return statementResult{}, fmt.Errorf("%w: insert-only targets cannot RETURNING", tabula.ErrOperationDenied)
		}
		for _, projection := range n.returning {
			if err := checkRowExpr(projection.part, nil); err != nil {
				return statementResult{}, err
			}
		}
		var columns []string
		columns, fields, err = c.projectQueryFields(ctx.output, n.returning, ctx.indexes)
		if err != nil {
			return statementResult{}, err
		}
		meta.Result = queryShapeFields(fields)
		meta.ReturningRelations = []string{n.target.rel.table.SQL()}
		meta.ResultLineage = queryResultLineage(fields)
		query += " RETURNING " + strings.Join(columns, ", ")
	}
	meta = writeReadRequirements(c, meta)
	own := statementEffects{Bits: uint8(tabula.EffectWrite), Writes: []statementWrite{{
		ID: n.id, Target: n.target.rel.table, Command: n.command, Returning: len(n.returning) != 0,
	}}}
	if n.conflict.mode == "UPDATE" {
		own.Writes = append(own.Writes, statementWrite{ID: n.id, Target: n.target.rel.table,
			Command: "UPDATE", Returning: len(n.returning) != 0})
	}
	effects, err = combineStatementEffects(effects, own)
	if err != nil {
		return statementResult{}, err
	}
	// A SELECT inside VALUES, FROM or RETURNING may itself acquire row locks.
	// Preserve those effects in the immutable execution record, even when its
	// physical reads were already folded into the enclosing write's metadata.
	for _, shape := range c.queryShapes[shapeStart:] {
		effects.Bits |= uint8(shape.Effects)
	}
	effects.Bits |= uint8(meta.Effects)
	meta.Effects |= tabula.ShapeEffects(effects.Bits)
	keys := queryProjectionKeys(c, ReadSelect{from: writeKeySource{n.target}, project: n.returning}, fields)
	return statementResult{sql: prefix + query, fields: fields, keys: keys, shape: meta, effects: effects}, nil
}

// Write targets share the graph's alias allocator, so nested reads cannot
// capture them and generated FROM aliases cannot collide with declared names.
func (n writeDeclaration) targetSQL(c *typedCompiler) string {
	return n.target.rel.table.SQL() + " AS " + typedQuote(c.block.physicalAliases[n.target.rel])
}

// A completed mutation returns each target row at most once. A direct export
// of its full primary key can therefore use the ordinary catalog-backed key
// proof. This adapter is only used by that proof, never to render a read.
type writeKeySource struct{ source rowSource }

func (s writeKeySource) readSource() (rowSource, error) { return s.source, nil }

func (n writeDeclaration) checkVersion() error {
	v, src := n.version, n.target
	if (src.version == "") != (v == (RowVersion{})) || src.version != "" &&
		(!v.valid || v.field == nil || v.field.source != src.rel || v.field.name != src.version || v.input == nil) {
		return fmt.Errorf("%w: expected version input must match the descriptor's version field", tabula.ErrOperationDenied)
	}
	if v.field != nil && !nativeType(v.field.kind, "int2", "int4", "int8") {
		return fmt.Errorf("tabula: expected a PostgreSQL integer version field")
	}
	return nil
}

func (n writeDeclaration) renderAssignments(c *typedCompiler, assignments []RowAssignment, meta *tabula.StatementShape) (string, error) {
	if len(assignments) == 0 {
		return "", fmt.Errorf("tabula: UPDATE requires assignments")
	}
	var targets, parts, defaults []string
	for _, a := range assignments {
		f := a.field
		if !a.valid || f == nil || f.source != n.target.rel || n.target.protected[f.name] ||
			!n.target.rel.canUpdate(f.name) || slices.Contains(targets, f.name) {
			return "", fmt.Errorf("%w: invalid, protected or duplicate assignment", tabula.ErrOperationDenied)
		}
		if err := n.recordTarget(c, f); err != nil {
			return "", err
		}
		value := "DEFAULT"
		if _, def := a.part.(defaultAssignment); def {
			defaults = append(defaults, f.name)
		} else {
			if err := checkRowExpr(a.part, nil); err != nil {
				return "", err
			}
			var err error
			value, err = a.part.render(c)
			if err != nil {
				return "", err
			}
		}
		targets = append(targets, f.name)
		parts = append(parts, typedQuote(f.name)+" = "+value)
	}
	if n.version.field != nil {
		field, err := n.version.field.render(c)
		if err != nil {
			return "", err
		}
		targets = append(targets, n.target.version)
		c.requireNative(scalarOperatorFunction("+", n.version.field.kind, typeID(Int4())))
		parts = append(parts, typedQuote(n.target.version)+" = "+field+" OPERATOR(pg_catalog.+) 1")
	}
	slices.Sort(targets)
	meta.Privileges = append(meta.Privileges, tabula.DefinedPrivilege{Table: n.target.rel.table.SQL(), Privilege: "UPDATE", Columns: targets})
	if len(defaults) != 0 {
		meta.Defaults = append(meta.Defaults, tabula.DefaultRequirement{Relation: n.target.rel.table.SQL(), Columns: defaults})
	}
	return strings.Join(parts, ", "), nil
}

func (n writeDeclaration) mutationWhere(c *typedCompiler, predicates []Pred) ([]string, error) {
	terms, err := n.target.where(c, predicates)
	if err != nil {
		return nil, err
	}
	if n.version.field != nil {
		field, err := n.version.field.render(c)
		if err != nil {
			return nil, err
		}
		input, err := n.version.input.render(c)
		if err != nil {
			return nil, err
		}
		if err := requireReadComparison(c, n.version.field.kind, "eq"); err != nil {
			return nil, err
		}
		terms = append(terms, "("+field+" OPERATOR(pg_catalog.=) "+input+")")
	}
	return terms, nil
}

func (n writeDeclaration) recordTarget(c *typedCompiler, field *fieldRef) error {
	if field == nil || field.source != n.target.rel || field.kind.GoType == nil {
		return fmt.Errorf("tabula: write target requires a mapped column of its occurrence")
	}
	if err := (physicalColumn{Table: field.source.table, Name: field.name}).Record(c.reads, field.kind); err != nil {
		return err
	}
	c.hasSecret = c.hasSecret || field.secret
	return nil
}

// Only expressions and real read sources contribute SELECT, never an overwritten
// target or an effect-only common table from the shared physical source walk.
func writeReadRequirements(c *typedCompiler, meta tabula.StatementShape) tabula.StatementShape {
	for _, table := range slices.SortedFunc(maps.Keys(c.selectCols), func(a, b physicalName) int {
		return strings.Compare(a.SQL(), b.SQL())
	}) {
		columns := slices.Sorted(maps.Keys(c.selectCols[table]))
		if len(columns) == 0 && !c.queryReads[table] {
			continue
		}
		meta.Access = append(meta.Access, tabula.ShapeAccess{Relation: table.SQL(), Command: "SELECT"})
		meta.Effects |= tabula.EffectRead
		if len(columns) == 0 {
			meta.AnyColumnSelect = append(meta.AnyColumnSelect, table.SQL())
		} else {
			meta.Privileges = append(meta.Privileges, tabula.DefinedPrivilege{Table: table.SQL(), Privilege: "SELECT", Columns: columns})
		}
	}
	return meta
}
