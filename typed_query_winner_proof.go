package tabula

import "fmt"

func queryWinnerRequirements(c *typedCompiler, q ReadSelect) (queryValueProof, error) {
	base := q
	base.distinct, base.distinctOn = false, nil
	fields := make([]queryResultField, len(q.order))
	for i, term := range q.order {
		value := term.orderValue()
		fields[i] = queryResultField{id: new(queryFieldIdentity), value: value, stability: queryStability(value.part)}
	}
	queryProveFields(c, base, fields)
	if len(q.group) == 0 && readHasAggregate(q) {
		proof := queryValueProof{valid: true, exact: true}
		for _, f := range fields {
			proof = proof.combine(f.proof)
		}
		if !proof.valid {
			return queryValueProof{}, fmt.Errorf("tabula: DISTINCT ON winner requires proved repeatable values")
		}
		return proof, nil
	}
	result := statementResult{fields: fields, keys: queryProjectionKeys(c, base, fields)}
	proof, err := queryFinalResultProof(result, q.order)
	if err != nil {
		return queryValueProof{}, fmt.Errorf("tabula: DISTINCT ON winner: %w", err)
	}
	return proof, nil
}
