package tabula

import (
	"fmt"
	"slices"
	"strings"
)

// Distinct removes duplicates of the complete projection. ORDER BY must use
// projected expressions. It cannot be combined with DistinctOn or row locks.
func (q ReadSelect) Distinct() ReadSelect {
	if q.distinct {
		q.clauseErr = fmt.Errorf("tabula: duplicate DISTINCT declaration")
	}
	q.distinct = true
	return q
}

// DistinctOn selects one representative per expression tuple. OrderBy must
// start with those keys and establish a stable total winner order in each group.
// Joined winners conservatively require every occurrence's complete key.
func (q ReadSelect) DistinctOn(first Scalar, rest ...Scalar) ReadSelect {
	if len(q.distinctOn) > 0 {
		q.clauseErr = fmt.Errorf("tabula: duplicate DISTINCT ON declaration")
	}
	q.distinctOn = make([]scalarOperand, 0, 1+len(rest))
	for _, value := range slices.Concat([]Scalar{first}, rest) {
		if isNilBound(value) {
			q.clauseErr = fmt.Errorf("tabula: DISTINCT ON needs declared expressions")
			continue
		}
		q.distinctOn = append(q.distinctOn, value.scalarOperand())
	}
	return q
}

func (q ReadSelect) checkDistinct(groups []scalarOperand, aggregate bool, sources []rowSource) error {
	if q.clauseErr != nil {
		return q.clauseErr
	}
	if q.distinct && len(q.distinctOn) > 0 {
		return fmt.Errorf("tabula: DISTINCT and DISTINCT ON cannot be combined")
	}
	if q.distinct {
		for _, projection := range q.project {
			if !projectionComparable(projection) {
				return fmt.Errorf("tabula: DISTINCT requires comparable projected types")
			}
		}
		for _, term := range q.order {
			if !slices.ContainsFunc(q.project, func(p Projection) bool { return sameExpression(p.part, term.orderValue().part) }) {
				return fmt.Errorf("tabula: DISTINCT order expressions must be projected")
			}
		}
	}
	if len(q.distinctOn) == 0 {
		return nil
	}
	if len(q.order) < len(q.distinctOn) {
		return fmt.Errorf("tabula: DISTINCT ON requires a leading winner order")
	}
	for i, key := range q.distinctOn {
		if !resultComparable(key.kind) || !repeatableExpression(key.part) {
			return fmt.Errorf("tabula: DISTINCT ON keys require repeatable comparable expressions")
		}
		if err := checkRowExpr(key.part, func(expressionPart) error { return nil }); err != nil {
			return err
		}
		if aggregate {
			if err := groupDetermined(key.part, groups); err != nil {
				return err
			}
		}
		for _, earlier := range q.distinctOn[:i] {
			if sameExpression(key.part, earlier.part) {
				return fmt.Errorf("tabula: duplicate DISTINCT ON key")
			}
		}
		if !orderIncludes(q.order[:len(q.distinctOn)], key.part) {
			return fmt.Errorf("tabula: DISTINCT ON requires an equivalent leading winner order")
		}
	}
	for _, term := range q.order {
		if err := validateOrderTerm(term); err != nil {
			return err
		}
		if !repeatableExpression(term.orderValue().part) {
			return fmt.Errorf("tabula: DISTINCT ON winner order must be repeatable")
		}
	}
	if aggregate {
		for _, group := range groups {
			if !orderIncludes(q.order, group.part) {
				return fmt.Errorf("tabula: DISTINCT ON winner order requires the complete group key")
			}
		}
		return nil
	}
	for _, src := range sources {
		if len(src.keys) == 0 {
			return fmt.Errorf("tabula: DISTINCT ON winner requires a declared source key")
		}
		for _, key := range src.keys {
			if !slices.ContainsFunc(q.order, func(term RowTerm) bool {
				f, ok := term.orderValue().part.(*fieldRef)
				return ok && f.source == src.rel && f.name == key
			}) {
				return fmt.Errorf("tabula: DISTINCT ON winner order needs the complete key of occurrence %q", src.rel.alias)
			}
		}
	}
	return nil
}

func projectionComparable(p Projection) bool { return resultComparable(p.kind) }

func (q ReadSelect) distinctSQL(c *typedCompiler) (string, error) {
	if q.distinct {
		return "DISTINCT ", nil
	}
	if len(q.distinctOn) == 0 {
		return "", nil
	}
	parts := make([]string, len(q.distinctOn))
	for i, key := range q.distinctOn {
		part, err := key.part.render(c)
		if err != nil {
			return "", err
		}
		parts[i] = part
	}
	return "DISTINCT ON (" + strings.Join(parts, ", ") + ") ", nil
}
