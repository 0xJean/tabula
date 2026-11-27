package plan

// Host returns the address of an exact PostgreSQL inet value as text, without
// its network prefix. It accepts either inet Go mapping and preserves SQL NULL.
// The native pg_catalog.host(inet) EXECUTE requirement is recorded in the plan.
func Host[V any](value Expr[V]) Expr[string] {
	n := scalarNode{name: "host", args: scalarArgs(value), functions: []string{"pg_catalog.host(inet)"}}
	if !nativeType(typeID(value.kind), "inet") {
		n.err = scalarError("host", "an exact pg_catalog.inet operand", n.args)
	}
	return scalarResult(Text(), n)
}
