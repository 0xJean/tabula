package pgtype

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/0xJean/tabula"
)

// JSON declares a PostgreSQL json value. Bindings marshal structured values
// and strings using encoding/json. A json.RawMessage is a serialized document.
// Nil pointers, maps, slices and interfaces bind SQL NULL. A raw document
// containing null binds JSON null.
func JSON[V any]() tabula.PGType[V] { return catalog[V]("json").WithEncoder(encodeJSON[V]) }

// JSONB declares a PostgreSQL jsonb value with the same input rules as JSON.
func JSONB[V any]() tabula.PGType[V] { return catalog[V]("jsonb").WithEncoder(encodeJSON[V]) }

func encodeJSON[V any](value V) (any, error) {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Invalid:
		return nil, nil
	case reflect.Map, reflect.Slice, reflect.Pointer:
		if v.IsNil() {
			return nil, nil
		}
	}
	if raw, ok := any(value).(json.RawMessage); ok {
		if !json.Valid(raw) {
			return nil, fmt.Errorf("pgtype: invalid JSON document")
		}
		return string(raw), nil
	}
	document, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(document), nil
}
