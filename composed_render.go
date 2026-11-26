package tabula

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

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
