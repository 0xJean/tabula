package planrt

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// Ident reports whether s is a valid PostgreSQL identifier up to 63 bytes.
func Ident(s string) bool {
	return s != "" && len(s) <= 63 && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// Quote double-quotes one PostgreSQL identifier component.
func Quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// CatalogIdentifier decodes one word or quoted identifier span.
func CatalogIdentifier(token pgstmt.Span) string {
	switch token.Kind {
	case pgstmt.Word:
		return token.Text
	case pgstmt.Quoted:
		return strings.ReplaceAll(token.Text[1:len(token.Text)-1], `""`, `"`)
	default:
		return ""
	}
}

// PhysicalName is a schema-qualified PostgreSQL catalog object name.
type PhysicalName struct{ Schema, Name string }

// ParsePhysicalName parses a schema-qualified catalog table name.
func ParsePhysicalName(text string) (PhysicalName, error) {
	tokens, err := pgstmt.ScanFragment(text)
	if err != nil || len(tokens) != 3 || tokens[1].Text != "." {
		return PhysicalName{}, fmt.Errorf("tabula: typed tables need a schema-qualified catalog name")
	}
	n := PhysicalName{CatalogIdentifier(tokens[0]), CatalogIdentifier(tokens[2])}
	if !Ident(n.Schema) || !Ident(n.Name) {
		return PhysicalName{}, fmt.Errorf("tabula: invalid physical relation name")
	}
	return n, nil
}

// SQL returns the quoted schema.name form.
func (n PhysicalName) SQL() string { return Quote(n.Schema) + "." + Quote(n.Name) }

// InputEncoder wraps a custom PGType input encoding function. Identity belongs
// to the pointer so distinct closures compare distinct.
type InputEncoder struct{ Encode func(any) (any, error) }

// CatalogType is the internal representation of a PostgreSQL catalog type
// paired with its Go value type and optional input encoder.
type CatalogType struct {
	Schema, Name  string
	GoType        reflect.Type
	VarcharLength int32
	Element       *CatalogType
	Encoder       *InputEncoder
}

// Qualified returns the quoted PostgreSQL type name (with [] suffix for arrays).
func (t CatalogType) Qualified() string {
	if t.Element != nil {
		return t.Element.Qualified() + "[]"
	}
	return Quote(t.Schema) + "." + Quote(t.Name)
}

// Cast wraps value in an explicit PostgreSQL CAST to this catalog type.
func (t CatalogType) Cast(value string) string { return "CAST(" + value + " AS " + t.Qualified() + ")" }

// SameSQLType reports whether a and b name the same PostgreSQL catalog type.
func SameSQLType(a, b CatalogType) bool {
	if a.Element != nil || b.Element != nil {
		return a.Element != nil && b.Element != nil && SameSQLType(*a.Element, *b.Element)
	}
	return a.GoType != nil && b.GoType != nil && a.Schema == b.Schema && a.Name == b.Name
}

// CompatibleColumn reports whether two column declarations agree on type and modifier.
func (t CatalogType) CompatibleColumn(other CatalogType) bool {
	return SameSQLType(t, other) && (t.VarcharLength == 0 || other.VarcharLength == 0 || t.VarcharLength == other.VarcharLength)
}

// EncodeValue encodes and snapshots one bound value according to t.
func (t CatalogType) EncodeValue(value any) (any, error) {
	if t.Encoder != nil {
		encoded, err := t.Encoder.Encode(value)
		if err != nil {
			return nil, err
		}
		value = encoded
	}
	return t.Snapshot(value), nil
}

// EncodeScope encodes a session scope value, unwrapping a pointer scope when
// the catalog type is declared for its element type.
func (t CatalogType) EncodeScope(scope any) (any, error) {
	v := reflect.ValueOf(scope)
	if v.Kind() == reflect.Pointer && v.Type().Elem() == t.GoType {
		if v.IsNil() {
			return nil, nil
		}
		scope = v.Elem().Interface()
	}
	return t.EncodeValue(scope)
}

// Snapshot defensively copies byte slices and one-dimensional array slices so
// caller mutations and driver array adapters never alias canonical values.
func (t CatalogType) Snapshot(value any) any {
	if t.Element == nil && (t.Schema != "pg_catalog" || t.Name != "bytea") {
		return value
	}
	v := reflect.ValueOf(value)
	if v.Kind() != reflect.Slice {
		return value
	}
	e := v.Type().Elem()
	if t.Element == nil && e.Kind() != reflect.Uint8 {
		return value
	}
	if v.IsNil() {
		if t.Element == nil {
			return nil
		}
		return value // array adapters accept a typed nil slice
	}
	clone := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
	reflect.Copy(clone, v)
	if e.Kind() == reflect.Pointer {
		for i := range clone.Len() {
			item := clone.Index(i)
			if item.IsNil() {
				continue
			}
			ptr := reflect.New(e.Elem())
			ptr.Elem().Set(item.Elem())
			if elem := ptr.Elem(); elem.Kind() == reflect.Slice && elem.Type().Elem().Kind() == reflect.Uint8 {
				elem.SetBytes(bytes.Clone(elem.Bytes()))
			}
			item.Set(ptr)
		}
	}
	if e.Kind() == reflect.Slice && e.Elem().Kind() == reflect.Uint8 {
		for i := range clone.Len() {
			raw := clone.Index(i)
			raw.SetBytes(bytes.Clone(raw.Bytes()))
		}
	}
	return clone.Interface()
}

// PhysicalColumn identifies one column on one physical table.
type PhysicalColumn struct {
	Table PhysicalName
	Name  string
}

// Record stores the strongest compatible catalog type for col in types.
func (col PhysicalColumn) Record(types map[PhysicalColumn]CatalogType, kind CatalogType) error {
	if prior, ok := types[col]; ok {
		if !prior.CompatibleColumn(kind) {
			return fmt.Errorf("tabula: conflicting PostgreSQL types for %s.%s", col.Table.SQL(), Quote(col.Name))
		}
		if kind.VarcharLength == 0 {
			kind.VarcharLength = prior.VarcharLength
		}
	}
	types[col] = kind
	return nil
}

// Snapshot returns the relation, column name, qualified type and optional typmod.
func (col PhysicalColumn) Snapshot(kind CatalogType) (relation, name, typ string, typeMod *int32) {
	relation, name, typ = col.Table.SQL(), col.Name, kind.Qualified()
	if kind.VarcharLength != 0 {
		// PostgreSQL stores the varchar limit plus its four-byte varlena header.
		mod := kind.VarcharLength + 4
		typeMod = &mod
	}
	return relation, name, typ, typeMod
}
