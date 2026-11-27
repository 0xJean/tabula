package tabula

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

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
			Inputs:    tbl.inputs,
			Result:    l.resultFields,
			Effects:   EffectRead,
			StructSQL: true,
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
			Effects:   EffectRead,
			StructSQL: true,
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
