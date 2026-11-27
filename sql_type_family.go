package tabula

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"time"
)

// TypeFamily pairs a Go value type with the PostgreSQL catalog type names
// considered compatible during DefineSQL, DefineSQLExec, DefineSQLReturning and
// DefineSQLList verification. Construct it with TypeFamilyOf.
type TypeFamily struct {
	goType  reflect.Type
	pgTypes []string
}

// TypeFamilyOf declares the PostgreSQL type names compatible with Go type T
// (for example "uuid", "numeric", or "public.item_status"). Pointer fields *T
// automatically inherit T's family unless overridden separately.
func TypeFamilyOf[T any](pgTypes ...string) TypeFamily {
	normalized, err := normalizePGTypeNames(pgTypes)
	if err != nil {
		panic("tabula.TypeFamilyOf: " + err.Error())
	}
	return TypeFamily{goType: reflect.TypeFor[T](), pgTypes: normalized}
}

// RegisterTypeFamily registers the PostgreSQL type names compatible with Go
// type T on r for DefineSQL, DefineSQLExec, DefineSQLReturning and
// DefineSQLList verification. Call during initialization before DefineSQL*.
// Registering T also covers *T unless *T is registered separately.
func RegisterTypeFamily[T, S any](r *Registry[S], pgTypes ...string) {
	if r == nil {
		panic("tabula.RegisterTypeFamily: registry is required")
	}
	normalized, err := normalizePGTypeNames(pgTypes)
	if err != nil {
		panic(r.prefix + ".RegisterTypeFamily: " + err.Error())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.typeFamilies == nil {
		r.typeFamilies = make(map[reflect.Type][]string)
	}
	r.typeFamilies[reflect.TypeFor[T]()] = normalized
}

func registryTypeFamilies(families []TypeFamily) map[reflect.Type][]string {
	if len(families) == 0 {
		return nil
	}
	out := make(map[reflect.Type][]string, len(families))
	for _, f := range families {
		if f.goType == nil || len(f.pgTypes) == 0 {
			panic("tabula.NewRegistry: TypeFamilies requires entries created with TypeFamilyOf")
		}
		if _, dup := out[f.goType]; dup {
			panic(fmt.Sprintf("tabula.NewRegistry: duplicate type family for %s", f.goType))
		}
		out[f.goType] = slices.Clone(f.pgTypes)
	}
	return out
}

func normalizePGTypeNames(pgTypes []string) ([]string, error) {
	if len(pgTypes) == 0 {
		return nil, fmt.Errorf("at least one PostgreSQL type name is required")
	}
	out := make([]string, 0, len(pgTypes))
	seen := make(map[string]bool, len(pgTypes))
	for _, raw := range pgTypes {
		name := strings.TrimSpace(raw)
		if name == "" || strings.ContainsRune(name, 0) {
			return nil, fmt.Errorf("invalid PostgreSQL type name %q", raw)
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out, nil
}

type sqlTypeDirection uint8

const (
	sqlTypeResult sqlTypeDirection = iota
	sqlTypeInput
)

func (r *Registry[S]) allowedTypes(ft reflect.Type, isJSON, isArray bool, dir sqlTypeDirection) ([]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.allowedTypesLocked(ft, isJSON, isArray, dir)
}

func (r *Registry[S]) allowedTypesLocked(ft reflect.Type, isJSON, isArray bool, dir sqlTypeDirection) ([]string, error) {
	if isJSON {
		return []string{"json", "jsonb"}, nil
	}
	if isArray {
		if ft.Kind() != reflect.Slice {
			return nil, fmt.Errorf("array option requires a slice type, got %s", ft)
		}
		if fam, ok := r.typeFamilies[ft]; ok {
			return slices.Clone(fam), nil
		}
		if def, ok := r.defaultTypes[ft]; ok {
			return []string{def.kind.qualified()}, nil
		}
		elem := ft.Elem()
		if elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		elemTypes, err := r.allowedTypesLocked(elem, false, false, dir)
		if err != nil {
			return nil, err
		}
		out := make([]string, len(elemTypes))
		for i, t := range elemTypes {
			out[i] = t + "[]"
		}
		return out, nil
	}
	if fam, ok := r.typeFamilies[ft]; ok {
		return slices.Clone(fam), nil
	}
	if ft.Kind() == reflect.Pointer {
		if fam, ok := r.typeFamilies[ft.Elem()]; ok {
			return slices.Clone(fam), nil
		}
	}
	if def, ok := r.defaultTypes[ft]; ok {
		return []string{def.kind.qualified()}, nil
	}
	if ft.Kind() == reflect.Pointer {
		if def, ok := r.defaultTypes[ft.Elem()]; ok {
			return []string{def.kind.qualified()}, nil
		}
	}

	base := ft
	if base.Kind() == reflect.Pointer {
		base = base.Elem()
	}
	switch base {
	case reflect.TypeFor[json.RawMessage]():
		return []string{"json", "jsonb"}, nil
	case reflect.TypeFor[time.Time](), reflect.TypeFor[sql.NullTime]():
		return []string{"timestamp with time zone", "timestamp without time zone", "date"}, nil
	case reflect.TypeFor[netip.Addr]():
		return []string{"inet"}, nil
	case reflect.TypeFor[netip.Prefix]():
		return []string{"cidr", "inet"}, nil
	case reflect.TypeFor[sql.NullString]():
		return []string{"text", "character varying", "character", "name", "citext"}, nil
	case reflect.TypeFor[sql.NullBool]():
		return []string{"boolean"}, nil
	case reflect.TypeFor[sql.NullInt16](), reflect.TypeFor[sql.NullByte]():
		if dir == sqlTypeInput {
			return []string{"smallint", "integer", "bigint"}, nil
		}
		return []string{"smallint"}, nil
	case reflect.TypeFor[sql.NullInt32]():
		if dir == sqlTypeInput {
			return []string{"smallint", "integer", "bigint"}, nil
		}
		return []string{"smallint", "integer"}, nil
	case reflect.TypeFor[sql.NullInt64]():
		return []string{"smallint", "integer", "bigint"}, nil
	case reflect.TypeFor[sql.NullFloat64]():
		return []string{"real", "double precision"}, nil
	}
	if base.Kind() == reflect.Struct && base.PkgPath() == "database/sql" && strings.HasPrefix(base.Name(), "Null[") {
		if vf, ok := base.FieldByName("V"); ok {
			return r.allowedTypesLocked(vf.Type, false, false, dir)
		}
	}
	switch base.Kind() {
	case reflect.String:
		return []string{"text", "character varying", "character", "name", "citext"}, nil
	case reflect.Bool:
		return []string{"boolean"}, nil
	case reflect.Int8, reflect.Int16:
		if dir == sqlTypeInput {
			return []string{"smallint", "integer", "bigint"}, nil
		}
		return []string{"smallint"}, nil
	case reflect.Int32:
		if dir == sqlTypeInput {
			return []string{"smallint", "integer", "bigint"}, nil
		}
		return []string{"smallint", "integer"}, nil
	case reflect.Int, reflect.Int64:
		return []string{"smallint", "integer", "bigint"}, nil
	case reflect.Uint8, reflect.Uint16:
		if dir == sqlTypeInput {
			return []string{"smallint", "integer", "bigint"}, nil
		}
		return []string{"smallint", "integer"}, nil
	case reflect.Uint32:
		return []string{"oid", "smallint", "integer", "bigint"}, nil
	case reflect.Uint, reflect.Uint64:
		return []string{"smallint", "integer", "bigint"}, nil
	case reflect.Float32, reflect.Float64:
		return []string{"real", "double precision"}, nil
	case reflect.Slice:
		if base.Elem().Kind() == reflect.Uint8 {
			return []string{"bytea"}, nil
		}
	}
	return nil, fmt.Errorf("type %s has no registered PostgreSQL type family (use RegisterTypeFamily or RegistryConfig.DefaultTypes)", ft)
}

func (r *Registry[S]) scopeAllowedTypes() ([]string, error) {
	types, err := r.allowedTypes(r.scopeType, false, false, sqlTypeInput)
	if err == nil {
		return types, nil
	}
	if r.scopeID != nil {
		var zero S
		if v := r.scopeID(zero); v != nil {
			if derived, derr := r.allowedTypes(reflect.TypeOf(v), false, false, sqlTypeInput); derr == nil {
				return derived, nil
			}
		}
	}
	return nil, fmt.Errorf("scope type %s: %w", r.scopeType, err)
}
