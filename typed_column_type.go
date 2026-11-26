package tabula

import "fmt"

func (kind catalogType) compatibleColumn(other catalogType) bool {
	return sameSQLType(kind, other) && (kind.varcharLength == 0 || other.varcharLength == 0 || kind.varcharLength == other.varcharLength)
}

// Keep the strongest compatible declaration of a physical column. Slots use
// the base catalog type independently of optional column modifiers.
func (col physicalColumn) record(types map[physicalColumn]catalogType, kind catalogType) error {
	if prior, ok := types[col]; ok {
		if !prior.compatibleColumn(kind) {
			return fmt.Errorf("tabula: conflicting PostgreSQL types for %s.%s", col.table.sql(), typedQuote(col.name))
		}
		if kind.varcharLength == 0 {
			kind.varcharLength = prior.varcharLength
		}
	}
	types[col] = kind
	return nil
}

func (col physicalColumn) snapshot(kind catalogType) ShapeColumn {
	out := ShapeColumn{Relation: col.table.sql(), Name: col.name, Type: kind.qualified()}
	if kind.varcharLength != 0 {
		// PostgreSQL stores the varchar limit plus its four-byte varlena header.
		mod := kind.varcharLength + 4
		out.TypeMod = &mod
	}
	return out
}
