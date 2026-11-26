package tabula

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// RowColumn identifies a typed descriptor field in a GROUP BY or source declaration.
type RowColumn struct {
	ref    *fieldRef
	encode func(any) any
}

// Column retains the field's occurrence and type in a heterogeneous column list.
func (f Field[V]) Column() RowColumn {
	col := RowColumn{ref: f.ref}
	if f.kind.encode != nil {
		col.encode = func(v any) any { return f.kind.encode(v.(V)) }
	}
	return col
}

// RowSource ties a single occurrence to its descriptor's keys and live-row policy.
type RowSource[T, S any] struct {
	owner *descriptor[T, S]
	rowSource
}

type rowSource struct {
	rel       *occurrence
	keys      []string
	protected map[string]bool
	scopeType catalogType
	deleted   *fieldRef
	liveOp    string
	liveValue Binding
	version   string
}

// RowsFrom uses the same occurrence as FieldOf. Supply the catalog type of a
// scope or deleted field with field.Column() when its Go type needs an explicit
// mapping. ByRLS and Global need no scope field.
func RowsFrom[T, K, S any](table *Table[T, K, S], rel Rel[T], columns ...RowColumn) RowSource[T, S] {
	if table == nil || rel.source == nil || table.entry != rel.source.entry {
		panic("tabula.RowsFrom: the occurrence must belong to the table")
	}
	src := rowSource{rel: rel.source, protected: make(map[string]bool)}
	declared := make(map[string]RowColumn)
	var deletedName string
	if table.deleted >= 0 {
		deletedName = table.cols[table.deleted].name
	}
	for _, col := range columns {
		f := col.ref
		if f == nil || f.source != rel.source || declared[f.name].ref != nil ||
			f.name != deletedName && (rel.source.scope != ByScopeColumn() || f.name != rel.source.scopeColumn) {
			panic("tabula.RowsFrom: expected a distinct scope or deleted field of this occurrence")
		}
		declared[f.name] = col
	}
	for _, col := range table.cols {
		src.protected[col.name] = !col.updatable() || col.role == versionRole
		switch col.role {
		case keyRole:
			src.keys = append(src.keys, col.name)
		case versionRole:
			src.version = col.name
		}
	}
	special := func(i int) *fieldRef {
		col := table.cols[i]
		if f := declared[col.name].ref; f != nil {
			return f
		}
		t := col.goType
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		// Only use built-in Go types with a conventional catalog mapping.
		var kind catalogType
		switch t {
		case reflect.TypeFor[int32]():
			kind = Int4().id
		case reflect.TypeFor[int64]():
			kind = Int8().id
		case reflect.TypeFor[string]():
			kind = Text().id
		case reflect.TypeFor[bool]():
			kind = Bool().id
		case reflect.TypeFor[time.Time]():
			kind = Timestamptz().id
		default:
			panic(fmt.Sprintf("tabula.RowsFrom: supply the catalog type of %q with FieldOf(...).Column()", col.name))
		}
		return &fieldRef{source: rel.source, name: col.name, kind: kind, nullable: col.nullable, secret: col.secret}
	}
	if rel.source.scope == ByScopeColumn() {
		src.scopeType = special(table.scope).kind
	}
	if table.deleted >= 0 {
		src.deleted = special(table.deleted)
		switch table.del.kind {
		case deleteTimestamp:
			src.liveOp = "IS NULL"
		case deleteBool:
			if !sameSQLType(src.deleted.kind, Bool().id) {
				panic("tabula.RowsFrom: boolean soft deletion requires a pg_catalog.bool column")
			}
			src.liveOp = "= FALSE"
		case deleteStatus:
			src.liveOp = "<>"
			value := table.del.deletedVal
			if table.del.aliveVal.IsValid() {
				value, src.liveOp = table.del.aliveVal, "="
			}
			bound := value.Interface()
			if encode := declared[src.deleted.name].encode; encode != nil {
				bound = encode(bound)
			}
			src.liveValue = Binding{&typedInput{"@live", src.deleted.kind}, bound}
		}
	}
	return RowSource[T, S]{&table.descriptor, src}
}

func (src rowSource) fromSQL() string {
	return src.rel.table.sql() + " AS " + typedQuote(src.rel.alias)
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
		term := col + " " + src.liveOp
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
	Observe       []StatementOption
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
	source  rowSource
	filters []string
	shapes  map[rowVariant]*compiledShape
	meta    stmtMeta
}

func compileRowFamily[T, S any](d *descriptor[T, S], name string, src rowSource, spec RowFilter, forms []string,
	render func(*typedCompiler, string, []Pred) (string, StatementShape, error),
) (*rowFamily, error) {
	if d == nil || d.entry == nil || src.rel == nil || src.rel.issuer != d.reg.statementIssuer || !rowLabel(name) {
		return nil, fmt.Errorf("tabula: a registered owner, matching source and named row plan are required")
	}
	scopes := []string{"global"}
	switch src.rel.scope {
	case ByScopeColumn():
		scopes = []string{"scoped"}
		if spec.AllowUnscoped {
			scopes = append(scopes, "unscoped")
		}
	case ByRLS():
		scopes = []string{"rls"}
	}
	choices := []shapeChoice{{"scope", scopes}}
	family := &rowFamily{source: src, shapes: make(map[rowVariant]*compiledShape), meta: statementMeta("", spec.Observe)}
	for _, p := range spec.Optional {
		if !rowLabel(p.Name) || p.When.part == nil {
			return nil, fmt.Errorf("tabula: invalid optional predicate %q in plan %q", p.Name, name)
		}
		family.filters = append(family.filters, p.Name)
		choices = append(choices, shapeChoice{"filter." + p.Name, []string{"off", "on"}})
	}
	choices = append(choices, shapeChoice{"form", forms})
	paths, err := enumerateShapes(name, spec.Budget, choices, nil)
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
		c, err := newTypedCompiler(scope == "scoped", src.rel)
		if err != nil {
			return nil, err
		}
		query, meta, err := render(c, form, predicates)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		c.hasSecret = c.hasSecret || family.meta.secret
		shape, err := c.finishShape(key, query, src.scopeType, meta)
		if err != nil {
			return nil, err
		}
		shapes = append(shapes, shape)
		family.shapes[rowVariant{scope, filters, form}] = shape
	}
	if err := d.registerShapes(name, shapes); err != nil {
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
	switch f.source.rel.scope {
	case ByScopeColumn():
		scope = "scoped"
		if unscoped {
			scope = "unscoped"
		}
	case ByRLS():
		scope = "rls"
	}
	shape := f.shapes[rowVariant{scope, strings.Join(enabled, "+"), form}]
	if len(enabled) != len(filters) || shape == nil {
		return nil, fmt.Errorf("tabula: unregistered row plan choice")
	}
	return shape, nil
}
