package tabula

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"slices"
	"strconv"
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
			if spec.Keyset && rowDesc.cols[idx].nullable {
				return compiledSQLListSortTerm{}, fmt.Errorf("keyset sort Column %q is nullable; project a non-null expression and bind that column", colName)
			}
			colIdx = idx
			colTypes = slices.Clone(resultFields[idx].AllowedTypes)
		} else if spec.Keyset {
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

	tieTerms := make([]compiledSQLListSortTerm, 0, len(spec.Order.TieBreaker))
	for i, tb := range spec.Order.TieBreaker {
		term, err := compileTerm(tb.Expr, tb.Column, tb.Direction)
		if err != nil {
			return nil, fail("Order.TieBreaker[%d]: %w", i, err)
		}
		tieTerms = append(tieTerms, term)
	}

	sorts := make([]compiledSQLListSort, 0, len(spec.Order.Sorts))
	sortIndex := make(map[string]int, len(spec.Order.Sorts))
	for _, s := range spec.Order.Sorts {
		sKey := strings.TrimSpace(s.Key)
		if sKey == "" || strings.ContainsAny(sKey, " .\t\r\n") {
			return nil, fail("invalid Order.Sorts Key %q", s.Key)
		}
		if _, dup := sortIndex[sKey]; dup {
			return nil, fail("duplicate sort key %q", sKey)
		}
		primary, err := compileTerm(s.Expr, s.Column, s.Direction)
		if err != nil {
			return nil, fail("Order.Sorts[%q]: %w", sKey, err)
		}
		terms := []compiledSQLListSortTerm{primary}
		for _, tb := range tieTerms {
			if (primary.colName != "" && primary.colName == tb.colName) || strings.EqualFold(primary.expr, tb.expr) {
				continue
			}
			terms = append(terms, tb)
		}
		if spec.Keyset {
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
				for _, term := range terms {
					if term.dir == SortDefault {
						return nil, fail("Order.Sorts[%q]: keyset tie-breaker direction must match primary (leave Direction as SortDefault to follow caller)", sKey)
					}
				}
			}
		}
		sortIndex[sKey] = len(sorts)
		sorts = append(sorts, compiledSQLListSort{key: sKey, terms: terms})
	}
	defSort := strings.TrimSpace(spec.Order.Default)
	if defSort == "" {
		return nil, fail("Order.Default is required")
	}
	if _, ok := sortIndex[defSort]; !ok {
		return nil, fail("Order.Default %q is not declared in Order.Sorts", defSort)
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
		defaultSort:  defSort,
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

func compileNamedFragment[S any](r *Registry[S], raw string) (namedFragment, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return namedFragment{}, errors.New("SQL fragment is empty")
	}
	spans, err := pgstmt.ScanNamed(trimmed, true)
	if err != nil {
		return namedFragment{}, err
	}
	if err := r.checkSQLGuards(trimmed); err != nil {
		return namedFragment{}, err
	}
	seen := make(map[string]bool)
	var params []string
	for _, sp := range spans {
		if sp.Kind == pgstmt.NamedParam && !seen[sp.Text] {
			seen[sp.Text] = true
			params = append(params, sp.Text)
		}
	}
	return namedFragment{raw: trimmed, spans: spans, params: params}, nil
}

func resolveFilterWhen[Args any](
	argsDesc *compiledSQLArgs[Args],
	scope resolvedSQLScope,
	filterName string,
	fragParams []string,
	explicitWhen string,
) (func(*Args) bool, string, error) {
	whenKey := strings.TrimSpace(explicitWhen)
	if whenKey != "" {
		if argsDesc.ambiguous[whenKey] {
			return nil, "", fmt.Errorf("When %q matches multiple Args fields; use the db tag name", whenKey)
		}
		wf, ok := argsDesc.whenByKey[whenKey]
		if !ok {
			if _, exists := argsDesc.byName[whenKey]; exists {
				return nil, "", fmt.Errorf("When field %q must have bool, pointer, slice or string type", whenKey)
			}
			return nil, "", fmt.Errorf("When field %q not found on Args (or has unsupported switch type)", whenKey)
		}
		return wf.active, wf.tagName, nil
	}

	// Auto-infer when omitted: require exactly one distinct non-scope Args parameter of pointer type.
	var nonScope []string
	for _, p := range fragParams {
		if scope.mode == scopeParam && p == scope.param {
			continue
		}
		nonScope = append(nonScope, p)
	}
	if len(nonScope) != 1 {
		return nil, "", fmt.Errorf("filter %q has %d Args parameters; specify When explicitly", filterName, len(nonScope))
	}
	idx := argsDesc.byName[nonScope[0]]
	f := argsDesc.fields[idx]
	if f.goType.Kind() != reflect.Pointer {
		return nil, "", fmt.Errorf("filter %q parameter @%s has non-pointer type %s; specify When explicitly", filterName, f.name, f.goType)
	}
	wf := argsDesc.whenByKey[f.name]
	return wf.active, f.name, nil
}

type builtSQLListVariant struct {
	sql   string
	shape StatementShape
	slots []sqlParamSlot
}

var intAllowedTypes = []string{"smallint", "integer", "bigint"}

func (l *SQLList[Row, Args, S]) resolveOrder(sortKey string, dir SortDirection) (compiledSQLListSort, SortDirection, string, error) {
	if sortKey == "" {
		sortKey = l.defaultSort
	}
	idx, ok := l.sortIndex[sortKey]
	if !ok {
		return compiledSQLListSort{}, SortDefault, "", fmt.Errorf("%s.SQLList %s: unknown sort key %q", l.reg.prefix, l.name, sortKey)
	}
	cs := l.sorts[idx]
	primaryDir := cs.terms[0].dir
	fallback := cmp.Or(dir, primaryDir, l.defaultDir)
	if _, err := fallback.sql(); err != nil {
		return compiledSQLListSort{}, SortDefault, "", fmt.Errorf("%s.SQLList %s: %w", l.reg.prefix, l.name, err)
	}
	parts := make([]string, len(cs.terms))
	for i, term := range cs.terms {
		dirSQL, _ := cmp.Or(term.dir, fallback).sql()
		parts[i] = term.expr + " " + dirSQL
	}
	return cs, cmp.Or(primaryDir, fallback), "\nORDER BY " + strings.Join(parts, ", "), nil
}

type listSlotTable[Args any] struct {
	args   *compiledSQLArgs[Args]
	scope  resolvedSQLScope
	allSec bool
	byName map[string]int
	slots  []sqlParamSlot
	inputs []ShapeInput
}

func newListSlotTable[Args any](args *compiledSQLArgs[Args], scope resolvedSQLScope, allSec bool) *listSlotTable[Args] {
	return &listSlotTable[Args]{
		args:   args,
		scope:  scope,
		allSec: allSec,
		byName: make(map[string]int),
	}
}

func (t *listSlotTable[Args]) slotFor(name string, forceSecret bool) (int, error) {
	if pos, ok := t.byName[name]; ok {
		if forceSecret {
			t.slots[pos-1].secret = true
			t.inputs[pos-1].Secret = true
		}
		return pos, nil
	}
	if t.scope.mode == scopeParam && name == t.scope.param {
		sec := t.allSec || forceSecret
		t.slots = append(t.slots, sqlParamSlot{name: name, isScope: true, secret: sec})
		t.inputs = append(t.inputs, ShapeInput{Name: name, Scope: true, Secret: sec, AllowedTypes: slices.Clone(t.scope.types)})
		pos := len(t.slots)
		t.byName[name] = pos
		return pos, nil
	}
	idx, ok := t.args.byName[name]
	if !ok {
		return 0, fmt.Errorf("unknown parameter @%s", name)
	}
	f := t.args.fields[idx]
	sec := t.allSec || forceSecret || f.secret
	t.slots = append(t.slots, sqlParamSlot{name: name, fieldIdx: idx, secret: sec})
	t.inputs = append(t.inputs, ShapeInput{Name: name, Secret: sec, AllowedTypes: slices.Clone(f.allowedTypes)})
	pos := len(t.slots)
	t.byName[name] = pos
	return pos, nil
}

func (t *listSlotTable[Args]) rewrite(frag namedFragment, forceSecret bool) (string, error) {
	return pgstmt.RewriteNamed(frag.raw, frag.spans, func(name string) (int, error) {
		return t.slotFor(name, forceSecret)
	})
}

func (l *SQLList[Row, Args, S]) buildSelectVariant(enabled []int, sortKey string, dir SortDirection, withOffset, withKeysetLimit, withAfter bool) (builtSQLListVariant, error) {
	tbl := newListSlotTable(l.args, l.scope, l.meta.secret)
	selSQL, err := tbl.rewrite(l.selectFrag, false)
	if err != nil {
		return builtSQLListVariant{}, err
	}
	var whereParts []string
	if l.hasWhere {
		wSQL, err := tbl.rewrite(l.whereFrag, false)
		if err != nil {
			return builtSQLListVariant{}, err
		}
		whereParts = append(whereParts, "("+wSQL+")")
	}
	for _, idx := range enabled {
		f := l.filters[idx]
		fSQL, err := tbl.rewrite(f.frag, f.secret)
		if err != nil {
			return builtSQLListVariant{}, err
		}
		whereParts = append(whereParts, "("+fSQL+")")
	}
	cs, effDir, orderSQL, err := l.resolveOrder(sortKey, dir)
	if err != nil {
		return builtSQLListVariant{}, err
	}
	if withAfter {
		op := ">"
		if effDir == SortDesc {
			op = "<"
		}
		if len(cs.terms) == 1 {
			next := len(tbl.inputs) + 1
			tbl.inputs = append(tbl.inputs, ShapeInput{
				Name:         "after_" + cs.terms[0].colName,
				Secret:       l.meta.secret,
				AllowedTypes: slices.Clone(cs.terms[0].allowedTypes),
			})
			whereParts = append(whereParts, "("+cs.terms[0].expr+" "+op+" $"+strconv.Itoa(next)+")")
		} else {
			exprs := make([]string, len(cs.terms))
			holders := make([]string, len(cs.terms))
			for i, term := range cs.terms {
				next := len(tbl.inputs) + 1
				tbl.inputs = append(tbl.inputs, ShapeInput{
					Name:         "after_" + term.colName,
					Secret:       l.meta.secret,
					AllowedTypes: slices.Clone(term.allowedTypes),
				})
				exprs[i] = term.expr
				holders[i] = "$" + strconv.Itoa(next)
			}
			whereParts = append(whereParts, "(("+strings.Join(exprs, ", ")+") "+op+" ("+strings.Join(holders, ", ")+"))")
		}
	}

	var b strings.Builder
	b.WriteString(selSQL)
	if len(whereParts) > 0 {
		b.WriteString("\nWHERE ")
		b.WriteString(strings.Join(whereParts, "\n  AND "))
	}
	if l.hasTail {
		tSQL, err := tbl.rewrite(l.tailFrag, false)
		if err != nil {
			return builtSQLListVariant{}, err
		}
		b.WriteByte('\n')
		b.WriteString(tSQL)
	}
	b.WriteString(orderSQL)
	if withOffset {
		limSlot := len(tbl.inputs) + 1
		offSlot := limSlot + 1
		tbl.inputs = append(tbl.inputs,
			ShapeInput{Name: "limit", Secret: l.meta.secret, AllowedTypes: slices.Clone(intAllowedTypes)},
			ShapeInput{Name: "offset", Secret: l.meta.secret, AllowedTypes: slices.Clone(intAllowedTypes)},
		)
		b.WriteString(" LIMIT $" + strconv.Itoa(limSlot) + " OFFSET $" + strconv.Itoa(offSlot))
	} else if withKeysetLimit {
		limSlot := len(tbl.inputs) + 1
		tbl.inputs = append(tbl.inputs,
			ShapeInput{Name: "limit", Secret: l.meta.secret, AllowedTypes: slices.Clone(intAllowedTypes)},
		)
		b.WriteString(" LIMIT $" + strconv.Itoa(limSlot))
	}

	return builtSQLListVariant{
		sql: b.String(),
		shape: StatementShape{
			Inputs:  tbl.inputs,
			Result:  l.resultFields,
			Effects: EffectRead,
		},
		slots: tbl.slots,
	}, nil
}

func (l *SQLList[Row, Args, S]) buildCountVariant(enabled []int) (builtSQLListVariant, error) {
	tbl := newListSlotTable(l.args, l.scope, l.meta.secret)
	var baseSQL string
	var err error
	if l.hasCountBase {
		baseSQL, err = tbl.rewrite(l.countFrag, false)
	} else {
		baseSQL, err = tbl.rewrite(l.selectFrag, false)
	}
	if err != nil {
		return builtSQLListVariant{}, err
	}
	var whereParts []string
	if l.hasWhere {
		wSQL, err := tbl.rewrite(l.whereFrag, false)
		if err != nil {
			return builtSQLListVariant{}, err
		}
		whereParts = append(whereParts, "("+wSQL+")")
	}
	for _, idx := range enabled {
		f := l.filters[idx]
		fSQL, err := tbl.rewrite(f.frag, f.secret)
		if err != nil {
			return builtSQLListVariant{}, err
		}
		whereParts = append(whereParts, "("+fSQL+")")
	}

	var query string
	if l.hasCountBase {
		var b strings.Builder
		b.WriteString(baseSQL)
		if len(whereParts) > 0 {
			b.WriteString("\nWHERE ")
			b.WriteString(strings.Join(whereParts, "\n  AND "))
		}
		query = b.String()
	} else {
		var inner strings.Builder
		inner.WriteString(baseSQL)
		if len(whereParts) > 0 {
			inner.WriteString("\nWHERE ")
			inner.WriteString(strings.Join(whereParts, "\n  AND "))
		}
		if l.hasTail {
			tSQL, err := tbl.rewrite(l.tailFrag, false)
			if err != nil {
				return builtSQLListVariant{}, err
			}
			inner.WriteByte('\n')
			inner.WriteString(tSQL)
		}
		query = "SELECT count(*) FROM (" + inner.String() + ") AS tabula_count"
	}

	return builtSQLListVariant{
		sql: query,
		shape: StatementShape{
			Inputs: tbl.inputs,
			Result: []ShapeField{{
				Name:         "count",
				AllowedTypes: slices.Clone(intAllowedTypes),
			}},
			Effects: EffectRead,
		},
		slots: tbl.slots,
	}, nil
}

func (l *SQLList[Row, Args, S]) buildVerificationDefinition() (Definition, error) {
	def := Definition{
		Name:             l.name,
		Columns:          l.row.definedColumns(),
		Statements:       make(map[string]string),
		StatementQueries: make(map[string]string),
		StatementShapes:  make(map[string]StatementShape),
	}
	add := func(key string, build func() (builtSQLListVariant, error)) error {
		built, err := build()
		if err != nil {
			return err
		}
		if err := l.reg.checkSQLGuards(built.sql); err != nil {
			return fmt.Errorf("variant %s: %w", key, err)
		}
		cls, err := classifySQLStatement(built.sql, sqlKindRead, false)
		if err != nil {
			return fmt.Errorf("variant %s: %w", key, err)
		}
		if cls.effects&EffectRowLock != 0 {
			return fmt.Errorf("variant %s: row locks are not supported in DefineSQLList", key)
		}
		if cls.effects&EffectAdvisoryLock != 0 {
			return fmt.Errorf("variant %s: advisory locks are not supported in DefineSQLList", key)
		}
		if _, dup := def.Statements[key]; dup {
			return fmt.Errorf("duplicate variant key %q", key)
		}
		def.Statements[key] = built.sql
		def.StatementQueries[key] = l.name
		def.StatementShapes[key] = cloneShape(built.shape)
		return nil
	}

	// 1. Zero-filter baseline.
	if err := add("find", func() (builtSQLListVariant, error) {
		return l.buildSelectVariant(nil, l.defaultSort, l.defaultDir, false, false, false)
	}); err != nil {
		return Definition{}, err
	}
	if err := add("page", func() (builtSQLListVariant, error) {
		return l.buildSelectVariant(nil, l.defaultSort, l.defaultDir, true, false, false)
	}); err != nil {
		return Definition{}, err
	}
	if err := add("count", func() (builtSQLListVariant, error) {
		return l.buildCountVariant(nil)
	}); err != nil {
		return Definition{}, err
	}

	// 2. All-filters canonical variant.
	if len(l.filters) > 0 {
		allIdx := make([]int, len(l.filters))
		for i := range l.filters {
			allIdx[i] = i
		}
		if err := add("all.page", func() (builtSQLListVariant, error) {
			return l.buildSelectVariant(allIdx, l.defaultSort, l.defaultDir, true, false, false)
		}); err != nil {
			return Definition{}, err
		}
		if err := add("all.count", func() (builtSQLListVariant, error) {
			return l.buildCountVariant(allIdx)
		}); err != nil {
			return Definition{}, err
		}
	}

	// 3. Individual single-filter variants.
	for i, flt := range l.filters {
		one := []int{i}
		if err := add("filter."+flt.name+".page", func() (builtSQLListVariant, error) {
			return l.buildSelectVariant(one, l.defaultSort, l.defaultDir, true, false, false)
		}); err != nil {
			return Definition{}, err
		}
		if err := add("filter."+flt.name+".count", func() (builtSQLListVariant, error) {
			return l.buildCountVariant(one)
		}); err != nil {
			return Definition{}, err
		}
	}

	// 4. Selectable sort orders and optional keyset variants.
	for _, cs := range l.sorts {
		if cs.key != l.defaultSort {
			if err := add("sort."+cs.key+".page", func() (builtSQLListVariant, error) {
				return l.buildSelectVariant(nil, cs.key, l.defaultDir, true, false, false)
			}); err != nil {
				return Definition{}, err
			}
		}
		if l.keyset {
			if err := add("keyset."+cs.key+".first", func() (builtSQLListVariant, error) {
				return l.buildSelectVariant(nil, cs.key, l.defaultDir, false, true, false)
			}); err != nil {
				return Definition{}, err
			}
			if err := add("keyset."+cs.key+".after", func() (builtSQLListVariant, error) {
				return l.buildSelectVariant(nil, cs.key, l.defaultDir, false, true, true)
			}); err != nil {
				return Definition{}, err
			}
		}
	}

	return def, nil
}

// Name returns the registered declaration name.
func (l *SQLList[Row, Args, S]) Name() string { return l.name }

// Definition returns a deep copy of the registered Definition for sqlcheck.
func (l *SQLList[Row, Args, S]) Definition() Definition {
	l.reg.mu.RLock()
	defer l.reg.mu.RUnlock()
	return cloneDefinition(l.entry.def)
}

// Bind attaches this list query to an authorized session from the owning Registry.
func (l *SQLList[Row, Args, S]) Bind(s Session[S]) *BoundSQLList[Row, Args, S] {
	if l == nil || l.reg == nil {
		return &BoundSQLList[Row, Args, S]{binding: sqlSessionBinding[S]{bindErr: ErrStatementAuthority}}
	}
	return &BoundSQLList[Row, Args, S]{l: l, binding: bindSQLSession(l.reg, s)}
}

// BoundSQLList executes a registered SQLList on one authorized session.
type BoundSQLList[Row, Args, S any] struct {
	l       *SQLList[Row, Args, S]
	binding sqlSessionBinding[S]
}

// WithLogger returns a handle with a per-binding logger override.
func (b *BoundSQLList[Row, Args, S]) WithLogger(logger *slog.Logger) *BoundSQLList[Row, Args, S] {
	clone := *b
	clone.binding.logger = logger
	return &clone
}

func (b *BoundSQLList[Row, Args, S]) enabledFilters(args *Args) []int {
	var enabled []int
	for i, f := range b.l.filters {
		if f.when(args) {
			enabled = append(enabled, i)
		}
	}
	return enabled
}

func (b *BoundSQLList[Row, Args, S]) prepareVariant(action string, method CallMethod, built builtSQLListVariant, args *Args, extraVals []any) (AuthorizedStatement, error) {
	if b.binding.bindErr != nil {
		return AuthorizedStatement{}, b.binding.bindErr
	}
	values := make([]any, 0, len(built.slots)+len(extraVals))
	secret := make([]bool, 0, len(built.slots)+len(extraVals))
	for _, slot := range built.slots {
		secret = append(secret, slot.secret)
		if slot.isScope {
			if b.binding.s.Unscoped() {
				return AuthorizedStatement{}, ErrStatementAuthority
			}
			values = append(values, b.l.reg.scopeValue(b.binding.s.Scope()))
			continue
		}
		v, err := b.l.args.fields[slot.fieldIdx].bind(args)
		if err != nil {
			return AuthorizedStatement{}, fmt.Errorf("%s.SQLList %s: encode parameter @%s: %w", b.l.reg.prefix, b.l.name, slot.name, err)
		}
		values = append(values, v)
	}
	for _, ev := range extraVals {
		values = append(values, ev)
		secret = append(secret, b.l.meta.secret)
	}
	shape := &compiledShape{
		observationKey: b.l.name + "." + action,
		tableName:      b.l.name,
		action:         action,
		authority:      b.l.reg.statementIssuer,
		key:            action,
		sql:            built.sql,
		shape:          built.shape,
	}
	return b.binding.authorize(shape, method, values, secret, b.l.meta, b.l.scope.mode == scopeParam)
}

func (b *BoundSQLList[Row, Args, S]) runCount(ctx context.Context, enabled []int, args *Args) (int64, error) {
	built, err := b.l.buildCountVariant(enabled)
	if err != nil {
		return 0, err
	}
	call, err := b.prepareVariant("count", CallScan, built, args, nil)
	if err != nil {
		return 0, err
	}
	var total int64
	_, err = b.binding.runSQL(ctx, b.l.name, "count", b.l.meta, call, func(ctx context.Context) (int64, error) {
		return -1, b.binding.driver.ScanAuthorized(ctx, call, &total)
	})
	return total, err
}

// List reads one offset-paginated page plus optional exact total count.
// CountMode must be CountExact (default) or CountNone (total = -1).
func (b *BoundSQLList[Row, Args, S]) List(ctx context.Context, args Args, page QueryPage) ([]Row, int64, error) {
	if b == nil || b.l == nil {
		return nil, 0, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return nil, 0, b.binding.bindErr
	}
	if page.Size < 1 || page.Size > b.l.maxLimit || page.Offset < 0 {
		return nil, 0, fmt.Errorf("%s.SQLList %s: invalid page size=%d offset=%d (max=%d)", b.l.reg.prefix, b.l.name, page.Size, page.Offset, b.l.maxLimit)
	}
	if b.l.maxOffset > 0 && page.Offset > b.l.maxOffset {
		return nil, 0, fmt.Errorf("%s.SQLList %s: offset %d exceeds MaxOffset %d", b.l.reg.prefix, b.l.name, page.Offset, b.l.maxOffset)
	}
	if page.Count != CountExact && page.Count != CountNone {
		return nil, 0, fmt.Errorf("%s.SQLList %s: unsupported CountMode %d (use CountExact or CountNone)", b.l.reg.prefix, b.l.name, page.Count)
	}
	enabled := b.enabledFilters(&args)
	pageBuilt, err := b.l.buildSelectVariant(enabled, page.Sort, page.Direction, true, false, false)
	if err != nil {
		return nil, 0, err
	}
	pageCall, err := b.prepareVariant("list", CallEach, pageBuilt, &args, []any{page.Size, page.Offset})
	if err != nil {
		return nil, 0, err
	}
	total := int64(-1)
	if page.Count == CountExact {
		total, err = b.runCount(ctx, enabled, &args)
		if err != nil {
			return nil, 0, err
		}
	}
	rows, err := execSQLAll(ctx, &b.binding, b.l.row, b.l.name, "list", b.l.meta, pageCall)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// Count returns the exact number of matching rows before pagination.
func (b *BoundSQLList[Row, Args, S]) Count(ctx context.Context, args Args) (int64, error) {
	if b == nil || b.l == nil {
		return 0, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return 0, b.binding.bindErr
	}
	enabled := b.enabledFilters(&args)
	return b.runCount(ctx, enabled, &args)
}

// All reads all matching rows in the selected order without pagination or COUNT(*).
func (b *BoundSQLList[Row, Args, S]) All(ctx context.Context, args Args, order QueryOrder) ([]Row, error) {
	if b == nil || b.l == nil {
		return nil, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return nil, b.binding.bindErr
	}
	enabled := b.enabledFilters(&args)
	built, err := b.l.buildSelectVariant(enabled, order.Sort, order.Direction, false, false, false)
	if err != nil {
		return nil, err
	}
	call, err := b.prepareVariant("all", CallEach, built, &args, nil)
	if err != nil {
		return nil, err
	}
	return execSQLAll(ctx, &b.binding, b.l.row, b.l.name, "all", b.l.meta, call)
}

// Each streams all matching rows in the selected order to fn.
func (b *BoundSQLList[Row, Args, S]) Each(ctx context.Context, args Args, order QueryOrder, fn func(Row) error) error {
	if b == nil || b.l == nil {
		return ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return b.binding.bindErr
	}
	enabled := b.enabledFilters(&args)
	built, err := b.l.buildSelectVariant(enabled, order.Sort, order.Direction, false, false, false)
	if err != nil {
		return err
	}
	call, err := b.prepareVariant("each", CallEach, built, &args, nil)
	if err != nil {
		return err
	}
	return execSQLEach(ctx, &b.binding, b.l.row, b.l.name, "each", b.l.meta, call, fn)
}

// ListKeyset reads one cursor-paginated page without OFFSET, fetching Limit+1
// rows to set HasMore and populating Next from the last returned row.
func (b *BoundSQLList[Row, Args, S]) ListKeyset(ctx context.Context, args Args, page QueryKeyset) (KeysetPage[Row], error) {
	if b == nil || b.l == nil {
		return KeysetPage[Row]{}, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return KeysetPage[Row]{}, b.binding.bindErr
	}
	if !b.l.keyset {
		return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: keyset pagination is not enabled on this query", b.l.reg.prefix, b.l.name)
	}
	if page.Limit < 1 || page.Limit > b.l.maxLimit {
		return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: invalid keyset limit=%d (max=%d)", b.l.reg.prefix, b.l.name, page.Limit, b.l.maxLimit)
	}
	cs, _, _, err := b.l.resolveOrder(page.Sort, page.Direction)
	if err != nil {
		return KeysetPage[Row]{}, err
	}
	var extra []any
	if len(page.After) > 0 {
		if len(page.After) != len(cs.terms) {
			return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: After cursor has %d values, expected %d", b.l.reg.prefix, b.l.name, len(page.After), len(cs.terms))
		}
		extra = make([]any, 0, len(page.After)+1)
		for i, val := range page.After {
			if val == nil {
				return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: After[%d] must not be nil", b.l.reg.prefix, b.l.name, i)
			}
			col := b.l.row.cols[cs.terms[i].colIdx]
			gotType := reflect.TypeOf(val)
			if gotType != col.goType {
				return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: After[%d] has type %s, expected %s", b.l.reg.prefix, b.l.name, i, gotType, col.goType)
			}
			b.l.reg.mu.RLock()
			customWrap := b.l.reg.types[col.goType]
			b.l.reg.mu.RUnlock()
			if customWrap != nil {
				ptr := reflect.New(col.goType)
				ptr.Elem().Set(reflect.ValueOf(val))
				extra = append(extra, customWrap(ptr.Interface()))
			} else {
				extra = append(extra, val)
			}
		}
	}
	extra = append(extra, page.Limit+1)
	enabled := b.enabledFilters(&args)
	built, err := b.l.buildSelectVariant(enabled, page.Sort, page.Direction, false, true, len(page.After) > 0)
	if err != nil {
		return KeysetPage[Row]{}, err
	}
	call, err := b.prepareVariant("list_keyset", CallEach, built, &args, extra)
	if err != nil {
		return KeysetPage[Row]{}, err
	}
	rows, err := execSQLAll(ctx, &b.binding, b.l.row, b.l.name, "list_keyset", b.l.meta, call)
	if err != nil {
		return KeysetPage[Row]{}, err
	}
	hasMore := len(rows) > page.Limit
	var next []any
	if hasMore {
		rows = rows[:page.Limit]
		last := rows[len(rows)-1]
		next = make([]any, len(cs.terms))
		for i, term := range cs.terms {
			next[i] = reflect.ValueOf(b.l.row.cols[term.colIdx].field(&last)).Elem().Interface()
		}
	}
	return KeysetPage[Row]{Items: rows, HasMore: hasMore, Next: next}, nil
}
