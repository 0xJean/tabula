package tabula

import "slices"

func queryWindowNullable(n windowExpression, block *queryBlock) bool {
	switch function := n.function.(type) {
	case windowFunction:
		function.args = slices.Clone(function.args)
		for i, arg := range function.args {
			function.args[i].nullable = queryNullable(arg.part, arg.nullable, block)
		}
		return function.nullableAfterJoin(block.outerPhysical)
	case aggregateCall:
		return function.nullableAfterJoin(block.outerPhysical)
	}
	return true
}
