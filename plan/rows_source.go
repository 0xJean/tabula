package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/planrt"
)

// RowColumn identifies a typed descriptor field in a GROUP BY or source declaration.
type RowColumn struct {
	ref  *fieldRef
	expr scalarOperand
}

// Column retains the field's occurrence and type in a heterogeneous column list.
func (f Field[V]) Column() RowColumn {
	return RowColumn{ref: f.ref}
}

// RowSource ties a single occurrence to its descriptor's keys and live-row policy.
type RowSource[T, S any] struct {
	owner    *descriptor[T, S]
	relation Rel[T]
	types    []planrt.DefaultType
	rowSource
}

// Rel returns this source's occurrence with its declared scope and live-row
// policy. Repeated calls identify the same occurrence.
func (src RowSource[T, S]) Rel() Rel[T] { return src.relation }

type rowSource struct {
	rel       *occurrence
	keys      []string
	protected map[string]bool
	scopeType catalogType
	deleted   *fieldRef
	rowLivePolicy
	version string
}

type rowLivePolicy struct {
	liveOp    string
	liveValue Binding
}

func deleteRowLivePolicy(d planrt.DeleteInfo, kind catalogType) (rowLivePolicy, error) {
	switch d.Kind {
	case planrt.DeleteTimestamp:
		return rowLivePolicy{liveOp: "IS NULL"}, nil
	case planrt.DeleteBool:
		if !sameSQLType(kind, typeID(Bool())) {
			return rowLivePolicy{}, fmt.Errorf("tabula: boolean soft deletion requires a pg_catalog.bool column")
		}
		return rowLivePolicy{liveOp: "= FALSE"}, nil
	case planrt.DeleteStatus:
		op, value := "<>", d.DeletedVal
		if d.AliveVal.IsValid() {
			op, value = "=", d.AliveVal
		}
		b := bindCatalogValue(kind, &typedInput{name: "@live", kind: kind}, value.Interface())
		if b.err != nil {
			return rowLivePolicy{}, fmt.Errorf("tabula: encoding the declared soft-delete value: %w", b.err)
		}
		return rowLivePolicy{liveOp: op, liveValue: b}, nil
	default:
		return rowLivePolicy{}, nil
	}
}

// RowsFrom uses the same occurrence as FieldOf. Supply the catalog type of a
// scope or deleted field with field.Column() when its Go type needs an explicit
// mapping. ByRLS and Global need no scope field.
func RowsFrom[T, K, S any](table *tabula.Table[T, K, S], rel Rel[T], columns ...RowColumn) RowSource[T, S] {
	return newRowSource(planTable(table), rel, columns, nil)
}

func newRowSource[T, S any](tb *planrt.Table[T, S], rel Rel[T], columns []RowColumn, live *rowLivePolicy) RowSource[T, S] {
	src, err := tableRowSource(tb, rel.source, columns, live)
	if err != nil {
		panic(err)
	}
	rel.rows = func() (rowSource, error) { return src, nil }
	return RowSource[T, S]{owner: tb.Owner, relation: rel, rowSource: src}
}

func tableRowSource[T, S any](tb *planrt.Table[T, S], rel *occurrence, columns []RowColumn, live *rowLivePolicy) (rowSource, error) {
	if tb == nil || rel == nil || tb.Entry != rel.entry {
		return rowSource{}, fmt.Errorf("plan.RowsFrom: the occurrence must belong to the table")
	}
	src := rowSource{rel: rel, protected: make(map[string]bool)}
	declared := make(map[string]RowColumn)
	var deletedName string
	if tb.DeletedIdx >= 0 {
		deletedName = tb.Columns[tb.DeletedIdx].Name
	}
	for _, col := range columns {
		f := col.ref
		if f == nil || f.source != rel || declared[f.name].ref != nil ||
			f.name != deletedName && (rel.scope != tabula.ByScopeColumn() || f.name != rel.scopeColumn) {
			return rowSource{}, fmt.Errorf("plan.RowsFrom: expected a distinct scope or deleted field of this occurrence")
		}
		declared[f.name] = col
	}
	for _, col := range tb.Columns {
		src.protected[col.Name] = !col.Updatable || col.Role == planrt.RoleVersion
		switch col.Role {
		case planrt.RoleKey:
			src.keys = append(src.keys, col.Name)
		case planrt.RoleVersion:
			src.version = col.Name
		}
	}
	special := func(i int) (*fieldRef, error) {
		col := tb.Columns[i]
		if !typedIdent(col.Name) {
			return nil, fmt.Errorf("plan.RowsFrom: invalid scope or soft-delete column %q", col.Name)
		}
		if f := declared[col.Name].ref; f != nil {
			return f, nil
		}
		kind := columnDefaultType(col, tb.DefaultTypes).Kind
		if kind.GoType == nil {
			return nil, fmt.Errorf("plan.RowsFrom: supply the catalog type of %q with FieldOf(...).Column()", col.Name)
		}
		return columnTypedRef(col, rel, kind), nil
	}
	if rel.scope == tabula.ByScopeColumn() {
		scope, err := special(tb.ScopeIdx)
		if err != nil {
			return rowSource{}, err
		}
		src.scopeType = scope.kind
	}
	if tb.DeletedIdx >= 0 {
		deleted, err := special(tb.DeletedIdx)
		if err != nil {
			return rowSource{}, err
		}
		src.deleted = deleted
		if live != nil {
			src.rowLivePolicy = *live
		} else {
			src.rowLivePolicy, err = deleteRowLivePolicy(tb.Delete, deleted.kind)
			if err != nil {
				return rowSource{}, err
			}
		}
	}
	return src, nil
}

func (src rowSource) fromSQL() string {
	return src.rel.table.SQL() + " AS " + typedQuote(src.rel.alias)
}

func (src rowSource) where(c *typedCompiler, predicates []Pred) ([]string, error) {
	scope, err := c.scopePredicate(src.rel, src.scopeType)
	if err != nil {
		return nil, err
	}
	var terms []string
	if scope != "" {
		terms = append(terms, scope)
	}
	if src.deleted != nil {
		col, err := src.deleted.render(c)
		if err != nil {
			return nil, err
		}
		op := src.liveOp
		if op == "= FALSE" {
			op, err = c.comparisonOperator(src.deleted.kind, "=")
			op += " FALSE"
		} else {
			op, err = c.comparisonOperator(src.deleted.kind, op)
		}
		if err != nil {
			return nil, err
		}
		term := col + " " + op
		if src.liveValue.input != nil {
			value, err := src.liveValue.input.render(c)
			if err != nil {
				return nil, err
			}
			term += " " + value
		}
		terms = append(terms, "("+term+")")
	}
	for _, pred := range predicates {
		if err := checkRowExpr(pred.part, nil); err != nil {
			return nil, err
		}
		part, err := renderRowPredicate(c, pred)
		if err != nil {
			return nil, err
		}
		terms = append(terms, part)
	}
	return terms, nil
}

func whereSQL(terms []string) string {
	if len(terms) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(terms, " AND ")
}

// RowPredicate enables one named, independently optional WHERE term.
type RowPredicate struct {
	Name string
	When Pred
}

// RowFilter declares the common WHERE terms and the registration budget.
// Where is required for mutations. Column-scoped plans register scoped
// statements by default. AllowUnscoped adds a separate unscoped family.
type RowFilter struct {
	Where         Pred
	Optional      []RowPredicate
	AllowUnscoped bool
	Budget        int
	Observe       []tabula.StatementOption
}

func rowLabel(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			continue
		}
		return false
	}
	return true
}

type rowVariant struct{ scope, filters, form string }

type rowFamily struct {
	scope   tabula.RelationScope
	live    []Binding
	filters []string
	shapes  map[rowVariant]*compiledShape
	meta    stmtMeta
}

func compileRowFamily[T, S any](d *descriptor[T, S], name string, sources []rowSource, spec RowFilter, forms []string,
	render func(*typedCompiler, string, []Pred) (string, tabula.StatementShape, error),
) (*rowFamily, error) {
	return compileRowFamilyChoices(d, name, sources, spec, forms, nil, render)
}

func compileRowFamilyChoices[T, S any](d *descriptor[T, S], name string, sources []rowSource, spec RowFilter, forms []string,
	reachable func([]shapeAlternative) bool,
	render func(*typedCompiler, string, []Pred) (string, tabula.StatementShape, error),
) (*rowFamily, error) {
	return compileSourceFamily(d, name, sources, spec, forms, reachable, false, render)
}

func compileSourceFamily[T, S any](d *descriptor[T, S], name string, sources []rowSource, spec RowFilter, forms []string,
	reachable func([]shapeAlternative) bool, graph bool,
	render func(*typedCompiler, string, []Pred) (string, tabula.StatementShape, error),
) (*rowFamily, error) {
	if d == nil || d.Entry == nil || !graph && len(sources) == 0 || !rowLabel(name) {
		return nil, fmt.Errorf("tabula: a registered owner, matching source and named row plan are required")
	}
	family := &rowFamily{scope: tabula.Global(), shapes: make(map[rowVariant]*compiledShape), meta: ownerStatementMeta(d, "", spec.Observe)}
	occurrences := make([]*occurrence, len(sources))
	var scopeType catalogType
	for i, src := range sources {
		if src.rel == nil || src.rel.issuer != d.Issuer {
			return nil, fmt.Errorf("tabula: row plan source must belong to the owner's registry")
		}
		occurrences[i] = src.rel
		if src.rel.scope == tabula.ByScopeColumn() {
			if scopeType.GoType != nil && !sameSQLType(scopeType, src.scopeType) {
				return nil, fmt.Errorf("tabula: incompatible scope column types in plan %q", name)
			}
			if scopeType.GoType != nil && !d.HasScopeID && scopeType.Encoder != src.scopeType.Encoder {
				return nil, fmt.Errorf("tabula: joined scope columns must share an encoder or declare RegistryConfig.ScopeID in plan %q", name)
			}
			scopeType, family.scope = src.scopeType, tabula.ByScopeColumn()
		} else if src.rel.scope == tabula.ByRLS() && family.scope != tabula.ByScopeColumn() {
			family.scope = tabula.ByRLS()
		}
		if src.liveValue.input != nil {
			family.live = append(family.live, src.liveValue)
		}
	}
	scopes := []string{"global"}
	switch family.scope {
	case tabula.ByScopeColumn():
		scopes = []string{"scoped"}
		if spec.AllowUnscoped {
			scopes = append(scopes, "unscoped")
		}
	case tabula.ByRLS():
		scopes = []string{"rls"}
	}
	choices := []shapeChoice{{"scope", scopes}}
	for _, p := range spec.Optional {
		if !rowLabel(p.Name) || p.When.part == nil {
			return nil, fmt.Errorf("tabula: invalid optional predicate %q in plan %q", p.Name, name)
		}
		family.filters = append(family.filters, p.Name)
		choices = append(choices, shapeChoice{"filter." + p.Name, []string{"off", "on"}})
	}
	choices = append(choices, shapeChoice{"form", forms})
	paths, err := enumerateShapes(name, spec.Budget, choices, reachable)
	if err != nil {
		return nil, err
	}
	shapes := make([]*compiledShape, 0, len(paths))
	for _, path := range paths {
		scope, form := path[0].value, path[len(path)-1].value
		var enabled []string
		var predicates []Pred
		if spec.Where.part != nil {
			predicates = append(predicates, spec.Where)
		}
		for i, p := range spec.Optional {
			if path[i+1].value == "on" {
				enabled = append(enabled, p.Name)
				predicates = append(predicates, p.When)
			}
		}
		filters := strings.Join(enabled, "+")
		key := name + "." + scope + "." + form
		if filters != "" {
			key += ".where." + filters
		}
		var c *typedCompiler
		if graph {
			c, err = newQueryCompiler(scope == "scoped", d.Issuer, sources)
		} else {
			c, err = newTypedCompiler(scope == "scoped", occurrences...)
		}
		if err != nil {
			return nil, err
		}
		query, meta, err := render(c, form, predicates)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		c.hasSecret = c.hasSecret || family.meta.Secret
		shape, err := c.finishShape(key, query, scopeType, meta)
		if err != nil {
			return nil, err
		}
		if graph {
			shape.compiled.WriteEffects = statementEffects{Bits: uint8(meta.Effects)}
		}
		for _, live := range family.live {
			if pos, ok := shape.layout.positions[live.input]; ok {
				shape.shape.Inputs[pos].Secret = true
			}
		}
		shapes = append(shapes, shape)
		family.shapes[rowVariant{scope, filters, form}] = shape
	}
	if err := registerShapes(d, name, shapes); err != nil {
		return nil, err
	}
	return family, nil
}

func (f *rowFamily) variant(unscoped bool, filters []string, form string) (*compiledShape, error) {
	enabled := make([]string, 0, len(filters))
	for _, name := range f.filters {
		if slices.Contains(filters, name) {
			enabled = append(enabled, name)
		}
	}
	scope := "global"
	switch f.scope {
	case tabula.ByScopeColumn():
		scope = "scoped"
		if unscoped {
			scope = "unscoped"
		}
	case tabula.ByRLS():
		scope = "rls"
	}
	shape := f.shapes[rowVariant{scope, strings.Join(enabled, "+"), form}]
	if len(enabled) != len(filters) || shape == nil {
		return nil, fmt.Errorf("tabula: unregistered row plan choice")
	}
	return shape, nil
}
