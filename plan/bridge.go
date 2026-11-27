package plan

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/planrt"
)

// Internal aliases to planrt so moved plan files stay concise while public
// signatures expose tabula.* types directly.
type (
	catalogType             = planrt.CatalogType
	physicalName            = planrt.PhysicalName
	physicalColumn          = planrt.PhysicalColumn
	statementIssuer         = planrt.Issuer
	registration            = planrt.EntryID
	statementWrite          = planrt.WriteEffect
	statementEffects        = planrt.StatementEffects
	statementEffectIdentity = planrt.EffectIdentity
	stmtMeta                = planrt.Meta
	descriptor[T, S any]    = planrt.Owner[T, S]
	column[T any]           = planrt.Column[T]
)

// Bool declares PostgreSQL boolean.
func Bool() tabula.PGType[bool] { return tabula.CatalogType[bool]("pg_catalog", "bool", nil) }

// Int4 declares PostgreSQL integer.
func Int4() tabula.PGType[int32] { return tabula.CatalogType[int32]("pg_catalog", "int4", nil) }

// Int8 declares PostgreSQL bigint.
func Int8() tabula.PGType[int64] { return tabula.CatalogType[int64]("pg_catalog", "int8", nil) }

// Text declares PostgreSQL text.
func Text() tabula.PGType[string] { return tabula.CatalogType[string]("pg_catalog", "text", nil) }

// Varchar declares PostgreSQL character varying with a column length in characters.
func Varchar(n int) tabula.PGType[string] { return tabula.Varchar(n) }

// Timestamptz declares PostgreSQL timestamp with time zone.
func Timestamptz() tabula.PGType[time.Time] {
	return tabula.CatalogType[time.Time]("pg_catalog", "timestamptz", nil)
}

// ArrayOf declares a PostgreSQL array of element.
func ArrayOf[V any](element tabula.PGType[V]) tabula.PGType[[]V] { return tabula.ArrayOf(element) }

// ArrayOfNullable declares a one-dimensional array with nullable elements.
func ArrayOfNullable[V any](element tabula.PGType[V]) tabula.PGType[[]*V] {
	return tabula.ArrayOfNullable(element)
}

func typeInfo[V any](t tabula.PGType[V]) planrt.PGTypeInfo { return t.PlanType(planrt.Key()) }
func typeID[V any](t tabula.PGType[V]) catalogType         { return t.PlanType(planrt.Key()).ID }
func typeNullable[V any](t tabula.PGType[V]) bool          { return t.PlanType(planrt.Key()).Nullable }
func defaultInfo(d tabula.TypeDefault) planrt.DefaultType  { return d.PlanDefault(planrt.Key()) }

func planTable[T, K, S any](t *tabula.Table[T, K, S]) *planrt.Table[T, S] {
	if t == nil {
		return nil
	}
	return t.PlanTable(planrt.Key())
}

func planView[T, S any](v *tabula.View[T, S]) *planrt.Owner[T, S] {
	if v == nil {
		return nil
	}
	return v.PlanView(planrt.Key())
}

func relationScopeFromKind(kind uint8) tabula.RelationScope {
	switch kind {
	case 1:
		return tabula.ByScopeColumn()
	case 2:
		return tabula.ByRLS()
	case 3:
		return tabula.Global()
	default:
		return tabula.RelationScope{}
	}
}

func typedIdent(s string) bool                         { return planrt.Ident(s) }
func typedQuote(s string) string                       { return planrt.Quote(s) }
func parsePhysicalName(s string) (physicalName, error) { return planrt.ParsePhysicalName(s) }
func sameSQLType(a, b catalogType) bool                { return planrt.SameSQLType(a, b) }

func isNilBound(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

type outputLayout struct {
	entry     *registration
	secrets   []bool
	nullables []bool
}

func ownerOutputLayout[T, S any](owner *descriptor[T, S]) *outputLayout {
	if owner == nil || owner.Entry == nil {
		return nil
	}
	secrets := make([]bool, len(owner.Columns))
	nullables := make([]bool, len(owner.Columns))
	for i, c := range owner.Columns {
		secrets[i] = c.Secret
		nullables[i] = c.Nullable
	}
	return &outputLayout{entry: owner.Entry, secrets: secrets, nullables: nullables}
}

func ownerStatementMeta[T, S any](owner *descriptor[T, S], key string, opts []tabula.StatementOption) stmtMeta {
	if owner == nil || owner.StatementMeta == nil {
		return stmtMeta{Key: key}
	}
	return owner.StatementMeta(key, opts)
}

func appendKeyRequirements(dst []tabula.KeyRequirement, values ...tabula.KeyRequirement) []tabula.KeyRequirement {
	for _, v := range values {
		if !slices.ContainsFunc(dst, func(k tabula.KeyRequirement) bool {
			return k.Relation == v.Relation && slices.Equal(k.Columns, v.Columns)
		}) {
			v.Columns = slices.Clone(v.Columns)
			dst = append(dst, v)
		}
	}
	return dst
}

func cloneShape(s tabula.StatementShape) tabula.StatementShape {
	s.Inputs = slices.Clone(s.Inputs)
	for i := range s.Inputs {
		s.Inputs[i].AllowedTypes = slices.Clone(s.Inputs[i].AllowedTypes)
	}
	s.Result = slices.Clone(s.Result)
	for i := range s.Result {
		s.Result[i].AllowedTypes = slices.Clone(s.Result[i].AllowedTypes)
	}
	s.Privileges = slices.Clone(s.Privileges)
	for i := range s.Privileges {
		s.Privileges[i].Columns = slices.Clone(s.Privileges[i].Columns)
	}
	s.Access = slices.Clone(s.Access)
	s.PhysicalColumns = slices.Clone(s.PhysicalColumns)
	for i, col := range s.PhysicalColumns {
		if col.TypeMod != nil {
			mod := *col.TypeMod
			s.PhysicalColumns[i].TypeMod = &mod
		}
	}
	s.ForceRLS = slices.Clone(s.ForceRLS)
	s.SchemaUsage = slices.Clone(s.SchemaUsage)
	s.AnyColumnSelect = slices.Clone(s.AnyColumnSelect)
	s.AnyColumnUpdate = slices.Clone(s.AnyColumnUpdate)
	s.Routines = slices.Clone(s.Routines)
	s.Keys = appendKeyRequirements(nil, s.Keys...)
	s.Comparisons = slices.Clone(s.Comparisons)
	s.UniqueMatches = slices.Clone(s.UniqueMatches)
	for i := range s.UniqueMatches {
		s.UniqueMatches[i].Columns = slices.Clone(s.UniqueMatches[i].Columns)
	}
	s.Defaults = slices.Clone(s.Defaults)
	for i := range s.Defaults {
		s.Defaults[i].Columns = slices.Clone(s.Defaults[i].Columns)
		s.Defaults[i].InsertColumns = slices.Clone(s.Defaults[i].InsertColumns)
	}
	s.Conflicts = slices.Clone(s.Conflicts)
	for i := range s.Conflicts {
		s.Conflicts[i].Columns = slices.Clone(s.Conflicts[i].Columns)
	}
	s.ReturningRelations = slices.Clone(s.ReturningRelations)
	s.ResultLineage = slices.Clone(s.ResultLineage)
	return s
}

func combineStatementEffects(e, other statementEffects) (statementEffects, error) {
	out := e
	out.Writes = slices.Clone(e.Writes)
	out.Bits |= other.Bits
	out.WriteCTE = e.WriteCTE || other.WriteCTE
	out.WriteRoutine = e.WriteRoutine || other.WriteRoutine
	for _, next := range other.Writes {
		if next.ID == nil || !typedIdent(next.Target.Schema) || !typedIdent(next.Target.Name) {
			return statementEffects{}, fmt.Errorf("tabula: incomplete physical write effect")
		}
		switch next.Command {
		case "INSERT", "UPDATE", "DELETE":
		default:
			return statementEffects{}, fmt.Errorf("tabula: unsupported physical write command %q", next.Command)
		}
		reused := false
		for _, prior := range out.Writes {
			if prior.ID == next.ID {
				if prior.Target != next.Target || prior.Returning != next.Returning {
					return statementEffects{}, fmt.Errorf("tabula: inconsistent reused write effect")
				}
				if prior.Command == next.Command {
					reused = true
					break
				}
				continue
			}
			if prior.Target == next.Target {
				return statementEffects{}, fmt.Errorf("tabula: multiple writes target %s in one statement", next.Target.SQL())
			}
		}
		if !reused {
			out.Writes = append(out.Writes, next)
		}
	}
	if len(out.Writes) > 0 || out.WriteCTE || out.WriteRoutine {
		out.Bits |= uint8(tabula.EffectWrite)
	}
	return out, nil
}

func runOwnerOp[T, S any](
	ctx context.Context,
	owner *descriptor[T, S],
	s tabula.Session[S],
	action string,
	meta stmtMeta,
	call tabula.AuthorizedStatement,
	execute func(context.Context) (int64, error),
) (int64, error) {
	if owner == nil || owner.Run == nil {
		return 0, tabula.ErrStatementAuthority
	}
	return owner.Run(ctx, s, action, meta, call, execute)
}
