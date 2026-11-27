package plan

import "github.com/0xJean/tabula"

// Cast selects a closed PostgreSQL conversion. It never changes the Go input
// codec. Explicit targets must be unconstrained: use pgtype.Varchar() rather
// than pgtype.Varchar(n), so a cast cannot silently truncate a bound value.
func Cast[To, From any](target tabula.PGType[To], value Expr[From]) Expr[To] {
	n := scalarNode{syntax: scalarCast, name: "CAST", args: scalarArgs(value)}
	stability, ok := scalarCastSignature(typeID(value.kind), typeID(target))
	n.stability = stability
	if !ok || typeID(target).VarcharLength != 0 {
		n.err = scalarError("CAST", "a reviewed native conversion to an unconstrained catalog type (remove target modifiers)", n.args)
	}
	return scalarResult(target, n)
}

func scalarCastSignature(from, to catalogType) (expressionStability, bool) {
	if !reviewedType(from) || !reviewedType(to) {
		return expressionImmutable, false
	}
	if sameSQLType(from, to) {
		return expressionImmutable, true
	}
	if from.Element != nil && to.Element != nil {
		return scalarCastSignature(*from.Element, *to.Element)
	}
	if from.Element != nil || to.Element != nil {
		// Array I/O depends on element formatting and session settings.
		return expressionStable, nativeType(from, "text") || nativeType(to, "text")
	}
	if numericScalar(from) && numericScalar(to) {
		return expressionImmutable, true
	}
	text := func(t catalogType) bool { return nativeType(t, "text", "varchar", "bpchar") }
	if text(from) && text(to) {
		return expressionImmutable, true
	}
	if text(from) || text(to) {
		other := from
		if text(from) {
			other = to
		}
		if nativeType(other, "date", "time", "timetz", "timestamp", "timestamptz", "interval", "float4", "float8", "bytea") {
			return expressionStable, true
		}
		return expressionImmutable, true
	}
	if nativeType(from, "json", "jsonb") && nativeType(to, "json", "jsonb") {
		return expressionImmutable, true
	}
	switch from.Name + "/" + to.Name {
	case "date/timestamp", "timestamp/date", "timestamp/time", "interval/time", "time/interval", "timetz/time":
		return expressionImmutable, true
	case "date/timestamptz", "timestamp/timestamptz", "timestamptz/timestamp", "timestamptz/date", "timestamptz/time", "timestamptz/timetz", "time/timetz":
		return expressionStable, true
	}
	return expressionImmutable, false
}

func numericScalar(t catalogType) bool {
	return nativeType(t, "int2", "int4", "int8", "numeric", "float4", "float8")
}

func scalarCastFunctions(from, to catalogType) []string {
	if sameSQLType(from, to) {
		return nil
	}
	if from.Element != nil && to.Element != nil {
		return scalarCastFunctions(*from.Element, *to.Element)
	}
	if fn, ok := scalarNativeCasts[from.Name+"/"+to.Name]; ok && from.Element == nil && to.Element == nil {
		if fn == "" {
			return nil
		}
		return []string{fn}
	}
	source, target := from.Name, to.Name
	if from.Element != nil {
		source = "array"
	}
	if to.Element != nil {
		target = "array"
	}
	return []string{scalarTypeIO[source][1], scalarTypeIO[target][0]}
}
