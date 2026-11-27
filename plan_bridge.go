package tabula

import (
	"context"
	"fmt"
	"maps"
	"reflect"

	"github.com/0xJean/tabula/internal/planrt"
)

// PlanTable exposes internal table metadata and sealed execution hooks to
// github.com/0xJean/tabula/plan. External packages cannot construct a Seal.
func (t *Table[T, K, S]) PlanTable(seal planrt.Seal) *planrt.Table[T, S] {
	if !seal.Valid() {
		panic("tabula: invalid internal bridge seal")
	}
	if t == nil || t.entry == nil || t.reg == nil {
		return nil
	}
	owner := t.planOwner()
	if owner == nil {
		return nil
	}
	var delKind planrt.DeleteKind
	switch t.del.kind {
	case deleteTimestamp:
		delKind = planrt.DeleteTimestamp
	case deleteBool:
		delKind = planrt.DeleteBool
	case deleteStatus:
		delKind = planrt.DeleteStatus
	}
	return &planrt.Table[T, S]{
		Owner:             owner,
		RelationScopeKind: t.relationScope.kind,
		WriteMode:         uint8(t.mode),
		ReadOnly:          t.readOnly,
		AllowDelete:       t.allowDelete,
		ScopeIdx:          t.scope,
		DeletedIdx:        t.deleted,
		VersionIdx:        t.version,
		Delete: planrt.DeleteInfo{
			Kind:       delKind,
			DeletedVal: t.del.deletedVal,
			AliveVal:   t.del.aliveVal,
		},
		AllowsUpdate: t.allowsUpdateColumn,
	}
}

// PlanView exposes internal view metadata and sealed execution hooks to
// github.com/0xJean/tabula/plan. External packages cannot construct a Seal.
func (v *View[T, S]) PlanView(seal planrt.Seal) *planrt.Owner[T, S] {
	if !seal.Valid() {
		panic("tabula: invalid internal bridge seal")
	}
	if v == nil || v.entry == nil || v.reg == nil {
		return nil
	}
	return v.planOwner()
}

func (d *descriptor[T, S]) planOwner() *planrt.Owner[T, S] {
	if d == nil || d.entry == nil || d.reg == nil {
		return nil
	}
	cols := make([]planrt.Column[T], len(d.cols))
	for i, c := range d.cols {
		var r planrt.ColumnRole
		switch c.role {
		case keyRole:
			r = planrt.RoleKey
		case scopeRole:
			r = planrt.RoleScope
		case deletedRole:
			r = planrt.RoleDeleted
		case versionRole:
			r = planrt.RoleVersion
		}
		cols[i] = planrt.Column[T]{
			Name:            c.name,
			Role:            r,
			Once:            c.once,
			Secret:          c.secret,
			Updatable:       c.updatable(),
			GoType:          c.goType,
			Nullable:        c.nullable,
			NeedsColumnType: c.needsColumnType,
			Target:          c.target,
			Field:           c.field,
		}
	}
	var defaults map[reflect.Type]planrt.DefaultType
	if len(d.reg.defaultTypes) > 0 {
		defaults = make(map[reflect.Type]planrt.DefaultType, len(d.reg.defaultTypes))
		for k, v := range d.reg.defaultTypes {
			defaults[k] = planrt.DefaultType{Kind: v.kind, Typ: v.typ}
		}
	}
	reg := d.reg
	return &planrt.Owner[T, S]{
		Issuer:         reg.statementIssuer,
		Entry:          d.entry.id,
		Name:           d.name,
		Columns:        cols,
		ByName:         maps.Clone(d.byName),
		HasScopeID:     reg.scopeID != nil,
		DefaultTypes:   defaults,
		RegisterShapes: d.registerPlanShapes,
		StatementMeta: func(key string, opts any) planrt.Meta {
			var list []StatementOption
			if opts != nil {
				list = opts.([]StatementOption)
			}
			m := statementMeta(key, list)
			return planrt.Meta{Key: m.key, Quiet: m.quiet, Secret: m.secret}
		},
		CheckSession: func(s any) error {
			sess, ok := s.(Session[S])
			if !ok {
				return ErrStatementAuthority
			}
			return reg.checkSession(sess)
		},
		EligibleOwner: eligibleOwner,
		EncodeScope: func(s any, scopeType planrt.CatalogType) (any, error) {
			sess, ok := s.(Session[S])
			if !ok {
				return nil, ErrStatementAuthority
			}
			if reg.scopeID != nil {
				return scopeType.Snapshot(reg.scopeID(sess.Scope())), nil
			}
			val, err := scopeType.EncodeScope(sess.Scope())
			if err != nil {
				return nil, fmt.Errorf("tabula: encode scope: %w", err)
			}
			return val, nil
		},
		AdaptArray:   reg.arrayArgFn,
		Authorize:    reg.authorizePlanCall,
		ValidateAuth: reg.validatePlanAuth,
		Run:          d.runPlanOp,
		FillAddrs: func(dst []any, row *T) []any {
			for i, c := range d.cols {
				dst[i] = c.target(row)
			}
			return dst
		},
		Addrs: func(row *T) []any {
			dst := make([]any, len(d.cols))
			for i, c := range d.cols {
				dst[i] = c.target(row)
			}
			return dst
		},
	}
}

func (r *Registry[S]) validatePlanAuth(s any, shape *planrt.CompiledShape, method uint8, scoped bool) error {
	sess, ok := s.(Session[S])
	if !ok {
		return ErrStatementAuthority
	}
	if err := r.checkSession(sess); err != nil {
		return err
	}
	if shape == nil || shape.Authority == nil || shape.Authority != r.statementIssuer || !eligibleOwner(sess) {
		return ErrStatementAuthority
	}
	if !shape.PermitsMethod(method) {
		return ErrStatementAuthority
	}
	if shape.UnscopedOnly && !sess.Unscoped() {
		return ErrStatementAuthority
	}
	effects := ShapeEffects(shape.Effects)
	if effects&(EffectWrite|EffectRowLock) != 0 {
		if ro, ok := sess.(interface{ ReadOnly() bool }); ok && ro.ReadOnly() {
			return ErrReadOnly
		}
	}
	if effects&EffectWrite != 0 && r.deny&DenyUnscopedWrites != 0 && sess.Unscoped() {
		return ErrCapabilityDenied
	}
	if scoped && sess.Unscoped() {
		return ErrStatementAuthority
	}
	return nil
}

func (r *Registry[S]) authorizePlanCall(s any, shape *planrt.CompiledShape, method uint8, args []any, secret []bool, quiet, allSecret, scoped bool) (any, error) {
	if err := r.validatePlanAuth(s, shape, method, scoped); err != nil {
		return AuthorizedStatement{}, err
	}
	return AuthorizedStatement{
		method:    CallMethod(method),
		shape:     shape,
		owner:     s,
		args:      args,
		secret:    secret,
		quiet:     quiet,
		allSecret: allSecret,
	}, nil
}

func (d *descriptor[T, S]) runPlanOp(
	ctx context.Context,
	s any,
	action string,
	meta planrt.Meta,
	rawCall any,
	execute func(context.Context) (int64, error),
) (int64, error) {
	sess, ok := s.(Session[S])
	if !ok {
		return 0, ErrStatementAuthority
	}
	call, ok := rawCall.(AuthorizedStatement)
	if !ok || call.shape == nil {
		return 0, ErrStatementAuthority
	}
	var observed ObserveMask
	if obs, ok := sess.(interface{ TabulaObserved() ObserveMask }); ok {
		observed = obs.TabulaObserved()
	}
	m := stmtMeta{key: call.shape.ObservationKey, quiet: meta.Quiet, secret: meta.Secret}
	if action == "" {
		action = call.shape.Action
	}
	ctx, span := d.reg.startOp(ctx, nil, sess.Scope(), d.name, action, m, call.secret, observed)
	n, raw := execute(ctx)
	span.finish(ctx, call.shape.SQL, n, raw, call.args, call.secret, m.secret, m.quiet)
	if raw == nil {
		return n, nil
	}
	return n, d.reg.classifyErr(ctx, raw)
}

// registerPlanShapes publishes the complete family under one registry lock. It
// checks all sources, keys and physical column types before changing either index.
func (d *descriptor[T, S]) registerPlanShapes(plan string, specs []planrt.ShapeRegistration) error {
	if d.entry == nil || plan == "" || len(specs) == 0 {
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
	types := make(map[planrt.PhysicalColumn]planrt.CatalogType)
	parsedShapes := make([]StatementShape, len(specs))
	for i, spec := range specs {
		s := spec.Compiled
		st, ok := spec.Shape.(StatementShape)
		if !ok || s == nil || s.Key == "" || s.SQL == "" || st.Effects == 0 || s.Authority != nil || spec.Check == nil {
			return fmt.Errorf("tabula: incomplete shape in plan %q", plan)
		}
		if err := spec.Check(r.statementIssuer); err != nil {
			return err
		}
		_, registered := entry.Statements[s.Key]
		if keys[s.Key] || registered {
			return fmt.Errorf("tabula: %s.%s already registered", d.name, s.Key)
		}
		keys[s.Key] = true
		for col, kind := range spec.Columns {
			if old, ok := r.typedColumns[col]; ok {
				if err := col.Record(types, old); err != nil {
					return err
				}
			}
			if err := col.Record(types, kind); err != nil {
				return err
			}
		}
		parsedShapes[i] = st
	}
	if r.typedColumns == nil {
		r.typedColumns = make(map[planrt.PhysicalColumn]planrt.CatalogType)
	}
	maps.Copy(r.typedColumns, types)
	if entry.StatementQueries == nil {
		entry.StatementQueries = make(map[string]string)
	}
	if entry.StatementShapes == nil {
		entry.StatementShapes = make(map[string]StatementShape)
	}
	for i, spec := range specs {
		s := spec.Compiled
		st := cloneShape(parsedShapes[i])
		s.WriteEffects = s.WriteEffects.Clone()
		s.Effects = uint8(st.Effects)
		s.HasResult = len(st.Result) != 0
		s.UnscopedOnly = st.UnscopedOnly
		s.Routines = make([]planrt.RoutineMeta, len(st.Routines))
		for j, rt := range st.Routines {
			s.Routines[j] = planrt.RoutineMeta{Signature: rt.Signature, ReadOnly: rt.ReadOnly}
		}
		s.FunctionGrants = nil
		for _, p := range st.Privileges {
			if p.Function != "" {
				s.FunctionGrants = append(s.FunctionGrants, planrt.FunctionGrant{Function: p.Function, Privilege: p.Privilege})
			}
		}
		s.Authority = r.statementIssuer
		s.ObservationKey = d.name + "." + s.Key
		s.TableName, s.Action = d.name, plan
		entry.Statements[s.Key] = s.SQL
		entry.StatementQueries[s.Key] = plan
		entry.StatementShapes[s.Key] = st
	}
	return nil
}
