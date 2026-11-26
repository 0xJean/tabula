package tabula

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// SortDirection selects the order of a composed query. SortDefault uses the
// primary term's fixed direction when set, then the QueryOrderSpec's default.
type SortDirection uint8

const (
	SortDefault SortDirection = iota
	SortAsc
	SortDesc
)

func (d SortDirection) sql() (string, error) {
	switch d {
	case SortAsc:
		return "ASC", nil
	case SortDesc:
		return "DESC", nil
	default:
		return "", fmt.Errorf("invalid sort direction %d", d)
	}
}

// QueryFilter declares one named, optional WHERE fragment of a composed query.
// Fragment parameters are numbered locally starting at $1 (independent of
// Select/Where base parameters) and may be reused across multiple predicates
// (for example "(o.code ILIKE $1 OR c.name ILIKE $1)").
type QueryFilter struct {
	Name   string
	SQL    string
	Secret bool
}

type scopeMode uint8

const (
	scopeUnset scopeMode = iota
	scopeColumns
	scopeByRLS
	scopeGlobal
)

// QueryScope declares how a composed query applies row scope on a scoped
// Registry. On a scoped Registry (S != struct{}), leaving Scope unset fails
// closed at DefineQuery time; choose ScopeColumns, ScopeByRLS or ScopeGlobal.
type QueryScope struct {
	mode scopeMode
	cols []string
}

// ScopeColumns injects outer WHERE predicates (<col> = $n) using the bound
// session's scope value on scoped sessions, omitting them on validated
// unscoped sessions. All listed columns share one parameter slot. Applies to
// driving and inner-joined tables; outer-joined tables must correlate in ON,
// use a composite foreign key or rely on PostgreSQL RLS.
func ScopeColumns(cols ...string) QueryScope {
	return QueryScope{mode: scopeColumns, cols: slices.Clone(cols)}
}

// ScopeByRLS declares that row scope is enforced by PostgreSQL RLS policies
// (or CTE/base predicates) rather than an outer scope column predicate.
func ScopeByRLS() QueryScope { return QueryScope{mode: scopeByRLS} }

// ScopeGlobal declares a view over global lookup tables that have no row scope.
func ScopeGlobal() QueryScope { return QueryScope{mode: scopeGlobal} }

// QuerySortTerm pairs a parameter-free SQL ordering expression with its matching View column.
type QuerySortTerm struct {
	Expr      string
	Column    string
	Direction SortDirection
}

// QuerySort declares one whitelisted caller sort key on QueryOrderSpec.
// Expr is a static SQL expression and cannot contain parameter placeholders.
type QuerySort struct {
	Key       string
	Expr      string
	Column    string
	Direction SortDirection
}

// QueryOrderSpec declares the whitelisted sort choices and optional tie-breaker
// terms of a composed query.
type QueryOrderSpec struct {
	Default          string
	DefaultDirection SortDirection
	Sorts            []QuerySort
	TieBreaker       []QuerySortTerm
}

// QuerySpec declares a view-owned composed query using explicit Select, Where,
// optional Tail (GROUP BY / HAVING) and optional optimized CountBase segments.
//
// Base parameters ($1..$k) share one namespace across Select, Where, Tail and
// CountBase. Filter fragments use fragment-local $1..$m numbering rebased at
// compile time.
type QuerySpec struct {
	Select    string
	Where     string
	Tail      string
	CountBase string
	Scope     QueryScope
	Filters   []QueryFilter
	Order     QueryOrderSpec
	MaxLimit  int
	// MaxOffset is the largest permitted QueryPage.Offset, inclusive. Zero allows any nonnegative offset.
	MaxOffset int
	Keyset    bool
}

type compiledFragment struct {
	raw   string
	spans []pgstmt.Span
	arity int
	slots []int
}

type compiledFilter struct {
	name   string
	frag   compiledFragment
	secret bool
}

type compiledSortTerm struct {
	expr    string
	colName string
	colIdx  int // -1 when Column omitted (offset-only)
	dir     SortDirection
}

type compiledSort struct {
	key   string
	terms []compiledSortTerm // primary + deduplicated tie-breakers
}

// Query is a validated, compiled composed query owned by a View.
type Query[T, S any] struct {
	v            *View[T, S]
	key          string
	selectFrag   compiledFragment
	whereFrag    compiledFragment
	tailFrag     compiledFragment
	countFrag    compiledFragment
	hasWhere     bool
	hasTail      bool
	hasCountBase bool
	baseArity    int
	countSlots   []int // sorted 1-based base slots referenced by CountBase + Where
	countBaseSQL string
	countWhere   string
	scopeMode    scopeMode
	scopeCols    []string
	filters      []compiledFilter
	filterIndex  map[string]int
	defaultSort  string
	defaultDir   SortDirection
	sorts        []compiledSort
	sortIndex    map[string]int
	maxLimit     int
	maxOffset    int
	keyset       bool
	opts         []StatementOption
	meta         stmtMeta
}

// DefineQuery validates spec against v, registers C3 verification variants in
// v's ViewDefinition.Statements, and returns a compiled Query handle. Panics
// on declaration mistakes at package initialization.
func (v *View[T, S]) DefineQuery(key string, spec QuerySpec, opts ...StatementOption) *Query[T, S] {
	q, err := v.TryDefineQuery(key, spec, opts...)
	if err != nil {
		panic(err)
	}
	q.registerVariants()
	return q
}

// TryDefineQuery validates and compiles spec against v without registering C3
// variants in the registry.
func (v *View[T, S]) TryDefineQuery(key string, spec QuerySpec, opts ...StatementOption) (*Query[T, S], error) {
	if v == nil || v.reg == nil {
		return nil, errors.New("tabula.DefineQuery: view is not defined")
	}
	if v.reg.deny&DenyLegacySQL != 0 {
		return nil, ErrLegacySQL
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf(v.prefix()+".DefineQuery "+v.name+"."+key+": "+format, args...)
	}
	if strings.TrimSpace(key) == "" {
		return nil, errors.New(v.prefix() + ".DefineQuery " + v.name + ": key is required")
	}
	sel, err := compileSQLFragment(spec.Select, true)
	if err != nil {
		return nil, fail("Select: %w", err)
	}
	var whr compiledFragment
	hasWhere := strings.TrimSpace(spec.Where) != ""
	if hasWhere {
		whr, err = compileSQLFragment(spec.Where, true)
		if err != nil {
			return nil, fail("Where: %w", err)
		}
	}
	var tail compiledFragment
	hasTail := strings.TrimSpace(spec.Tail) != ""
	if hasTail {
		tail, err = compileSQLFragment(spec.Tail, true)
		if err != nil {
			return nil, fail("Tail: %w", err)
		}
	}
	var cnt compiledFragment
	hasCountBase := strings.TrimSpace(spec.CountBase) != ""
	if hasCountBase {
		if hasTail {
			return nil, fail("CountBase cannot combine with Tail")
		}
		cnt, err = compileSQLFragment(spec.CountBase, true)
		if err != nil {
			return nil, fail("CountBase: %w", err)
		}
	}
	if spec.Keyset && hasTail {
		return nil, fail("Keyset pagination cannot combine with Tail")
	}

	// Base slot validation across Select + Where + Tail.
	baseSeen := map[int]bool{}
	maxBase := 0
	for _, f := range []compiledFragment{sel, whr, tail} {
		for _, s := range f.slots {
			baseSeen[s] = true
			if s > maxBase {
				maxBase = s
			}
		}
	}
	for slot := 1; slot <= maxBase; slot++ {
		if !baseSeen[slot] {
			return nil, fail("base parameters must be contiguous $1..$%d (missing $%d)", maxBase, slot)
		}
	}
	for _, s := range cnt.slots {
		if s > maxBase {
			return nil, fail("CountBase references $%d beyond base arity %d", s, maxBase)
		}
	}

	// Remap sparse base slots used by CountBase + Where into dense $1..$m.
	var countSlots []int
	var countBaseSQL, countWhereSQL string
	if hasCountBase {
		cSeen := map[int]bool{}
		for _, s := range cnt.slots {
			cSeen[s] = true
		}
		for _, s := range whr.slots {
			cSeen[s] = true
		}
		for s := range cSeen {
			countSlots = append(countSlots, s)
		}
		slices.Sort(countSlots)
		remapIdx := make(map[int]int, len(countSlots))
		for i, orig := range countSlots {
			remapIdx[orig] = i + 1
		}
		remapFn := func(slot int) (int, error) {
			mapped, ok := remapIdx[slot]
			if !ok {
				return 0, fmt.Errorf("unexpected base slot $%d", slot)
			}
			return mapped, nil
		}
		countBaseSQL, err = pgstmt.RewriteParams(cnt.raw, cnt.spans, remapFn)
		if err != nil {
			return nil, fail("CountBase remap: %w", err)
		}
		if hasWhere {
			countWhereSQL, err = pgstmt.RewriteParams(whr.raw, whr.spans, remapFn)
			if err != nil {
				return nil, fail("CountBase Where remap: %w", err)
			}
		}
	} else {
		for s := 1; s <= maxBase; s++ {
			countSlots = append(countSlots, s)
		}
	}

	// Scope validation: three-state fail-closed on scoped registries.
	sMode := spec.Scope.mode
	var sCols []string
	if sMode == scopeUnset {
		if v.reg.scopeType != reflect.TypeFor[struct{}]() {
			return nil, fail("scoped registry requires explicit Scope (ScopeColumns, ScopeByRLS or ScopeGlobal)")
		}
		sMode = scopeGlobal
	}
	if sMode == scopeColumns {
		if len(spec.Scope.cols) == 0 {
			return nil, fail("ScopeColumns requires at least one column")
		}
		seenCol := map[string]bool{}
		for _, rawCol := range spec.Scope.cols {
			col := strings.TrimSpace(rawCol)
			if col == "" || !safeOrderExpr(col) || strings.ContainsAny(col, " ,") {
				return nil, fail("invalid scope column %q", rawCol)
			}
			if seenCol[col] {
				return nil, fail("duplicate scope column %q", col)
			}
			seenCol[col] = true
			sCols = append(sCols, col)
		}
	}

	// Filters validation (ordered, fragment-local $1..$m).
	filters := make([]compiledFilter, 0, len(spec.Filters))
	filterIndex := make(map[string]int, len(spec.Filters))
	for _, flt := range spec.Filters {
		name := strings.TrimSpace(flt.Name)
		if name == "" {
			return nil, fail("filter name is required")
		}
		if _, dup := filterIndex[name]; dup {
			return nil, fail("duplicate filter %q", name)
		}
		cf, err := compileSQLFragment(flt.SQL, true)
		if err != nil {
			return nil, fail("filter %q: %w", name, err)
		}
		maxSlot := 0
		seenSlot := map[int]bool{}
		for _, s := range cf.slots {
			seenSlot[s] = true
			if s > maxSlot {
				maxSlot = s
			}
		}
		for s := 1; s <= maxSlot; s++ {
			if !seenSlot[s] {
				return nil, fail("filter %q placeholders must be contiguous $1..$%d (missing $%d)", name, maxSlot, s)
			}
		}
		cf.arity = maxSlot
		filterIndex[name] = len(filters)
		filters = append(filters, compiledFilter{name: name, frag: cf, secret: flt.Secret})
	}

	// MaxLimit validation.
	if spec.MaxLimit <= 0 || spec.MaxLimit >= math.MaxInt {
		return nil, fail("MaxLimit must satisfy 0 < MaxLimit < math.MaxInt, got %d", spec.MaxLimit)
	}

	if spec.MaxOffset < 0 {
		return nil, fail("MaxOffset must be nonnegative, got %d", spec.MaxOffset)
	}

	// Order & Keyset validation.
	defaultDir := spec.Order.DefaultDirection
	if defaultDir == SortDefault {
		defaultDir = SortDesc
	}
	if _, err := defaultDir.sql(); err != nil {
		return nil, fail("Order.DefaultDirection: %w", err)
	}
	if len(spec.Order.Sorts) == 0 {
		return nil, fail("Order.Sorts requires at least one sort option")
	}
	if spec.Keyset && len(spec.Order.TieBreaker) == 0 {
		return nil, fail("Keyset pagination requires Order.TieBreaker")
	}

	tieTerms := make([]compiledSortTerm, 0, len(spec.Order.TieBreaker))
	for i, tb := range spec.Order.TieBreaker {
		term, err := v.compileSortTerm(tb.Expr, tb.Column, tb.Direction, spec.Keyset)
		if err != nil {
			return nil, fail("Order.TieBreaker[%d]: %w", i, err)
		}
		tieTerms = append(tieTerms, term)
	}

	sorts := make([]compiledSort, 0, len(spec.Order.Sorts))
	sortIndex := make(map[string]int, len(spec.Order.Sorts))
	for _, s := range spec.Order.Sorts {
		sKey := strings.TrimSpace(s.Key)
		if sKey == "" {
			return nil, fail("Order.Sorts entry requires a Key")
		}
		if _, dup := sortIndex[sKey]; dup {
			return nil, fail("duplicate sort key %q", sKey)
		}
		primary, err := v.compileSortTerm(s.Expr, s.Column, s.Direction, spec.Keyset)
		if err != nil {
			return nil, fail("Order.Sorts[%q]: %w", sKey, err)
		}
		terms := []compiledSortTerm{primary}
		for _, tb := range tieTerms {
			if (primary.colName != "" && primary.colName == tb.colName) || strings.EqualFold(primary.expr, tb.expr) {
				continue
			}
			terms = append(terms, tb)
		}
		if spec.Keyset {
			// Reject mixed directions in v0.3.1: every term must follow the
			// caller direction (SortDefault) or share one uniform fixed direction.
			var fixed SortDirection
			for _, term := range terms {
				if term.dir != SortDefault {
					if fixed == SortDefault {
						fixed = term.dir
					} else if fixed != term.dir {
						return nil, fail("Order.Sorts[%q]: mixed keyset directions are not supported", sKey)
					}
				}
			}
			if fixed != SortDefault {
				// If any term pins a fixed direction while another follows caller
				// direction, caller flipping direction would produce mixed directions.
				for _, term := range terms {
					if term.dir == SortDefault {
						return nil, fail("Order.Sorts[%q]: keyset tie-breaker direction must match primary (leave Direction as SortDefault to follow caller)", sKey)
					}
				}
			}
		}
		sortIndex[sKey] = len(sorts)
		sorts = append(sorts, compiledSort{key: sKey, terms: terms})
	}
	defSort := strings.TrimSpace(spec.Order.Default)
	if defSort == "" {
		return nil, fail("Order.Default is required")
	}
	if _, ok := sortIndex[defSort]; !ok {
		return nil, fail("Order.Default %q is not declared in Order.Sorts", defSort)
	}

	return &Query[T, S]{
		v:            v,
		key:          key,
		selectFrag:   sel,
		whereFrag:    whr,
		tailFrag:     tail,
		countFrag:    cnt,
		hasWhere:     hasWhere,
		hasTail:      hasTail,
		hasCountBase: hasCountBase,
		baseArity:    maxBase,
		countSlots:   countSlots,
		countBaseSQL: countBaseSQL,
		countWhere:   countWhereSQL,
		scopeMode:    sMode,
		scopeCols:    sCols,
		filters:      filters,
		filterIndex:  filterIndex,
		defaultSort:  defSort,
		defaultDir:   defaultDir,
		sorts:        sorts,
		sortIndex:    sortIndex,
		maxLimit:     spec.MaxLimit,
		maxOffset:    spec.MaxOffset,
		keyset:       spec.Keyset,
		opts:         slices.Clone(opts),
		meta:         statementMeta(v.name+"."+key, opts),
	}, nil
}

func (v *View[T, S]) compileSortTerm(rawExpr, rawCol string, dir SortDirection, keyset bool) (compiledSortTerm, error) {
	expr := strings.TrimSpace(rawExpr)
	if expr == "" {
		return compiledSortTerm{}, errors.New("sort Expr is required")
	}
	if dir != SortDefault {
		if _, err := dir.sql(); err != nil {
			return compiledSortTerm{}, err
		}
	}
	fragment, err := compileSQLFragment(expr, true)
	if err != nil {
		return compiledSortTerm{}, fmt.Errorf("invalid sort Expr %q: %w", expr, err)
	}
	if len(fragment.slots) > 0 {
		return compiledSortTerm{}, fmt.Errorf("sort Expr cannot contain parameter $%d", fragment.slots[0])
	}
	colName := strings.TrimSpace(rawCol)
	colIdx := -1
	if colName != "" {
		idx, ok := v.byName[colName]
		if !ok {
			return compiledSortTerm{}, fmt.Errorf("sort Column %q is not declared on view %s", colName, v.name)
		}
		if keyset && v.cols[idx].nullable {
			return compiledSortTerm{}, fmt.Errorf("keyset sort Column %q is nullable; project a non-null expression and bind that column", colName)
		}
		colIdx = idx
	} else if keyset {
		return compiledSortTerm{}, errors.New("keyset sort term requires matching View Column")
	}
	return compiledSortTerm{expr: expr, colName: colName, colIdx: colIdx, dir: dir}, nil
}

func compileSQLFragment(raw string, forbidSessionEscape bool) (compiledFragment, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return compiledFragment{}, errors.New("SQL fragment is empty")
	}
	spans, err := pgstmt.ScanFragment(trimmed)
	if err != nil {
		return compiledFragment{}, err
	}
	hasCode := false
	seen := map[int]bool{}
	var slots []int
	for _, sp := range spans {
		if sp.Kind != pgstmt.Comment {
			hasCode = true
		}
		if sp.Kind == pgstmt.Param && !seen[sp.ParamNum] {
			seen[sp.ParamNum] = true
			slots = append(slots, sp.ParamNum)
		}
	}
	if !hasCode {
		return compiledFragment{}, errors.New("SQL fragment contains only comments")
	}
	if forbidSessionEscape {
		if err := GuardNoSessionEscape(trimmed); err != nil {
			return compiledFragment{}, err
		}
	}
	slices.Sort(slots)
	return compiledFragment{raw: trimmed, spans: spans, slots: slots}, nil
}

func (q *Query[T, S]) buildPredicates(baseWhere string, startSlot int, scoped bool, enabled []int) (string, int, error) {
	var parts []string
	next := startSlot
	if baseWhere != "" {
		parts = append(parts, "("+baseWhere+")")
	}
	if scoped && q.scopeMode == scopeColumns {
		next++
		slotStr := "$" + strconv.Itoa(next)
		conds := make([]string, len(q.scopeCols))
		for i, col := range q.scopeCols {
			conds[i] = col + " = " + slotStr
		}
		parts = append(parts, "("+strings.Join(conds, " AND ")+")")
	}
	for _, idx := range enabled {
		f := q.filters[idx]
		offset := next
		rebased, err := pgstmt.RewriteParams(f.frag.raw, f.frag.spans, func(slot int) (int, error) {
			return offset + slot, nil
		})
		if err != nil {
			return "", 0, err
		}
		next += f.frag.arity
		parts = append(parts, "("+rebased+")")
	}
	if len(parts) == 0 {
		return "", next, nil
	}
	return "\nWHERE " + strings.Join(parts, "\n  AND "), next, nil
}

func (q *Query[T, S]) resolveOrder(sortKey string, dir SortDirection) (compiledSort, SortDirection, string, error) {
	if sortKey == "" {
		sortKey = q.defaultSort
	}
	idx, ok := q.sortIndex[sortKey]
	if !ok {
		return compiledSort{}, SortDefault, "", fmt.Errorf("%s.Query %s.%s: unknown sort key %q", q.v.prefix(), q.v.name, q.key, sortKey)
	}
	cs := q.sorts[idx]
	primaryDir := cs.terms[0].dir
	fallback := cmp.Or(dir, primaryDir, q.defaultDir)
	if _, err := fallback.sql(); err != nil {
		return compiledSort{}, SortDefault, "", fmt.Errorf("%s.Query %s.%s: %w", q.v.prefix(), q.v.name, q.key, err)
	}
	orderParts := make([]string, len(cs.terms))
	for i, term := range cs.terms {
		dirSQL, _ := cmp.Or(term.dir, fallback).sql()
		orderParts[i] = term.expr + " " + dirSQL
	}
	// Keyset terms have one uniform direction. Compare the cursor in the
	// primary term's actual order, including when the caller asked to reverse it.
	return cs, cmp.Or(primaryDir, fallback), "\nORDER BY " + strings.Join(orderParts, ", "), nil
}

func (q *Query[T, S]) buildSelectSQL(scoped bool, enabled []int, sortKey string, dir SortDirection, withOffset, withKeysetLimit, withAfter bool) (string, error) {
	whereSQL, next, err := q.buildPredicates(q.whereFrag.raw, q.baseArity, scoped, enabled)
	if err != nil {
		return "", err
	}
	cs, effDir, orderSQL, err := q.resolveOrder(sortKey, dir)
	if err != nil {
		return "", err
	}
	if withAfter {
		op := ">"
		if effDir == SortDesc {
			op = "<"
		}
		var pred string
		if len(cs.terms) == 1 {
			next++
			pred = cs.terms[0].expr + " " + op + " $" + strconv.Itoa(next)
		} else {
			exprs := make([]string, len(cs.terms))
			holders := make([]string, len(cs.terms))
			for i, term := range cs.terms {
				next++
				exprs[i] = term.expr
				holders[i] = "$" + strconv.Itoa(next)
			}
			pred = "(" + strings.Join(exprs, ", ") + ") " + op + " (" + strings.Join(holders, ", ") + ")"
		}
		if whereSQL == "" {
			whereSQL = "\nWHERE (" + pred + ")"
		} else {
			whereSQL += "\n  AND (" + pred + ")"
		}
	}
	var b strings.Builder
	b.WriteString(q.selectFrag.raw)
	b.WriteString(whereSQL)
	if q.hasTail {
		b.WriteByte('\n')
		b.WriteString(q.tailFrag.raw)
	}
	b.WriteString(orderSQL)
	if withOffset {
		b.WriteString(" LIMIT $" + strconv.Itoa(next+1) + " OFFSET $" + strconv.Itoa(next+2))
	} else if withKeysetLimit {
		b.WriteString(" LIMIT $" + strconv.Itoa(next+1))
	}
	return b.String(), nil
}

func (q *Query[T, S]) buildCountSQL(scoped bool, enabled []int) (string, error) {
	if q.hasCountBase {
		whereSQL, _, err := q.buildPredicates(q.countWhere, len(q.countSlots), scoped, enabled)
		if err != nil {
			return "", err
		}
		return q.countBaseSQL + whereSQL, nil
	}
	whereSQL, _, err := q.buildPredicates(q.whereFrag.raw, q.baseArity, scoped, enabled)
	if err != nil {
		return "", err
	}
	var inner strings.Builder
	inner.WriteString(q.selectFrag.raw)
	inner.WriteString(whereSQL)
	if q.hasTail {
		inner.WriteByte('\n')
		inner.WriteString(q.tailFrag.raw)
	}
	return "SELECT count(*) FROM (" + inner.String() + ") AS tabula_count", nil
}

// registerVariants records bounded C3 verification variants on the owning View
// in deterministic order.
func (q *Query[T, S]) registerVariants() {
	type scopeVariant struct {
		suffix string
		scoped bool
	}
	modes := []scopeVariant{{suffix: "", scoped: false}}
	if q.scopeMode == scopeColumns {
		modes = []scopeVariant{
			{suffix: ".scoped", scoped: true},
			{suffix: ".unscoped", scoped: false},
		}
	}
	allIdx := make([]int, len(q.filters))
	for i := range q.filters {
		allIdx[i] = i
	}
	must := func(sqlText string, err error) string {
		if err != nil {
			panic(err)
		}
		return sqlText
	}
	register := func(key, sql string) {
		q.v.registerStatement(key, sql, q.key, q.opts)
	}
	for _, m := range modes {
		prefix := q.key + m.suffix
		// 1. Zero-filter baseline.
		register(prefix+".find", must(q.buildSelectSQL(m.scoped, nil, q.defaultSort, q.defaultDir, false, false, false)))
		register(prefix+".page", must(q.buildSelectSQL(m.scoped, nil, q.defaultSort, q.defaultDir, true, false, false)))
		register(prefix+".count", must(q.buildCountSQL(m.scoped, nil)))

		// 2. All-filters canonical variant.
		if len(allIdx) > 0 {
			register(prefix+".all.page", must(q.buildSelectSQL(m.scoped, allIdx, q.defaultSort, q.defaultDir, true, false, false)))
			register(prefix+".all.count", must(q.buildCountSQL(m.scoped, allIdx)))
		}

		// 3. Individual single-filter variants.
		for i, flt := range q.filters {
			one := []int{i}
			register(prefix+".filter."+flt.name+".page", must(q.buildSelectSQL(m.scoped, one, q.defaultSort, q.defaultDir, true, false, false)))
			register(prefix+".filter."+flt.name+".count", must(q.buildCountSQL(m.scoped, one)))
		}

		// 4. Every selectable sort order (+ keyset variants when enabled).
		for _, cs := range q.sorts {
			if cs.key != q.defaultSort {
				register(prefix+".sort."+cs.key+".page", must(q.buildSelectSQL(m.scoped, nil, cs.key, q.defaultDir, true, false, false)))
			}
			if q.keyset {
				register(prefix+".keyset."+cs.key+".first", must(q.buildSelectSQL(m.scoped, nil, cs.key, q.defaultDir, false, true, false)))
				register(prefix+".keyset."+cs.key+".after", must(q.buildSelectSQL(m.scoped, nil, cs.key, q.defaultDir, false, true, true)))
			}
		}
	}
}
