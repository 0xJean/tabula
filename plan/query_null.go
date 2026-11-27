package plan

import (
	"fmt"
	"slices"
)

// Result occurrences need their compiled outer-join nullability before an
// aggregate can choose an element codec. Physical-only checks are insufficient
// after an exported result crosses a query boundary.
func checkQueryAggregateNulls(part expressionPart, block *queryBlock) error {
	if window, ok := part.(windowExpression); ok {
		if call, ok := window.function.(aggregateCall); ok {
			call.args = slices.Clone(call.args)
			for i, arg := range call.args {
				call.args[i].nullable = queryNullable(arg.part, arg.nullable, block)
			}
			window.function = call
		}
		if err := window.validate(block.outerPhysical); err != nil {
			return err
		}
	}
	if aggregate, ok := part.(aggregateRoot); ok {
		call := aggregate.aggregateRoot()
		call.args = slices.Clone(call.args)
		for i, arg := range call.args {
			call.args[i].nullable = queryNullable(arg.part, arg.nullable, block)
		}
		return call.validate(block.outerPhysical)
	}
	traits, ok := expressionStructure(part)
	if !ok {
		return fmt.Errorf("tabula: unsupported or undeclared query expression")
	}
	for _, child := range traits.children {
		if err := checkQueryAggregateNulls(child, block); err != nil {
			return err
		}
	}
	return nil
}
