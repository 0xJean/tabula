package tabula

import (
	"fmt"
	"reflect"
	"time"

	"github.com/0xJean/tabula/internal/planrt"
)

// RelationScope identifies the enforcement declared for a physical relation.
type RelationScope struct{ kind uint8 }

const (
	relationColumn uint8 = iota + 1
	relationRLS
	relationGlobal
)

// ByScopeColumn binds the session scope to the descriptor's tagged scope column.
func ByScopeColumn() RelationScope { return RelationScope{relationColumn} }

// ByRLS delegates scoping to PostgreSQL row-level security. A compiled shape
// records the physical relation for role-bound ENABLE and FORCE checks.
func ByRLS() RelationScope { return RelationScope{relationRLS} }

// Global declares a physical relation whose rows do not depend on session scope.
func Global() RelationScope { return RelationScope{relationGlobal} }

func (t *Table[T, K, S]) bindRelationScope(scope RelationScope) error {
	switch scope.kind {
	case 0:
		if t.scope >= 0 {
			scope = ByScopeColumn()
		}
	case relationColumn:
		if t.scope < 0 {
			return fmt.Errorf("ByScopeColumn requires a tagged scope column")
		}
	case relationGlobal:
		if t.scope >= 0 {
			return fmt.Errorf("Global conflicts with the tagged scope column")
		}
	case relationRLS:
		// A tagged scope remains available to legacy operations on this table.
	default:
		return fmt.Errorf("invalid relation scope")
	}
	t.relationScope = scope
	return nil
}

// PGType pairs a Go value type with an exact PostgreSQL catalog type.
// Its zero value cannot be used to declare a column or parameter.
type PGType[V any] struct {
	_        [0]func(V) // keep distinct Go representations non-convertible
	id       planrt.CatalogType
	nullable bool
}

// PlanType exposes catalog metadata to github.com/0xJean/tabula/plan.
func (t PGType[V]) PlanType(seal planrt.Seal) planrt.PGTypeInfo {
	if !seal.Valid() {
		panic("tabula: invalid internal bridge seal")
	}
	return planrt.PGTypeInfo{ID: t.id, Nullable: t.nullable}
}

// CatalogType declares a schema-qualified PostgreSQL type. Supply its encoding
// for a bound input, or nil when the driver already accepts V. Both names are
// exact catalog identifiers. No portion is interpreted as a SQL expression.
func CatalogType[V any](schema, name string, encode func(V) any) PGType[V] {
	if !planrt.Ident(schema) || !planrt.Ident(name) {
		panic("tabula.CatalogType: invalid schema or type name")
	}
	typ := reflect.TypeFor[V]()
	nullable := encode != nil
	switch typ.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		// A named scalar may still implement a custom encoding.
		nullable = nullable || typ.NumMethod() > 0
	default:
		nullable = nullable || typ != reflect.TypeFor[time.Time]()
	}
	t := PGType[V]{id: planrt.CatalogType{Schema: schema, Name: name, GoType: typ}, nullable: nullable}
	if encode != nil {
		return t.WithEncoder(func(v V) (any, error) { return encode(v), nil })
	}
	return t
}

// WithEncoder returns this catalog type with a replacement input encoder.
// Encoders run once when a value is bound. The encoder owns the stability of
// custom values it returns. Field decoding continues to use the descriptor's
// scanner. Joined scope columns sharing a parameter must reuse the same encoder
// declaration unless RegistryConfig.ScopeID supplies the session's scope argument.
func (t PGType[V]) WithEncoder(encode func(V) (any, error)) PGType[V] {
	if t.id.GoType == nil || encode == nil {
		panic("tabula.PGType.WithEncoder: a declared type and an encoder are required")
	}
	t.id.Encoder = &planrt.InputEncoder{Encode: func(value any) (any, error) {
		if value == nil {
			var zero V
			return encode(zero)
		}
		return encode(value.(V))
	}}
	t.nullable = true
	return t
}

// ArrayOf declares a PostgreSQL array of the given element type. Inputs use
// RegistryConfig.ArrayArg, or the driver's native slice binding when it is nil.
// The array codec is responsible for encoding its elements.
func ArrayOf[V any](element PGType[V]) PGType[[]V] {
	if element.id.GoType == nil || element.id.Element != nil {
		panic("tabula.ArrayOf: a declared scalar element type is required")
	}
	elem := element.id
	kind := planrt.CatalogType{
		Schema: element.id.Schema, GoType: reflect.TypeFor[[]V](),
		VarcharLength: element.id.VarcharLength, Element: &elem,
	}
	return PGType[[]V]{id: kind, nullable: true}
}

// ArrayOfNullable declares a one-dimensional array with nullable elements.
// A nil slice binds SQL NULL, an empty slice binds an empty array and a nil
// element binds a SQL NULL within the array.
func ArrayOfNullable[V any](element PGType[V]) PGType[[]*V] {
	kind := ArrayOf(element).id
	kind.GoType = reflect.TypeFor[[]*V]()
	return PGType[[]*V]{id: kind, nullable: true}
}

// Varchar declares PostgreSQL character varying with a column length in
// characters. Verification checks the column modifier. The parameter uses
// unbounded varchar so comparisons preserve the entire value and PostgreSQL
// applies its native length rules on assignment.
func Varchar(n int) PGType[string] {
	if n < 1 || n > 10_485_760 {
		panic("tabula.Varchar: length must be between 1 and 10485760 characters")
	}
	t := CatalogType[string]("pg_catalog", "varchar", nil)
	t.id.VarcharLength = int32(n)
	return t
}

// TypeDefault declares a registry's exact PostgreSQL type for a Go value type.
// Construct it with TypeMapping.
type TypeDefault struct {
	kind planrt.CatalogType
	typ  any // original PGType[V], including its codec and input nullability
}

// PlanDefault exposes registry default type metadata to github.com/0xJean/tabula/plan.
func (d TypeDefault) PlanDefault(seal planrt.Seal) planrt.DefaultType {
	if !seal.Valid() {
		panic("tabula: invalid internal bridge seal")
	}
	return planrt.DefaultType{Kind: d.kind, Typ: d.typ}
}

// TypeMapping retains the catalog identity and input codec of kind as a
// registry default for V. A nullable *V field can use the same declaration.
func TypeMapping[V any](kind PGType[V]) TypeDefault {
	return TypeDefault{kind: kind.id, typ: kind}
}

func registryTypeDefaults(defaults []TypeDefault) map[reflect.Type]TypeDefault {
	if len(defaults) == 0 {
		return nil
	}
	types := make(map[reflect.Type]TypeDefault, len(defaults))
	for _, d := range defaults {
		t := d.kind.GoType
		if t == nil {
			panic("tabula.NewRegistry: DefaultTypes requires declared PostgreSQL types")
		}
		if _, ok := types[t]; ok {
			panic(fmt.Sprintf("tabula.NewRegistry: duplicate default type for %s", t))
		}
		types[t] = d
	}
	return types
}
