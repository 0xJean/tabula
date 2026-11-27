package tabula

// HashTextExtended returns PostgreSQL's native seeded text hash as int8. It
// accepts the reviewed text family and an exact int8 seed, preserves SQL NULL,
// and records pg_catalog.hashtextextended(text,int8) EXECUTE. Its value is the
// server's hash, suitable for composing with AdvisoryXactLock; it is not a
// cryptographic digest or a promise of stability across PostgreSQL versions.
func HashTextExtended[T, S any](value Expr[T], seed Expr[S]) Expr[int64] {
	return checkedCall(Int8(), "hashtextextended", nullStrict, expressionImmutable, []string{"text", "int8"},
		textOperand(value.scalarOperand()), seed.scalarOperand())
}
