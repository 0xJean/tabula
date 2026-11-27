package tabula

import (
	"fmt"

	"github.com/0xJean/tabula/internal/planrt"
)

// DefinePlanSource registers a physical table for typed plans. The source
// contributes its column and key metadata. Each plan declares its own SQL
// and privileges. Calls through Table.Bind return ErrOperationDenied.
// It panics on invalid declarations.
func DefinePlanSource[T, K, S any](r *Registry[S], spec TableSpec[T, K]) *Table[T, K, S] {
	t, err := TryDefinePlanSource(r, spec)
	if err != nil {
		panic(err)
	}
	return t
}

// TryDefinePlanSource returns a registered source ready for FromTable and
// RowsFrom. An invalid spec returns an error and leaves the registry untouched.
// WriteMode, ReadOnly, AllowDelete and UpdateColumns constrain the plans.
// Order, GroupBy and Aggregates belong on the individual plans.
func TryDefinePlanSource[T, K, S any](r *Registry[S], spec TableSpec[T, K]) (*Table[T, K, S], error) {
	t, err := compileTable(r, spec, true)
	if err != nil {
		return nil, err
	}
	t.entry = r.record(Definition{
		Name: t.name, Columns: t.definedColumns(), Statements: make(map[string]string),
		Privileges: []DefinedPrivilege{},
	}, false)
	return t, nil
}

func (spec TableSpec[T, K]) checkPlanSource() error {
	if _, err := planrt.ParsePhysicalName(spec.Name); err != nil {
		return err
	}
	if spec.Order != "" || len(spec.GroupBy) != 0 {
		return fmt.Errorf("%w: declare ordering and grouping on a typed plan", ErrOperationDenied)
	}
	for range spec.Aggregates.columns {
		return fmt.Errorf("%w: declare aggregates on a typed plan", ErrOperationDenied)
	}
	return nil
}
