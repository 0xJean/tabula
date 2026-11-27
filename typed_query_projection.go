package tabula

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
)

func (c *typedCompiler) projectQueryFields(owner *registration, projections []Projection, indexes []int) ([]string, []queryResultField, error) {
	if len(projections) == 0 {
		return nil, nil, fmt.Errorf("tabula: query requires an explicit projection")
	}
	if owner != nil {
		width := len(owner.def.Columns)
		if indexes != nil {
			width = len(indexes)
		}
		if width != len(projections) {
			return nil, nil, fmt.Errorf("tabula: expected %d projected outputs", width)
		}
	}
	selects := make([]string, len(projections))
	fields := (ReadSelect{project: projections}).queryDeclaredFields()
	names := make(map[string]bool)
	ids := make(map[*queryFieldIdentity]bool)
	for i, p := range projections {
		if !p.compatible || p.part == nil || !typedIdent(p.field.Name) || !queryResultType(p.kind) || names[p.field.Name] || p.id != nil && ids[p.id] {
			return nil, nil, fmt.Errorf("tabula: invalid or duplicate query projection %q", p.field.Name)
		}
		names[p.field.Name] = true
		ids[p.id] = true
		if owner != nil {
			index := i
			if indexes != nil {
				index = indexes[i]
			}
			if index < 0 || index >= len(owner.def.Columns) || p.target != owner || p.index != index {
				return nil, nil, fmt.Errorf("tabula: incompatible projection for output %q", p.field.Name)
			}
			c.hasSecret = c.hasSecret || owner.def.Columns[index].Secret
		} else if p.id == nil {
			return nil, nil, fmt.Errorf("tabula: intermediate output requires an exported identity")
		}
		nullable := queryNullable(p.part, p.field.Nullable, c.block)
		if owner != nil && nullable && !owner.def.Columns[p.index].Nullable {
			return nil, nil, fmt.Errorf("tabula: query requires nullable output %q", p.field.Name)
		}
		if err := checkQueryAggregateNulls(p.part, c.block); err != nil {
			return nil, nil, err
		}
		text, err := p.part.render(c)
		if err != nil {
			return nil, nil, err
		}
		selects[i] = text + " AS " + typedQuote(p.field.Name)
		fields[i].value.nullable = nullable
		origins, secret, stability := queryResolvedProvenance(c, p.part)
		fields[i].origins = origins
		fields[i].secret = fields[i].secret || secret
		fields[i].stability = stability
		fields[i].lineage = queryProjectionLineage(c, p.part)
		if ref, ok := p.part.(queryResultRef); ok {
			field, err := c.block.resolveResult(ref.source, ref.id, 0)
			if err != nil {
				return nil, nil, err
			}
			fields[i].stored = field.stored
		}
		c.hasSecret = c.hasSecret || fields[i].secret
	}
	return selects, fields, nil
}

func queryResolvedOrigins(c *typedCompiler, part expressionPart) []physicalColumn {
	origins, _, _ := queryResolvedProvenance(c, part)
	return origins
}

func queryResolvedProvenance(c *typedCompiler, part expressionPart) ([]physicalColumn, bool, expressionStability) {
	var origins []physicalColumn
	secret := false
	switch n := part.(type) {
	case queryResultRef:
		field, err := c.block.resolveResult(n.source, n.id, 0)
		if err == nil {
			return slices.Clone(field.origins), field.secret, field.stability
		}
		return nil, false, expressionVolatile
	case queryOuter:
		if c.block == nil || c.block.parent == nil {
			return nil, false, expressionVolatile
		}
		outer := *c
		outer.block = c.block.parent
		return queryResolvedProvenance(&outer, n.value.part)
	case *fieldRef:
		if n != nil && n.source != nil {
			origins = []physicalColumn{{n.source.table, n.name}}
			secret = n.secret
		}
	case *typedInput:
		secret = n != nil && n.secret
	}
	traits, ok := expressionStructure(part)
	if !ok {
		return origins, secret, expressionVolatile
	}
	stability := traits.stability
	for _, child := range traits.children {
		fields, childSecret, childStability := queryResolvedProvenance(c, child)
		origins = append(origins, fields...)
		secret = secret || childSecret
		stability = max(stability, childStability)
	}
	return origins, secret, stability
}

func queryResultLineage(fields []queryResultField) []ResultLineage {
	result := make([]ResultLineage, len(fields))
	for i, field := range fields {
		result[i] = field.lineage
	}
	return result
}

func queryProjectionLineage(c *typedCompiler, part expressionPart) ResultLineage {
	switch node := part.(type) {
	case *fieldRef:
		if node != nil && node.source != nil {
			return ResultLineage{Relation: node.source.table.sql(), Column: node.name}
		}
	case queryResultRef:
		field, err := c.block.resolveResult(node.source, node.id, 0)
		if err == nil && field.lineage.CastType == "" {
			return field.lineage
		}
	case scalarNode:
		if node.syntax == scalarCast {
			return ResultLineage{CastType: node.result.qualified()}
		}
	case queryOuter:
		if c.block != nil && c.block.parent != nil {
			outer := *c
			outer.block = c.block.parent
			return queryProjectionLineage(&outer, node.value.part)
		}
	}
	return ResultLineage{}
}
func queryNullable(part expressionPart, base bool, b *queryBlock) bool {
	if b == nil {
		return base
	}
	switch n := part.(type) {
	case queryResultRef:
		f, err := b.resolveResult(n.source, n.id, 0)
		return err != nil || f.value.nullable || b.outerResults[n.source]
	case queryOuter:
		return queryNullable(n.value.part, n.value.nullable, b.parent)
	case windowExpression:
		return queryWindowNullable(n, b)
	case scalarNode:
		n.args = slices.Clone(n.args)
		for i, a := range n.args {
			n.args[i].nullable = queryNullable(a.part, a.nullable, b)
		}
		return n.nullableAfterJoin(b.outerPhysical)
	case queryExpression:
		return n.mode != "exists" && n.mode != "array"
	case rowNull:
		return false
	case comparison:
		if n.op == "IS DISTINCT FROM" || n.op == "IS NOT DISTINCT FROM" {
			return false
		}
	case aggregateCall, rowAggregate:
		return base
	}
	if base || nullExtended(part, b.outerPhysical) {
		return true
	}
	traits, _ := expressionStructure(part)
	for _, child := range traits.children {
		if queryNullable(child, false, b) {
			return true
		}
	}
	return false
}
func (q ReadSelect) queryOuterPhysical() map[*occurrence]bool {
	outer := make(map[*occurrence]bool)
	for _, j := range q.joins {
		if strings.HasPrefix(j.kind, "LEFT JOIN") {
			if _, ok := j.source.(QuerySource); !ok && !isNilBound(j.source) {
				src, err := j.source.readSource()
				if err == nil {
					outer[src.rel] = true
				}
			}
		}
	}
	return outer
}
func (q ReadSelect) queryResultNull(part expressionPart) bool {
	b := &queryBlock{outerPhysical: q.queryOuterPhysical(), outerResults: make(map[*queryOccurrence]bool), results: make(map[*queryOccurrence]bool)}
	for _, src := range append([]ReadSource{q.from}, queryJoinSources(q.joins)...) {
		if r, ok := src.(QuerySource); ok {
			b.results[r.occurrence] = true
		}
	}
	for _, j := range q.joins {
		if strings.HasPrefix(j.kind, "LEFT JOIN") {
			if r, ok := j.source.(QuerySource); ok {
				b.outerResults[r.occurrence] = true
			}
		}
	}
	return queryNullable(part, false, b)
}
func checkQueryAggregateOwnership(part expressionPart) error {
	traits, ok := expressionStructure(part)
	if !ok {
		return fmt.Errorf("tabula: unsupported query expression")
	}
	if traits.aggregate {
		outer, local := false, false
		var walk func(expressionPart)
		walk = func(p expressionPart) {
			switch p.(type) {
			case queryOuter:
				outer = true
				return
			case *fieldRef, queryResultRef:
				local = true
			}
			t, _ := expressionStructure(p)
			for _, child := range t.children {
				walk(child)
			}
		}
		for _, child := range traits.children {
			walk(child)
		}
		if outer && !local {
			return fmt.Errorf("tabula: aggregate over outer-only fields must be exported by its owning query")
		}
	}
	for _, child := range traits.children {
		if err := checkQueryAggregateOwnership(child); err != nil {
			return err
		}
	}
	return nil
}

func mergeQueryShapes(c *typedCompiler, meta StatementShape) StatementShape {
	return mergeQueryShapeRange(c, meta, 0)
}
func mergeQueryShapeRange(c *typedCompiler, meta StatementShape, start int) StatementShape {
	meta = cloneShape(meta)
	for _, child := range c.queryShapes[start:] {
		child = cloneShape(child)
		meta.Effects |= child.Effects
		meta.Access = appendQueryMetadata(meta.Access, child.Access)
		meta.Privileges = appendQueryMetadata(meta.Privileges, child.Privileges)
		meta.AnyColumnSelect = appendQueryMetadata(meta.AnyColumnSelect, child.AnyColumnSelect)
		meta.AnyColumnUpdate = appendQueryMetadata(meta.AnyColumnUpdate, child.AnyColumnUpdate)
		meta.SchemaUsage = appendQueryMetadata(meta.SchemaUsage, child.SchemaUsage)
		meta.ForceRLS = appendQueryMetadata(meta.ForceRLS, child.ForceRLS)
		meta.Routines = appendQueryMetadata(meta.Routines, child.Routines)
		meta.UniqueMatches = appendQueryMetadata(meta.UniqueMatches, child.UniqueMatches)
		meta.Defaults = appendQueryMetadata(meta.Defaults, child.Defaults)
		meta.Conflicts = appendQueryMetadata(meta.Conflicts, child.Conflicts)
		meta.ReturningRelations = appendQueryMetadata(meta.ReturningRelations, child.ReturningRelations)
		meta.Keys = appendKeyRequirements(meta.Keys, child.Keys...)
		meta.Comparisons = appendComparisonRequirements(meta.Comparisons, child.Comparisons...)
	}
	return meta
}

// Metadata union is idempotent even when a nested common-table result carries
// prerequisites that are also referenced by a sibling declaration.
func appendQueryMetadata[T any](dst, src []T) []T {
	for _, value := range src {
		if !slices.ContainsFunc(dst, func(old T) bool { return reflect.DeepEqual(old, value) }) {
			dst = append(dst, value)
		}
	}
	return dst
}

func queryResultType(kind catalogType) bool {
	if kind.goType == nil || kind.schema == "pg_catalog" && kind.name == "record" {
		return false
	}
	return kind.element == nil || kind.element.element == nil && queryResultType(*kind.element)
}
