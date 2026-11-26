package tabula

import (
	"errors"
	"reflect"
)

type stateCarrier = interface {
	error
	SQLState() string
}

// sqlDiagnostic selects exactly one carrier in standard wrapping and join
// order. A later diagnostic must never supply a field missing from the first.
func sqlDiagnostic(err error) (stateCarrier, string) {
	if nilError(err) {
		return nil, ""
	}
	pg, ok := errors.AsType[stateCarrier](err)
	if !ok || nilError(pg) {
		return nil, ""
	}
	state := pg.SQLState()
	if len(state) != 5 {
		return nil, ""
	}
	for i := range len(state) {
		c := state[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return nil, ""
		}
	}
	return pg, state
}

func nilError(err error) bool {
	v := reflect.ValueOf(err)
	switch v.Kind() {
	case reflect.Invalid:
		return true
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	}
	return false
}

// ConstraintName returns the structured PostgreSQL constraint name from the
// diagnostic selected by SQLState. It returns "" for a missing name or state.
// It recognizes ConstraintName() string and the exported driver diagnostic
// fields Constraint and ConstraintName. The database error text is untouched.
func ConstraintName(err error) string {
	pg, _ := sqlDiagnostic(err)
	if pg == nil {
		return ""
	}
	if named, ok := pg.(interface{ ConstraintName() string }); ok {
		return named.ConstraintName()
	}
	v := reflect.Indirect(reflect.ValueOf(pg))
	if v.Kind() == reflect.Struct {
		for _, name := range [...]string{"Constraint", "ConstraintName"} {
			field, ok := v.Type().FieldByName(name)
			if ok && field.IsExported() && field.Type.Kind() == reflect.String {
				if value, err := v.FieldByIndexErr(field.Index); err == nil && value.String() != "" {
					return value.String()
				}
			}
		}
	}
	return ""
}
