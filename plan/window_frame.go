package plan

import (
	"fmt"
	"strings"
)

// FrameMode selects the native window-frame unit.
type FrameMode uint8

const (
	// FrameRows counts individual rows in the partition's window order.
	FrameRows FrameMode = iota + 1
	// FrameRange groups peers; offsets use one native ordering value.
	FrameRange
	// FrameGroups counts peer groups and requires a window ORDER BY.
	FrameGroups
)

// FrameExclusion removes rows from the frame after determining its bounds.
type FrameExclusion uint8

const (
	// ExcludeNoOthers is the default: retain all rows within the bounds.
	ExcludeNoOthers FrameExclusion = iota
	// ExcludeCurrentRow removes only the current row.
	ExcludeCurrentRow
	// ExcludeGroup removes the current row and its peers.
	ExcludeGroup
	// ExcludeTies removes peers but retains the current row when within bounds.
	ExcludeTies
)

// WindowFrame declares an explicit BETWEEN frame. A zero value preserves the
// PostgreSQL default. Dynamic negative or NULL offsets retain PostgreSQL's
// native error; offsets never interpolate a value into SQL. ROWS/GROUPS use an
// exact int8 expression; RANGE uses a reviewed native ordering/offset pair.
type WindowFrame struct {
	Mode    FrameMode
	Start   FrameBound
	End     FrameBound
	Exclude FrameExclusion
}

// FrameBound is a sealed window-frame endpoint; construct it with one of the
// bound functions. Offsets cannot refer to rows, subqueries, aggregates or windows.
type FrameBound struct {
	kind  uint8
	value scalarOperand
}

const (
	boundUnboundedPreceding uint8 = iota + 1
	boundPreceding
	boundCurrent
	boundFollowing
	boundUnboundedFollowing
)

// UnboundedPreceding starts at the first partition row.
func UnboundedPreceding() FrameBound { return FrameBound{kind: boundUnboundedPreceding} }

// Preceding declares a typed nonnegative frame distance before the current row.
func Preceding[V any](offset Expr[V]) FrameBound {
	return FrameBound{kind: boundPreceding, value: offset.scalarOperand()}
}

// CurrentRow selects the current row, or its peer boundary for RANGE/GROUPS.
func CurrentRow() FrameBound { return FrameBound{kind: boundCurrent} }

// Following declares a typed nonnegative frame distance after the current row.
func Following[V any](offset Expr[V]) FrameBound {
	return FrameBound{kind: boundFollowing, value: offset.scalarOperand()}
}

// UnboundedFollowing ends at the last partition row.
func UnboundedFollowing() FrameBound { return FrameBound{kind: boundUnboundedFollowing} }

// WholePartition explicitly includes every row of the partition.
func WholePartition() WindowFrame {
	return WindowFrame{Mode: FrameRows, Start: UnboundedPreceding(), End: UnboundedFollowing()}
}

// RunningRows includes the partition's rows through the current ordered row.
func RunningRows() WindowFrame {
	return WindowFrame{Mode: FrameRows, Start: UnboundedPreceding(), End: CurrentRow()}
}

func (f WindowFrame) identity() string {
	return fmt.Sprintf("/frame/%d/%d/%d/%d", f.Mode, f.Start.kind, f.End.kind, f.Exclude)
}

func (f WindowFrame) validate(order []RowTerm) error {
	if f.Mode == 0 {
		if f.Start.kind != 0 || f.End.kind != 0 || f.Exclude != 0 {
			return fmt.Errorf("tabula: frame bounds and exclusions require a frame mode")
		}
		return nil
	}
	if f.Mode > FrameGroups || f.Exclude > ExcludeTies || f.Start.kind < boundUnboundedPreceding || f.Start.kind >= boundUnboundedFollowing || f.End.kind < boundPreceding || f.End.kind > boundUnboundedFollowing || f.End.kind < f.Start.kind {
		return fmt.Errorf("tabula: invalid window frame mode, exclusion or bound ordering")
	}
	if f.Mode == FrameGroups && len(order) == 0 {
		return fmt.Errorf("tabula: GROUPS frame requires ORDER BY")
	}
	for _, bound := range []FrameBound{f.Start, f.End} {
		if bound.kind != boundPreceding && bound.kind != boundFollowing {
			continue
		}
		v := bound.value
		if v.part == nil || v.kind.GoType == nil {
			return fmt.Errorf("tabula: frame offset requires a declared expression")
		}
		if err := checkFrameOffset(v.part); err != nil {
			return err
		}
		if f.Mode != FrameRange {
			if !nativeType(v.kind, "int8") {
				return fmt.Errorf("tabula: ROWS and GROUPS offsets require exact int8")
			}
		} else if len(order) != 1 || windowRangeSignature(order[0].orderValue().kind, v.kind) == "" {
			return fmt.Errorf("tabula: RANGE offset requires one reviewed native order/offset pair")
		}
	}
	return nil
}

func checkFrameOffset(p expressionPart) error {
	// Only closed scalar constructors and bound values are constant relative
	// to this query's rows. Query boundaries cannot hide a row dependency.
	switch p.(type) {
	case *typedInput, scalarNode, comparison, rowNull, rowNot, rowOr, conjunction:
	default:
		return fmt.Errorf("tabula: frame offset cannot contain a row, query, aggregate or window")
	}
	t, ok := expressionStructure(p)
	if !ok {
		return fmt.Errorf("tabula: undeclared frame offset")
	}
	for _, child := range t.children {
		if err := checkFrameOffset(child); err != nil {
			return err
		}
	}
	return nil
}

func windowRangeSignature(order, offset catalogType) string {
	if order.Schema != "pg_catalog" || offset.Schema != "pg_catalog" || order.Element != nil || offset.Element != nil {
		return ""
	}
	valid := false
	switch order.Name {
	case "int2", "int4":
		valid = nativeType(offset, "int2", "int4", "int8")
	case "int8", "numeric", "float8", "interval":
		valid = sameSQLType(order, offset)
	case "float4":
		valid = nativeType(offset, "float8")
	case "date", "timestamp", "timestamptz", "time", "timetz":
		valid = nativeType(offset, "interval")
	}
	if !valid {
		return ""
	}
	name := order.Name
	return "pg_catalog.in_range(" + name + "," + name + "," + offset.Name + ",boolean,boolean)"
}

func (f WindowFrame) render(c *typedCompiler, order []RowTerm) (string, error) {
	if err := f.validate(order); err != nil || f.Mode == 0 {
		return "", err
	}
	parts := make([]string, 2)
	for i, bound := range []FrameBound{f.Start, f.End} {
		switch bound.kind {
		case boundUnboundedPreceding:
			parts[i] = "UNBOUNDED PRECEDING"
		case boundUnboundedFollowing:
			parts[i] = "UNBOUNDED FOLLOWING"
		case boundCurrent:
			parts[i] = "CURRENT ROW"
		default:
			value, err := bound.value.part.render(c)
			if err != nil {
				return "", err
			}
			if f.Mode == FrameRange {
				c.requireNative(windowRangeSignature(order[0].orderValue().kind, bound.value.kind))
			}
			parts[i] = value + " PRECEDING"
			if bound.kind == boundFollowing {
				parts[i] = value + " FOLLOWING"
			}
		}
	}
	mode := [...]string{"", "ROWS", "RANGE", "GROUPS"}[f.Mode]
	exclusion := [...]string{"", " EXCLUDE CURRENT ROW", " EXCLUDE GROUP", " EXCLUDE TIES"}[f.Exclude]
	return mode + " BETWEEN " + strings.Join(parts, " AND ") + exclusion, nil
}
