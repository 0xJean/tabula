package tabula

// A bounded subset can change between executions without a total result
// order, even when every row has a key. Keep uniqueness separate from the
// repeatability of the selected subset.
func queryLimitValueProof(c *typedCompiler, q ReadSelect, fields []queryResultField) queryValueProof {
	proof := queryValueProof{valid: true, exact: true}
	if q.limit == nil || q.from == nil && len(q.joins) == 0 || len(q.group) == 0 && readHasAggregate(q) {
		return proof
	}
	q.limit = nil
	ordered := cloneQueryFields(fields)
	for _, term := range q.order {
		value := term.orderValue()
		if queryProjectedIdentity(ordered, value.part) == nil {
			ordered = append(ordered, queryResultField{id: new(queryFieldIdentity), value: value, stability: queryStability(value.part)})
		}
	}
	queryProveFields(c, q, ordered)
	result := statementResult{fields: ordered, keys: queryProjectionKeys(c, q, ordered)}
	proof, err := queryFinalResultProof(result, q.order)
	if err != nil {
		return queryValueProof{}
	}
	// Choosing a row does not turn its value into an equality representative.
	proof.exact = true
	return proof
}
