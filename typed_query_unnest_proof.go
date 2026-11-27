package tabula

// Array expansion preserves an immutable input's values and representations.
// This is deliberately not a uniqueness proof: duplicates and NULL padding
// remain possible, and ordinality never supplies a persistent result key.
func queryUnnestValueProof(c *typedCompiler, value scalarOperand) queryValueProof {
	proof := proveQueryValue(c, ReadSelect{}, value.part)
	proof.valid = proof.valid && proof.exact
	return proof
}
