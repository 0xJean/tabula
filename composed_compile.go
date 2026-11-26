package tabula

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

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
