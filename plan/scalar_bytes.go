package plan

import "github.com/0xJean/tabula"

// EncodeFormat selects one of PostgreSQL's built-in byte encodings.
type EncodeFormat uint8

// Formats supported by PostgreSQL 14 and later.
const (
	Hex EncodeFormat = iota + 1
	Base64
	Escape
)

// ConvertTo encodes text using a bound PostgreSQL encoding name. The native
// overload takes name, so the compiler explicitly casts the encoding operand.
func ConvertTo[V ~string](value Expr[V], encoding Expr[string]) Expr[[]byte] {
	arg := textOperand(encoding.scalarOperand())
	kind := typeID(tabula.CatalogType[string]("pg_catalog", "name", nil))
	encoded := scalarOperand{part: scalarNode{syntax: scalarCast, name: "encoding name", result: kind, args: []scalarOperand{arg}}, kind: kind, nullable: arg.nullable}
	return checkedCall(tabula.CatalogType[[]byte]("pg_catalog", "bytea", nil), "convert_to", nullStrict, expressionStable,
		[]string{"text", "name"}, textOperand(value.scalarOperand()), encoded)
}

// Encode renders bytea in the selected closed format. SQL NULL remains NULL.
func Encode(value Expr[[]byte], format EncodeFormat) Expr[string] {
	name := ""
	switch format {
	case Hex:
		name = "hex"
	case Base64:
		name = "base64"
	case Escape:
		name = "escape"
	}
	v := checkedCall(Text(), "encode", nullStrict, expressionImmutable, []string{"bytea", "text"}, value.scalarOperand(), grammarText(name))
	if name == "" {
		n := v.part.(scalarNode)
		n.err = scalarError("encode", "Hex, Base64 or Escape", n.args)
		v.part = n
	}
	return v
}

// SHA256 hashes bytea using PostgreSQL's core sha256(bytea) function.
func SHA256(value Expr[[]byte]) Expr[[]byte] {
	return checkedCall(tabula.CatalogType[[]byte]("pg_catalog", "bytea", nil), "sha256", nullStrict, expressionImmutable,
		[]string{"bytea"}, value.scalarOperand())
}
