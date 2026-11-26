package tabula

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
)

// ShapeEffects describes what a compiled statement can do on its session.
type ShapeEffects uint8

const (
	// EffectRead marks a statement that observes rows.
	EffectRead ShapeEffects = 1 << iota
	// EffectWrite marks a statement that can modify data, including through a routine.
	EffectWrite
	// EffectRowLock marks a row-locking read that needs a writable transaction.
	EffectRowLock
	// EffectAdvisoryLock marks an operation whose advisory lock needs a transaction.
	EffectAdvisoryLock
)

// ShapeField describes a result column in projection order.
type ShapeField struct {
	Name, Type string
	Nullable   bool
}

// ShapeInput identifies a numbered parameter. Scope marks a parameter supplied
// by the session. Values are kept outside all snapshots.
type ShapeInput struct {
	Name, Type string
	Scope      bool
	// Secret requests redaction of this bind value by observers.
	Secret bool
}

// ShapeAccess identifies one physical relation and command for policy checks.
type ShapeAccess struct{ Relation, Command string }

// ShapeColumn records the declared PostgreSQL type of a referenced physical
// column for catalog verification. Relation is its quoted, qualified table.
type ShapeColumn struct {
	Relation, Name, Type string
	// TypeMod is the expected pg_attribute.atttypmod when the declaration
	// specifies a modifier. Nil requests verification of the type OID alone.
	TypeMod *int32 `json:",omitempty"`
}

// StatementShape describes the inputs, results, effects and minimum grants for
// one SQL string in Definition.Statements under the same key.
type StatementShape struct {
	Inputs          []ShapeInput
	Result          []ShapeField
	Effects         ShapeEffects
	Privileges      []DefinedPrivilege
	Access          []ShapeAccess
	PhysicalColumns []ShapeColumn
	ForceRLS        []string
	SchemaUsage     []string
	AnyColumnSelect []string
	AnyColumnUpdate []string
	// Routines records the catalog contracts of called PostgreSQL functions.
	Routines []RoutineRequirement
	// UnscopedOnly marks a shape that omits a declared column-scope predicate.
	// It can be bound only to an unscoped session.
	UnscopedOnly bool
}

// The renderer produces these values, including all applicable scope and
// statement effects, before a registry can issue an executable call.
type compiledShape struct {
	observationKey string
	tableName      string
	action         string
	fixedMethod    CallMethod
	sources        []*occurrence
	authority      *statementIssuer
	key            string
	sql            string
	layout         *parameterLayout
	shape          StatementShape
	columns        map[physicalColumn]catalogType
}

type physicalColumn struct {
	table physicalName
	name  string
}

func cloneShape(s StatementShape) StatementShape {
	s.Inputs = slices.Clone(s.Inputs)
	s.Result = slices.Clone(s.Result)
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
	return s
}

func cloneShapeMap(m map[string]StatementShape) map[string]StatementShape {
	out := maps.Clone(m)
	for key, s := range out {
		out[key] = cloneShape(s)
	}
	return out
}

// Check the recorded physical commands against the same table declarations
// that authorize bound Rows. Registration is the final check before any
// session can receive an executable call.
func (s *compiledShape) checkSources(issuer *statementIssuer) error {
	used := make(map[string]bool)
	for _, src := range s.sources {
		if src == nil || src.issuer != issuer {
			return fmt.Errorf("tabula: shape %q refers to a foreign physical relation", s.key)
		}
		table := src.table.sql()
		found, updates := false, false
		for _, access := range s.shape.Access {
			if access.Relation == table {
				found = true
				if err := src.checkCommand(access.Command); err != nil {
					return err
				}
				if access.Command != "SELECT" && s.shape.Effects&EffectWrite == 0 {
					return fmt.Errorf("tabula: shape %q requires a write effect for %s", s.key, access.Command)
				}
				updates = updates || access.Command == "UPDATE"
				if src.mode == WriteInsertOnly && len(s.shape.Result) > 0 {
					return fmt.Errorf("%w: insert-only %s cannot return rows", ErrOperationDenied, table)
				}
			}
		}
		if !found {
			return fmt.Errorf("tabula: shape %q is missing access metadata for %s", s.key, table)
		}
		used[table] = true
		if slices.Contains(s.shape.AnyColumnUpdate, table) {
			if err := src.checkCommand("UPDATE"); err != nil {
				return err
			}
		}
		if updates && src.mode == WriteUpdateOnly {
			if err := src.checkUpdateGrants(s.shape.Privileges); err != nil {
				return err
			}
		}
	}
	for _, access := range s.shape.Access {
		if !used[access.Relation] {
			return fmt.Errorf("tabula: shape %q names an undeclared relation %s", s.key, access.Relation)
		}
	}
	return nil
}

func (src *occurrence) checkUpdateGrants(grants []DefinedPrivilege) error {
	table := src.table.sql()
	found := false
	for _, grant := range grants {
		if grant.Table != table || grant.Privilege != "UPDATE" {
			continue
		}
		if len(grant.Columns) == 0 {
			return fmt.Errorf("%w: UPDATE on %s requires column-level grants", ErrOperationDenied, table)
		}
		for _, col := range grant.Columns {
			if !src.canUpdate(col) {
				return fmt.Errorf("%w: UPDATE on %s.%s", ErrOperationDenied, table, typedQuote(col))
			}
		}
		found = true
	}
	if !found {
		return fmt.Errorf("%w: missing UPDATE columns for %s", ErrOperationDenied, table)
	}
	return nil
}

// registerShapes publishes the complete family under one registry lock. It
// checks all keys and all physical column types before changing either index.
func (d *descriptor[T, S]) registerShapes(plan string, shapes []*compiledShape) error {
	if d.entry == nil || plan == "" || len(shapes) == 0 {
		return fmt.Errorf("tabula: a registered owner, plan name and compiled shape are required")
	}
	r := d.reg
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := &d.entry.def
	for _, owner := range entry.StatementQueries {
		if owner == plan {
			return fmt.Errorf("tabula: %s plan %q already registered", d.name, plan)
		}
	}
	keys := make(map[string]bool)
	types := make(map[physicalColumn]catalogType)
	for _, s := range shapes {
		if s == nil || s.key == "" || s.sql == "" || s.layout == nil || s.shape.Effects == 0 || s.authority != nil {
			return fmt.Errorf("tabula: incomplete shape in plan %q", plan)
		}
		if err := s.checkSources(r.statementIssuer); err != nil {
			return err
		}
		_, registered := entry.Statements[s.key]
		if keys[s.key] || registered {
			return fmt.Errorf("tabula: %s.%s already registered", d.name, s.key)
		}
		keys[s.key] = true
		for col, kind := range s.columns {
			if old, ok := r.typedColumns[col]; ok {
				if err := col.record(types, old); err != nil {
					return err
				}
			}
			if err := col.record(types, kind); err != nil {
				return err
			}
		}
	}
	if r.typedColumns == nil {
		r.typedColumns = make(map[physicalColumn]catalogType)
	}
	maps.Copy(r.typedColumns, types)
	if entry.StatementQueries == nil {
		entry.StatementQueries = make(map[string]string)
	}
	if entry.StatementShapes == nil {
		entry.StatementShapes = make(map[string]StatementShape)
	}
	for _, s := range shapes {
		s.authority = r.statementIssuer
		s.observationKey = d.name + "." + s.key
		s.tableName, s.action = d.name, plan
		entry.Statements[s.key] = s.sql
		entry.StatementQueries[s.key] = plan
		entry.StatementShapes[s.key] = cloneShape(s.shape)
	}
	return nil
}

// finishShape freezes the parameter order and the data supplied by the
// renderer. scopeType is needed only for a shape with a real session slot.
func (c *typedCompiler) finishShape(key, query string, scopeType catalogType, meta StatementShape) (*compiledShape, error) {
	if key == "" || query == "" || meta.Effects == 0 {
		return nil, fmt.Errorf("tabula: incomplete typed statement")
	}
	if c.params.scoped && scopeType.goType == nil {
		return nil, fmt.Errorf("tabula: shape %q needs the PostgreSQL type of its scope column", key)
	}
	// Add the physical scope and schema requirements for reads and writes alike.
	meta.UnscopedOnly = false
	schemas, forced := slices.Clone(meta.SchemaUsage), slices.Clone(meta.ForceRLS)
	if len(c.functions) > 0 {
		schemas = append(schemas, "pg_catalog")
		for _, signature := range slices.Sorted(maps.Keys(c.functions)) {
			meta.Privileges = append(meta.Privileges, DefinedPrivilege{Function: signature, Privilege: "EXECUTE"})
		}
	}
	columnTypes := maps.Clone(c.reads)
	for _, src := range c.from {
		schemas = append(schemas, src.table.schema)
		switch src.scope {
		case ByRLS():
			forced = append(forced, src.table.sql())
		case ByScopeColumn():
			if c.params.scoped && !c.sources[src] {
				return nil, fmt.Errorf("tabula: missing scope predicate for relation %q", src.alias)
			}
			meta.UnscopedOnly = !c.params.scoped
		}
	}
	for _, slot := range c.params.order {
		schemas = append(schemas, slot.kind.schema)
	}
	if c.params.scoped {
		if !sameSQLType(c.scopeType, scopeType) {
			return nil, fmt.Errorf("tabula: scope slot type differs from its declaration")
		}
		c.params.scopeType = scopeType
		schemas = append(schemas, scopeType.schema)
	}
	slices.Sort(schemas)
	slices.Sort(forced)
	meta.SchemaUsage, meta.ForceRLS = slices.Compact(schemas), slices.Compact(forced)
	meta.PhysicalColumns = nil
	for col, kind := range columnTypes {
		meta.PhysicalColumns = append(meta.PhysicalColumns, col.snapshot(kind))
	}
	slices.SortFunc(meta.PhysicalColumns, func(a, b ShapeColumn) int {
		return cmp.Or(cmp.Compare(a.Relation, b.Relation), cmp.Compare(a.Name, b.Name))
	})
	meta.Inputs = nil
	if c.params.scoped {
		meta.Inputs = append(meta.Inputs, ShapeInput{Name: "scope", Type: scopeType.qualified(), Scope: true, Secret: c.hasSecret})
	}
	for _, slot := range c.params.order {
		meta.Inputs = append(meta.Inputs, ShapeInput{Name: slot.name, Type: slot.kind.qualified(), Secret: c.hasSecret || slot.secret})
	}
	return &compiledShape{
		key: key, sql: query, shape: cloneShape(meta), layout: c.params.frozen(), columns: columnTypes,
		sources: slices.Clone(c.from),
	}, nil
}
