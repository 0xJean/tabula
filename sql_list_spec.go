package tabula

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// SQLFilter declares one named, optional WHERE fragment of a SQLList.
// Parameters use @name placeholders bound from Args.
// When optionally names the Args field (by db tag name or exported Go field name)
// that toggles this filter (bool == true, pointer != nil, slice len > 0, string != "").
// When omitted, a filter referencing exactly one pointer Args parameter is enabled
// automatically whenever that pointer is non-nil.
type SQLFilter struct {
	Name   string
	SQL    string
	When   string
	Secret bool
}

// SQLListSpec declares a dynamic named-parameter list query using explicit
// Select, optional Where, optional Tail (GROUP BY / HAVING), optional optimized
// CountBase, optional Filters, whitelisted Order choices, and pagination bounds.
type SQLListSpec struct {
	Name      string
	Select    string
	Where     string
	Tail      string
	CountBase string
	Scope     QueryScope
	Filters   []SQLFilter
	Order     QueryOrderSpec
	MaxLimit  int
	// MaxOffset is the largest permitted QueryPage.Offset, inclusive. Zero allows any nonnegative offset.
	MaxOffset int
	Keyset    bool
	Observe   []StatementOption
}

type namedFragment struct {
	raw    string
	spans  []pgstmt.Span
	params []string // distinct parameter names in first-occurrence order
}

type compiledSQLFilter[Args any] struct {
	name   string
	frag   namedFragment
	when   func(*Args) bool
	secret bool
}

type compiledSQLListSortTerm struct {
	expr         string
	colName      string
	colIdx       int
	dir          SortDirection
	allowedTypes []string
}

type compiledSQLListSort struct {
	key   string
	terms []compiledSQLListSortTerm
}

// SQLList is a compiled, registered named-parameter dynamic list query that
// executes exclusively through AuthorizedSession (SQLAuthority).
type SQLList[Row, Args, S any] struct {
	reg          *Registry[S]
	name         string
	entry        *registration
	row          *descriptor[Row, S]
	resultFields []ShapeField
	args         *compiledSQLArgs[Args]
	selectFrag   namedFragment
	whereFrag    namedFragment
	tailFrag     namedFragment
	countFrag    namedFragment
	hasWhere     bool
	hasTail      bool
	hasCountBase bool
	scope        resolvedSQLScope
	filters      []compiledSQLFilter[Args]
	defaultSort  string
	defaultDir   SortDirection
	sorts        []compiledSQLListSort
	sortIndex    map[string]int
	maxLimit     int
	maxOffset    int
	keyset       bool
	meta         stmtMeta
}

// DefineSQLList validates spec against r, registers its finite verification
// variants in Definition.Statements and Definition.StatementShapes, and returns
// a compiled SQLList handle. Panics on declaration mistakes at initialization.
func DefineSQLList[Row, Args, S any](r *Registry[S], spec SQLListSpec) *SQLList[Row, Args, S] {
	l, err := TryDefineSQLList[Row, Args](r, spec)
	if err != nil {
		panic(err)
	}
	return l
}

// TryDefineSQLList validates and atomically registers a named-parameter list
// query on r, publishing nothing on error.
func TryDefineSQLList[Row, Args, S any](r *Registry[S], spec SQLListSpec) (*SQLList[Row, Args, S], error) {
	if r == nil {
		return nil, errors.New("tabula.DefineSQLList: registry is required")
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" || strings.ContainsRune(name, 0) {
		return nil, fmt.Errorf("%s.DefineSQLList: Name is required", r.prefix)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s.DefineSQLList %s: "+format, append([]any{r.prefix, name}, args...)...)
	}

	rowDesc, resultFields, err := compileSQLRow[Row](r, name, fail)
	if err != nil {
		return nil, err
	}
	scope, err := resolveSQLScope(r, spec.Scope, fail)
	if err != nil {
		return nil, err
	}
	argsDesc, err := compileSQLArgs[Args](r, fail)
	if err != nil {
		return nil, err
	}
	if scope.mode == scopeParam {
		if _, conflict := argsDesc.byName[scope.param]; conflict {
			return nil, fail("ScopeParam %q must not also be declared on Args", scope.param)
		}
	}

	selFrag, err := compileNamedFragment(r, spec.Select)
	if err != nil {
		return nil, fail("Select: %w", err)
	}
	var whrFrag namedFragment
	hasWhere := strings.TrimSpace(spec.Where) != ""
	if hasWhere {
		whrFrag, err = compileNamedFragment(r, spec.Where)
		if err != nil {
			return nil, fail("Where: %w", err)
		}
	}
	var tailFrag namedFragment
	hasTail := strings.TrimSpace(spec.Tail) != ""
	if hasTail {
		tailFrag, err = compileNamedFragment(r, spec.Tail)
		if err != nil {
			return nil, fail("Tail: %w", err)
		}
	}
	var cntFrag namedFragment
	hasCountBase := strings.TrimSpace(spec.CountBase) != ""
	if hasCountBase {
		if hasTail {
			return nil, fail("CountBase cannot combine with Tail")
		}
		cntFrag, err = compileNamedFragment(r, spec.CountBase)
		if err != nil {
			return nil, fail("CountBase: %w", err)
		}
	}
	if spec.Keyset && hasTail {
		return nil, fail("Keyset pagination cannot combine with Tail")
	}

	usedField := make([]bool, len(argsDesc.fields))
	checkParams := func(label string, params []string) (scopeSeen bool, err error) {
		for _, pName := range params {
			if scope.mode == scopeParam && pName == scope.param {
				scopeSeen = true
				continue
			}
			idx, ok := argsDesc.byName[pName]
			if !ok {
				return false, fail("%s references unknown parameter @%s", label, pName)
			}
			usedField[idx] = true
		}
		return scopeSeen, nil
	}
	scopeInSel, err := checkParams("Select", selFrag.params)
	if err != nil {
		return nil, err
	}
	scopeInWhr, err := checkParams("Where", whrFrag.params)
	if err != nil {
		return nil, err
	}
	if _, err := checkParams("Tail", tailFrag.params); err != nil {
		return nil, err
	}
	scopeInCnt, err := checkParams("CountBase", cntFrag.params)
	if err != nil {
		return nil, err
	}
	if scope.mode == scopeParam {
		if !scopeInSel && !scopeInWhr {
			return nil, fail("ScopeParam @%s must be referenced in Select or Where", scope.param)
		}
		if hasCountBase && !scopeInCnt && !scopeInWhr {
			return nil, fail("ScopeParam @%s must be referenced in CountBase or Where", scope.param)
		}
	}

	filters := make([]compiledSQLFilter[Args], 0, len(spec.Filters))
	filterNames := make(map[string]bool, len(spec.Filters))
	for _, flt := range spec.Filters {
		fName := strings.TrimSpace(flt.Name)
		if fName == "" || strings.ContainsAny(fName, " .\t\r\n") {
			return nil, fail("invalid filter name %q", flt.Name)
		}
		if filterNames[fName] {
			return nil, fail("duplicate filter %q", fName)
		}
		filterNames[fName] = true
		cf, err := compileNamedFragment(r, flt.SQL)
		if err != nil {
			return nil, fail("filter %q: %w", fName, err)
		}
		if _, err := checkParams("filter "+fName, cf.params); err != nil {
			return nil, err
		}
		whenFn, whenTag, err := resolveFilterWhen(argsDesc, scope, fName, cf.params, flt.When)
		if err != nil {
			return nil, fail("filter %q: %w", fName, err)
		}
		if whenTag != "" {
			if idx, ok := argsDesc.byName[whenTag]; ok {
				usedField[idx] = true
			}
		}
		filters = append(filters, compiledSQLFilter[Args]{
			name:   fName,
			frag:   cf,
			when:   whenFn,
			secret: flt.Secret,
		})
	}

	for i, f := range argsDesc.fields {
		if !usedField[i] {
			return nil, fail("args field %s (db:%q) is not referenced in SQL or filter When", f.goFieldName, f.name)
		}
	}

	if spec.MaxLimit <= 0 || spec.MaxLimit >= math.MaxInt {
		return nil, fail("MaxLimit must satisfy 0 < MaxLimit < math.MaxInt, got %d", spec.MaxLimit)
	}
	if spec.MaxOffset < 0 {
		return nil, fail("MaxOffset must be nonnegative, got %d", spec.MaxOffset)
	}

	defaultSort, defaultDir, sorts, sortIndex, err := compileSQLListOrder(r, rowDesc, resultFields, spec.Order, spec.Keyset, fail)
	if err != nil {
		return nil, err
	}

	l := &SQLList[Row, Args, S]{
		reg:          r,
		name:         name,
		row:          rowDesc,
		resultFields: resultFields,
		args:         argsDesc,
		selectFrag:   selFrag,
		whereFrag:    whrFrag,
		tailFrag:     tailFrag,
		countFrag:    cntFrag,
		hasWhere:     hasWhere,
		hasTail:      hasTail,
		hasCountBase: hasCountBase,
		scope:        scope,
		filters:      filters,
		defaultSort:  defaultSort,
		defaultDir:   defaultDir,
		sorts:        sorts,
		sortIndex:    sortIndex,
		maxLimit:     spec.MaxLimit,
		maxOffset:    spec.MaxOffset,
		keyset:       spec.Keyset,
		meta:         statementMeta(name, spec.Observe),
	}

	def, err := l.buildVerificationDefinition()
	if err != nil {
		return nil, fail("%w", err)
	}
	entry, err := r.recordSQLDefinition(def)
	if err != nil {
		return nil, err
	}
	l.entry = entry
	return l, nil
}

func compileSQLListOrder[Row, S any](
	r *Registry[S],
	rowDesc *descriptor[Row, S],
	resultFields []ShapeField,
	order QueryOrderSpec,
	keyset bool,
	fail func(string, ...any) error,
) (string, SortDirection, []compiledSQLListSort, map[string]int, error) {
	defaultDir := order.DefaultDirection
	if defaultDir == SortDefault {
		defaultDir = SortDesc
	}
	if _, err := defaultDir.sql(); err != nil {
		return "", SortDefault, nil, nil, fail("Order.DefaultDirection: %w", err)
	}
	if len(order.Sorts) == 0 {
		return "", SortDefault, nil, nil, fail("Order.Sorts requires at least one sort option")
	}
	if keyset && len(order.TieBreaker) == 0 {
		return "", SortDefault, nil, nil, fail("Keyset pagination requires Order.TieBreaker")
	}

	compileTerm := func(rawExpr, rawCol string, dir SortDirection) (compiledSQLListSortTerm, error) {
		expr := strings.TrimSpace(rawExpr)
		if expr == "" {
			return compiledSQLListSortTerm{}, errors.New("sort Expr is required")
		}
		if dir != SortDefault {
			if _, err := dir.sql(); err != nil {
				return compiledSQLListSortTerm{}, err
			}
		}
		frag, err := compileNamedFragment(r, expr)
		if err != nil {
			return compiledSQLListSortTerm{}, fmt.Errorf("invalid sort Expr %q: %w", expr, err)
		}
		if len(frag.params) > 0 {
			return compiledSQLListSortTerm{}, fmt.Errorf("sort Expr cannot contain parameter @%s", frag.params[0])
		}
		colName := strings.TrimSpace(rawCol)
		colIdx := -1
		var colTypes []string
		if colName != "" {
			idx, ok := rowDesc.byName[colName]
			if !ok {
				return compiledSQLListSortTerm{}, fmt.Errorf("sort Column %q is not declared on Row", colName)
			}
			if keyset && rowDesc.cols[idx].nullable {
				return compiledSQLListSortTerm{}, fmt.Errorf("keyset sort Column %q is nullable; project a non-null expression and bind that column", colName)
			}
			colIdx = idx
			colTypes = slices.Clone(resultFields[idx].AllowedTypes)
		} else if keyset {
			return compiledSQLListSortTerm{}, errors.New("keyset sort term requires matching Row Column")
		}
		return compiledSQLListSortTerm{
			expr:         expr,
			colName:      colName,
			colIdx:       colIdx,
			dir:          dir,
			allowedTypes: colTypes,
		}, nil
	}

	tieTerms := make([]compiledSQLListSortTerm, 0, len(order.TieBreaker))
	for i, tb := range order.TieBreaker {
		term, err := compileTerm(tb.Expr, tb.Column, tb.Direction)
		if err != nil {
			return "", SortDefault, nil, nil, fail("Order.TieBreaker[%d]: %w", i, err)
		}
		tieTerms = append(tieTerms, term)
	}

	sorts := make([]compiledSQLListSort, 0, len(order.Sorts))
	sortIndex := make(map[string]int, len(order.Sorts))
	for _, s := range order.Sorts {
		sKey := strings.TrimSpace(s.Key)
		if sKey == "" || strings.ContainsAny(sKey, " .\t\r\n") {
			return "", SortDefault, nil, nil, fail("invalid Order.Sorts Key %q", s.Key)
		}
		if _, dup := sortIndex[sKey]; dup {
			return "", SortDefault, nil, nil, fail("duplicate sort key %q", sKey)
		}
		primary, err := compileTerm(s.Expr, s.Column, s.Direction)
		if err != nil {
			return "", SortDefault, nil, nil, fail("Order.Sorts[%q]: %w", sKey, err)
		}
		terms := []compiledSQLListSortTerm{primary}
		for _, tb := range tieTerms {
			if (primary.colName != "" && primary.colName == tb.colName) || strings.EqualFold(primary.expr, tb.expr) {
				continue
			}
			terms = append(terms, tb)
		}
		if keyset {
			var fixed SortDirection
			for _, term := range terms {
				if term.dir != SortDefault {
					if fixed == SortDefault {
						fixed = term.dir
					} else if fixed != term.dir {
						return "", SortDefault, nil, nil, fail("Order.Sorts[%q]: mixed keyset directions are not supported", sKey)
					}
				}
			}
			if fixed != SortDefault {
				for _, term := range terms {
					if term.dir == SortDefault {
						return "", SortDefault, nil, nil, fail("Order.Sorts[%q]: keyset tie-breaker direction must match primary (leave Direction as SortDefault to follow caller)", sKey)
					}
				}
			}
		}
		sortIndex[sKey] = len(sorts)
		sorts = append(sorts, compiledSQLListSort{key: sKey, terms: terms})
	}
	defSort := strings.TrimSpace(order.Default)
	if defSort == "" {
		return "", SortDefault, nil, nil, fail("Order.Default is required")
	}
	if _, ok := sortIndex[defSort]; !ok {
		return "", SortDefault, nil, nil, fail("Order.Default %q is not declared in Order.Sorts", defSort)
	}
	return defSort, defaultDir, sorts, sortIndex, nil
}
