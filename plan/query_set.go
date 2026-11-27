package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
	"strings"
)

type querySet struct {
	operation string
	arms      []ReadSelect
}

func setQuery(operation string, a, b ReadSelect, rest []ReadSelect) ReadSelect {
	return ReadSelect{set: &querySet{operation, slices.Concat([]ReadSelect{a, b}, rest)}}
}

// Union removes duplicates from the combined query results.
func Union(a, b ReadSelect, rest ...ReadSelect) ReadSelect { return setQuery("UNION", a, b, rest) }

// UnionAll preserves every input row and its multiplicity.
func UnionAll(a, b ReadSelect, rest ...ReadSelect) ReadSelect {
	return setQuery("UNION ALL", a, b, rest)
}

// Intersect returns distinct rows present in every input.
func Intersect(a, b ReadSelect, rest ...ReadSelect) ReadSelect {
	return setQuery("INTERSECT", a, b, rest)
}

// IntersectAll preserves the minimum multiplicity of matching rows.
func IntersectAll(a, b ReadSelect, rest ...ReadSelect) ReadSelect {
	return setQuery("INTERSECT ALL", a, b, rest)
}

// Except removes matching rows from the first input and eliminates duplicates.
func Except(a, b ReadSelect, rest ...ReadSelect) ReadSelect { return setQuery("EXCEPT", a, b, rest) }

// ExceptAll subtracts matching multiplicities, in declared arm order.
func ExceptAll(a, b ReadSelect, rest ...ReadSelect) ReadSelect {
	return setQuery("EXCEPT ALL", a, b, rest)
}

func (q ReadSelect) renderSet(ctx *statementContext) (statementResult, error) {
	if q.from != nil || len(q.joins)+len(q.project)+len(q.where)+len(q.group)+len(q.having)+len(q.order) > 0 || q.distinct || len(q.distinctOn) > 0 || q.hasRowLocks() {
		return statementResult{}, fmt.Errorf("tabula: use Derived for clauses on a set result")
	}
	var result statementResult
	var sql []string
	var arms []statementResult
	for i, arm := range q.set.arms {
		if arm.hasRowLocks() {
			return statementResult{}, fmt.Errorf("tabula: a set arm cannot declare a row lock")
		}
		child := *ctx
		child.position = statementSetArm
		next, err := arm.renderStatement(&child)
		if err != nil {
			return statementResult{}, err
		}
		if tabula.ShapeEffects(next.effects.Bits)&tabula.EffectWrite != 0 {
			return statementResult{}, fmt.Errorf("tabula: set arms must be read-only")
		}
		if i == 0 {
			result = next.frozen()
		} else {
			if err := matchQueryFields(result.fields, next.fields); err != nil {
				return statementResult{}, err
			}
			for j, f := range next.fields {
				result.fields[j].value.nullable = result.fields[j].value.nullable || f.value.nullable
				result.fields[j].stability = max(result.fields[j].stability, f.stability)
				result.fields[j].stored = result.fields[j].stored && f.stored
				result.fields[j].origins = append(result.fields[j].origins, f.origins...)
				result.fields[j].secret = result.fields[j].secret || f.secret
			}
		}
		sql = append(sql, "("+next.sql+")")
		arms = append(arms, next)
	}
	result.sql = "(" + strings.Join(sql, " "+q.set.operation+" ") + ")"
	for i := range result.fields {
		result.fields[i].lineage = tabula.ResultLineage{}
	}
	result.keys = querySetResultKeys(q.set.operation, arms, result.fields)
	if q.limit != nil {
		// Set nodes have no ORDER BY of their own. Use an ordered Derived
		// query before bounding a subset that must support traversal.
		for i := range result.fields {
			result.fields[i].proof.valid = false
		}
	}
	if q.set.operation != "UNION ALL" {
		for _, field := range result.fields {
			if !resultComparable(field.value.kind) {
				return statementResult{}, fmt.Errorf("tabula: set equality requires comparable output types")
			}
		}
	}
	limit, err := q.limit.render(ctx.compiler)
	result.sql += limit
	return result, err
}
func matchQueryFields(a, b []queryResultField) error {
	if len(a) != len(b) || len(a) == 0 {
		return fmt.Errorf("tabula: set arms require matching nonempty output arity")
	}
	for i := range a {
		if !sameQueryType(a[i].value.kind, b[i].value.kind) {
			return fmt.Errorf("tabula: set output %d requires exactly matching SQL and Go types and codecs", i+1)
		}
	}
	return nil
}
