package plan

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/planrt"
)

// compiledShape pairs the immutable driver-verified planrt.CompiledShape with
// plan-internal parameter layout, physical occurrences, column types and shape
// metadata.
type compiledShape struct {
	compiled *planrt.CompiledShape
	sources  []*occurrence
	layout   *parameterLayout
	shape    tabula.StatementShape
	columns  map[physicalColumn]catalogType
}

func (s *compiledShape) checkSources(issuer *statementIssuer) error {
	used := make(map[string]bool)
	for _, src := range s.sources {
		if src == nil || src.issuer != issuer {
			return fmt.Errorf("tabula: shape %q refers to a foreign physical relation", s.compiled.Key)
		}
		table := src.table.SQL()
		found, updates := false, false
		for _, access := range s.shape.Access {
			if access.Relation == table {
				found = true
				if err := src.checkCommand(access.Command); err != nil {
					return err
				}
				if access.Command != "SELECT" && s.shape.Effects&tabula.EffectWrite == 0 {
					return fmt.Errorf("tabula: shape %q requires a write effect for %s", s.compiled.Key, access.Command)
				}
				updates = updates || access.Command == "UPDATE"
				if src.mode == tabula.WriteInsertOnly && len(s.shape.Result) > 0 &&
					(len(s.compiled.WriteEffects.Writes) == 0 || s.compiled.WriteEffects.Returns(src.table)) {
					return fmt.Errorf("%w: insert-only %s cannot return rows", tabula.ErrOperationDenied, table)
				}
			}
		}
		if !found {
			return fmt.Errorf("tabula: shape %q is missing access metadata for %s", s.compiled.Key, table)
		}
		used[table] = true
		if slices.Contains(s.shape.AnyColumnUpdate, table) {
			if err := src.checkCommand("UPDATE"); err != nil {
				return err
			}
		}
		if updates && src.mode == tabula.WriteUpdateOnly {
			if err := src.checkUpdateGrants(s.shape.Privileges); err != nil {
				return err
			}
		}
	}
	for _, access := range s.shape.Access {
		if !used[access.Relation] {
			return fmt.Errorf("tabula: shape %q names an undeclared relation %s", s.compiled.Key, access.Relation)
		}
	}
	return nil
}

func (src *occurrence) checkUpdateGrants(grants []tabula.DefinedPrivilege) error {
	table := src.table.SQL()
	found := false
	for _, grant := range grants {
		if grant.Table != table || grant.Privilege != "UPDATE" {
			continue
		}
		if len(grant.Columns) == 0 {
			return fmt.Errorf("%w: UPDATE on %s requires column-level grants", tabula.ErrOperationDenied, table)
		}
		for _, col := range grant.Columns {
			if !src.canUpdate(col) {
				return fmt.Errorf("%w: UPDATE on %s.%s", tabula.ErrOperationDenied, table, typedQuote(col))
			}
		}
		found = true
	}
	if !found {
		return fmt.Errorf("%w: missing UPDATE columns for %s", tabula.ErrOperationDenied, table)
	}
	return nil
}

func registerShapes[T, S any](owner *descriptor[T, S], planName string, shapes []*compiledShape) error {
	if owner == nil || owner.Entry == nil || planName == "" || len(shapes) == 0 {
		return fmt.Errorf("tabula: a registered owner, plan name and compiled shape are required")
	}
	specs := make([]planrt.ShapeRegistration, len(shapes))
	for i, s := range shapes {
		if s == nil || s.compiled == nil || s.layout == nil || s.compiled.Key == "" || s.compiled.SQL == "" || s.shape.Effects == 0 || s.compiled.Authority != nil {
			return fmt.Errorf("tabula: incomplete shape in plan %q", planName)
		}
		shape := s
		specs[i] = planrt.ShapeRegistration{
			Compiled: shape.compiled,
			Shape:    shape.shape,
			Columns:  shape.columns,
			Check:    shape.checkSources,
		}
	}
	return owner.RegisterShapes(planName, specs)
}

func bindShapeCall[T, S any](
	owner *descriptor[T, S],
	shape *compiledShape,
	s tabula.Session[S],
	method tabula.CallMethod,
	values []Binding,
	quiet, allSecret bool,
) (tabula.AuthorizedStatement, error) {
	if owner == nil || shape == nil || shape.compiled == nil || shape.layout == nil {
		return tabula.AuthorizedStatement{}, tabula.ErrStatementAuthority
	}
	if err := owner.ValidateAuth(s, shape.compiled, uint8(method), shape.layout.scoped); err != nil {
		return tabula.AuthorizedStatement{}, err
	}
	var scope any
	if shape.layout.scoped {
		var err error
		scope, err = owner.EncodeScope(s, shape.layout.scopeType)
		if err != nil {
			return tabula.AuthorizedStatement{}, err
		}
	}
	args, err := shape.layout.bind(scope, values)
	if err != nil {
		return tabula.AuthorizedStatement{}, err
	}
	if owner.AdaptArray != nil {
		if shape.layout.scoped && shape.layout.scopeType.Element != nil {
			args[0] = owner.AdaptArray(args[0])
		}
		for _, slot := range shape.layout.order {
			if slot.kind.Element != nil {
				i := shape.layout.positions[slot]
				args[i] = owner.AdaptArray(args[i])
			}
		}
	}
	secret := make([]bool, len(shape.shape.Inputs))
	for i, input := range shape.shape.Inputs {
		secret[i] = input.Secret
	}
	raw, err := owner.Authorize(s, shape.compiled, uint8(method), args, secret, quiet, allSecret, shape.layout.scoped)
	if err != nil {
		return tabula.AuthorizedStatement{}, err
	}
	return raw.(tabula.AuthorizedStatement), nil
}

// finishShape freezes the parameter order and the data supplied by the
// renderer. scopeType is needed only for a shape with a real session slot.
func (c *typedCompiler) finishShape(key, query string, scopeType catalogType, meta tabula.StatementShape) (*compiledShape, error) {
	if key == "" || query == "" || meta.Effects == 0 {
		return nil, fmt.Errorf("tabula: incomplete typed statement")
	}
	if c.params.scoped && scopeType.GoType == nil {
		return nil, fmt.Errorf("tabula: shape %q needs the PostgreSQL type of its scope column", key)
	}
	meta.UnscopedOnly = false
	schemas, forced := slices.Clone(meta.SchemaUsage), slices.Clone(meta.ForceRLS)
	if c.extendedForm || len(c.functions) > 0 {
		for _, a := range c.legacyAggregates {
			signature := a.knownLegacySignature()
			if signature == "" {
				return nil, fmt.Errorf("tabula: shape %q requires a known pg_catalog.%s overload for %s", key, a.fn, a.args[0].kind.Qualified())
			}
			c.requireNative(signature)
		}
	}
	if len(c.functions) > 0 {
		schemas = append(schemas, "pg_catalog")
		for _, signature := range slices.Sorted(maps.Keys(c.functions)) {
			meta.Privileges = append(meta.Privileges, tabula.DefinedPrivilege{Function: signature, Privilege: "EXECUTE"})
		}
	}
	columnTypes := maps.Clone(c.reads)
	for _, src := range c.from {
		schemas = append(schemas, src.table.Schema)
		switch src.scope {
		case tabula.ByRLS():
			forced = append(forced, src.table.SQL())
		case tabula.ByScopeColumn():
			if c.params.scoped && !c.sources[src] {
				return nil, fmt.Errorf("tabula: missing scope predicate for relation %q", src.alias)
			}
			meta.UnscopedOnly = !c.params.scoped
		}
	}
	for _, slot := range c.params.order {
		schemas = append(schemas, slot.kind.Schema)
	}
	if c.params.scoped {
		if !sameSQLType(c.scopeType, scopeType) {
			return nil, fmt.Errorf("tabula: scope slot type differs from its declaration")
		}
		c.params.scopeType = scopeType
		schemas = append(schemas, scopeType.Schema)
	}
	slices.Sort(schemas)
	slices.Sort(forced)
	meta.SchemaUsage, meta.ForceRLS = slices.Compact(schemas), slices.Compact(forced)
	meta.PhysicalColumns = nil
	for col, kind := range columnTypes {
		rel, name, typ, mod := col.Snapshot(kind)
		meta.PhysicalColumns = append(meta.PhysicalColumns, tabula.ShapeColumn{Relation: rel, Name: name, Type: typ, TypeMod: mod})
	}
	slices.SortFunc(meta.PhysicalColumns, func(a, b tabula.ShapeColumn) int {
		return cmp.Or(cmp.Compare(a.Relation, b.Relation), cmp.Compare(a.Name, b.Name))
	})
	meta.Inputs = nil
	if c.params.scoped {
		meta.Inputs = append(meta.Inputs, tabula.ShapeInput{Name: "scope", Type: scopeType.Qualified(), Scope: true, Secret: c.hasSecret})
	}
	for _, slot := range c.params.order {
		meta.Inputs = append(meta.Inputs, tabula.ShapeInput{Name: c.params.snapshotName(slot), Type: slot.kind.Qualified(), Secret: c.hasSecret || slot.secret})
	}
	frozen := cloneShape(meta)
	return &compiledShape{
		compiled: &planrt.CompiledShape{
			Key:          key,
			SQL:          query,
			Effects:      uint8(frozen.Effects),
			HasResult:    len(frozen.Result) != 0,
			UnscopedOnly: frozen.UnscopedOnly,
		},
		shape:   frozen,
		layout:  c.params.frozen(),
		columns: columnTypes,
		sources: slices.Clone(c.from),
	}, nil
}
