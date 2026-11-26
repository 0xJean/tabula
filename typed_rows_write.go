package tabula

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// RowAssignment assigns a typed expression to a field on the target occurrence.
type RowAssignment struct {
	field *fieldRef
	part  expressionPart
	valid bool
}

// Assign sets a column to a compatible expression, evaluated on the old row.
func Assign[V any](field Field[V], value Expr[V]) RowAssignment {
	return RowAssignment{field.ref, value.part, sameSQLType(field.kind.id, value.kind.id) &&
		field.ref != nil && (!value.nullable || field.ref.nullable)}
}

// RowSet names a complete, ordered set of simultaneous assignments.
type RowSet struct {
	Name        string
	Assignments []RowAssignment
}

// RowVersion compares the existing version with a required input.
type RowVersion struct {
	field *fieldRef
	input *typedInput
	valid bool
}

// ExpectVersion declares a version check for every shape of a mutation.
// UpdateMany also increments each matching row's version.
func ExpectVersion[V any](field Field[V], expected Slot[V]) RowVersion {
	return RowVersion{field.ref, expected.input, sameSQLType(field.kind.id, expected.kind.id)}
}

// MutationSpec requires a fixed WHERE predicate in addition to any optional
// predicates. Versioned tables require ExpectVersion for updates and deletes.
type MutationSpec struct {
	RowFilter
	Version RowVersion
}

// MutationPlan owns one registered UpdateMany or DeleteMany family.
type MutationPlan[T, S any] struct {
	source  RowSource[T, S]
	family  *rowFamily
	command string
}

// DefineUpdateMany registers the permitted sets of assignments. It panics on declaration errors.
func DefineUpdateMany[T, S any](src RowSource[T, S], name string, spec MutationSpec, sets ...RowSet) *MutationPlan[T, S] {
	p, err := TryDefineUpdateMany(src, name, spec, sets...)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineUpdateMany atomically registers a predicate-based UPDATE.
func TryDefineUpdateMany[T, S any](src RowSource[T, S], name string, spec MutationSpec, sets ...RowSet) (*MutationPlan[T, S], error) {
	return defineRowMutation(src, name, spec, "UPDATE", sets)
}

// DefineDeleteMany registers a physical delete on a table that permits it.
// It panics on declaration errors.
func DefineDeleteMany[T, S any](src RowSource[T, S], name string, spec MutationSpec) *MutationPlan[T, S] {
	p, err := TryDefineDeleteMany(src, name, spec)
	if err != nil {
		panic(err)
	}
	return p
}

// TryDefineDeleteMany atomically registers a predicate-based DELETE.
func TryDefineDeleteMany[T, S any](src RowSource[T, S], name string, spec MutationSpec) (*MutationPlan[T, S], error) {
	return defineRowMutation(src, name, spec, "DELETE", nil)
}

func defineRowMutation[T, S any](src RowSource[T, S], name string, spec MutationSpec, command string, sets []RowSet) (*MutationPlan[T, S], error) {
	if src.owner == nil || src.rel == nil {
		return nil, fmt.Errorf("tabula: a declared mutation source is required")
	}
	if err := src.rel.checkCommand(command); err != nil {
		return nil, err
	}
	if spec.Where.part == nil {
		return nil, fmt.Errorf("tabula: a mutation requires a fixed typed predicate")
	}
	v := spec.Version
	if (src.version == "") != (v == (RowVersion{})) ||
		src.version != "" && (!v.valid || v.field == nil || v.field.source != src.rel || v.field.name != src.version || v.input == nil) {
		return nil, fmt.Errorf("%w: expected version input must match the descriptor's version field", ErrOperationDenied)
	}
	if v.field != nil && (v.field.kind.schema != "pg_catalog" || !slices.Contains([]string{"int2", "int4", "int8"}, v.field.kind.name)) {
		return nil, fmt.Errorf("tabula: expected a PostgreSQL integer version field")
	}
	forms := make(map[string][]RowAssignment)
	if command == "DELETE" {
		forms["delete"] = nil
	} else {
		for _, set := range sets {
			if !rowLabel(set.Name) || len(set.Assignments) == 0 || forms["set."+set.Name] != nil {
				return nil, fmt.Errorf("tabula: invalid or duplicate assignment set %q", set.Name)
			}
			forms["set."+set.Name] = set.Assignments
		}
	}
	fam, err := compileRowFamily(src.owner, name, src.rowSource, spec.RowFilter, slices.Sorted(maps.Keys(forms)),
		func(c *typedCompiler, form string, predicates []Pred) (string, StatementShape, error) {
			return src.renderMutation(c, command, forms[form], v, predicates)
		})
	if err != nil {
		return nil, err
	}
	return &MutationPlan[T, S]{src, fam, command}, nil
}

func (src rowSource) renderMutation(c *typedCompiler, command string, set []RowAssignment, version RowVersion, predicates []Pred) (string, StatementShape, error) {
	table := src.rel.table.sql()
	meta := StatementShape{Effects: EffectWrite, Access: []ShapeAccess{{table, command}}}
	query := "DELETE FROM " + src.fromSQL()
	if command == "UPDATE" {
		var assigns, targets []string
		for _, a := range set {
			f := a.field
			if !a.valid || f == nil || f.source != src.rel || a.part == nil || src.protected[f.name] ||
				!src.rel.canUpdate(f.name) || slices.Contains(targets, f.name) {
				return "", StatementShape{}, fmt.Errorf("%w: invalid, protected or duplicate assignment", ErrOperationDenied)
			}
			// Assignment targets contribute a physical type and secrecy, but
			// do not require SELECT unless an expression also reads them.
			col := physicalColumn{src.rel.table, f.name}
			if prior, ok := c.reads[col]; ok && !sameSQLType(prior, f.kind) {
				return "", StatementShape{}, fmt.Errorf("tabula: conflicting assignment type for %q", f.name)
			}
			c.reads[col] = f.kind
			c.hasSecret = c.hasSecret || f.secret
			if err := checkRowExpr(a.part, nil); err != nil {
				return "", StatementShape{}, err
			}
			value, err := a.part.render(c)
			if err != nil {
				return "", StatementShape{}, err
			}
			targets = append(targets, f.name)
			assigns = append(assigns, typedQuote(f.name)+" = "+value)
		}
		if version.field != nil {
			field, err := version.field.render(c)
			if err != nil {
				return "", StatementShape{}, err
			}
			targets = append(targets, src.version)
			assigns = append(assigns, typedQuote(src.version)+" = "+field+" + 1")
		}
		query = "UPDATE " + src.fromSQL() + " SET " + strings.Join(assigns, ", ")
		slices.Sort(targets)
		meta.Privileges = append(meta.Privileges, DefinedPrivilege{Table: table, Privilege: "UPDATE", Columns: targets})
	} else {
		meta.Privileges = append(meta.Privileges, DefinedPrivilege{Table: table, Privilege: "DELETE"})
	}
	terms, err := src.where(c, predicates)
	if err != nil {
		return "", StatementShape{}, err
	}
	if version.field != nil {
		field, err := version.field.render(c)
		if err != nil {
			return "", StatementShape{}, err
		}
		input, err := version.input.render(c)
		if err != nil {
			return "", StatementShape{}, err
		}
		terms = append(terms, "("+field+" = "+input+")")
	}
	if len(c.selectCols[src.rel.table]) != 0 {
		read, err := c.readRequirements()
		if err != nil {
			return "", StatementShape{}, err
		}
		meta.Access = append(meta.Access, read.Access...)
		meta.Privileges = append(meta.Privileges, read.Privileges...)
	}
	return query + whereSQL(terms), meta, nil
}
