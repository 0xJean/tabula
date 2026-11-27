package tabula

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
	"github.com/0xJean/tabula/internal/planrt"
)

type compiledSQLPlan[Args, S any] struct {
	reg        *Registry[S]
	name       string
	entry      *registration
	shape      *planrt.CompiledShape
	args       *compiledSQLArgs[Args]
	paramOrder []sqlParamSlot // ordered $1..$n
	scope      resolvedSQLScope
	meta       stmtMeta
}

type sqlParamSlot struct {
	name     string
	isScope  bool
	fieldIdx int // index into compiledSQLArgs.fields when !isScope
	secret   bool
}

func compileSQLPlan[Args, S any](
	r *Registry[S],
	spec SQLSpec,
	kind sqlStmtKind,
	ctor string,
	result []ShapeField,
	cols []DefinedColumn,
) (*compiledSQLPlan[Args, S], error) {
	if r == nil {
		return nil, fmt.Errorf("tabula.%s: registry is required", ctor)
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" || strings.ContainsRune(name, 0) {
		return nil, fmt.Errorf("%s.%s: Name is required", r.prefix, ctor)
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s.%s %s: "+format, append([]any{r.prefix, ctor, name}, args...)...)
	}
	rawSQL := strings.TrimSpace(spec.SQL)
	if rawSQL == "" {
		return nil, fail("SQL is required")
	}
	scope, err := resolveSQLScope(r, spec.Scope, fail)
	if err != nil {
		return nil, err
	}
	argsDesc, err := compileSQLArgs[Args](r, fail)
	if err != nil {
		return nil, err
	}
	if scope.mode == scopeParam {
		if _, conflict := argsDesc.byName[scope.param]; conflict {
			return nil, fail("ScopeParam %q must not also be declared on Args", scope.param)
		}
	}
	rewritten, paramNames, err := pgstmt.RewriteNamedSQL(rawSQL, false)
	if err != nil {
		return nil, fail("%w", err)
	}
	rewritten = strings.TrimSpace(rewritten)
	if err := r.checkSQLGuards(rewritten); err != nil {
		return nil, fail("%w", err)
	}
	cls, err := classifySQLStatement(rewritten, kind, spec.AdvisoryLock)
	if err != nil {
		return nil, fail("%w", err)
	}

	meta := statementMeta(name+".sql", spec.Observe)
	usedField := make([]bool, len(argsDesc.fields))
	scopeUsed := false
	paramOrder := make([]sqlParamSlot, 0, len(paramNames))
	inputs := make([]ShapeInput, 0, len(paramNames))
	for _, pName := range paramNames {
		if scope.mode == scopeParam && pName == scope.param {
			scopeUsed = true
			paramOrder = append(paramOrder, sqlParamSlot{
				name:    pName,
				isScope: true,
				secret:  meta.secret,
			})
			inputs = append(inputs, ShapeInput{
				Name:         pName,
				Scope:        true,
				Secret:       meta.secret,
				AllowedTypes: slices.Clone(scope.types),
			})
			continue
		}
		idx, ok := argsDesc.byName[pName]
		if !ok {
			return nil, fail("SQL references unknown parameter @%s", pName)
		}
		usedField[idx] = true
		f := argsDesc.fields[idx]
		sec := meta.secret || f.secret
		paramOrder = append(paramOrder, sqlParamSlot{
			name:     pName,
			fieldIdx: idx,
			secret:   sec,
		})
		inputs = append(inputs, ShapeInput{
			Name:         pName,
			Secret:       sec,
			AllowedTypes: slices.Clone(f.allowedTypes),
		})
	}
	if scope.mode == scopeParam && !scopeUsed {
		return nil, fail("ScopeParam @%s is not referenced in SQL", scope.param)
	}
	for i, f := range argsDesc.fields {
		if !usedField[i] {
			return nil, fail("args field %s (db:%q) is not referenced in SQL", f.goFieldName, f.name)
		}
	}

	stmtShape := StatementShape{
		Inputs:    inputs,
		Result:    result,
		Effects:   cls.effects,
		StructSQL: true,
	}
	compiled := &planrt.CompiledShape{
		ObservationKey: name + ".sql",
		TableName:      name,
		Action:         "sql",
		FixedMethod:    uint8(cls.fixedMethod),
		Authority:      r.statementIssuer,
		Key:            "sql",
		SQL:            rewritten,
		Effects:        uint8(cls.effects),
		HasResult:      len(result) != 0,
		WriteEffects:   cls.stmtEffects,
	}
	def := Definition{
		Name:             name,
		Columns:          cols,
		Statements:       map[string]string{"sql": rewritten},
		StatementQueries: map[string]string{"sql": name},
		StatementShapes:  map[string]StatementShape{"sql": cloneShape(stmtShape)},
	}
	entry, err := r.recordSQLDefinition(def)
	if err != nil {
		return nil, err
	}
	return &compiledSQLPlan[Args, S]{
		reg:        r,
		name:       name,
		entry:      entry,
		shape:      compiled,
		args:       argsDesc,
		paramOrder: paramOrder,
		scope:      scope,
		meta:       meta,
	}, nil
}

func (p *compiledSQLPlan[Args, S]) definition() Definition {
	p.reg.mu.RLock()
	defer p.reg.mu.RUnlock()
	return cloneDefinition(p.entry.def)
}

type sqlSessionBinding[S any] struct {
	reg             *Registry[S]
	s               Session[S]
	driver          AuthorizedSession
	logger          *slog.Logger
	sessionObserved ObserveMask
	bindErr         error
}

func bindSQLSession[S any](reg *Registry[S], s Session[S]) sqlSessionBinding[S] {
	if reg == nil {
		return sqlSessionBinding[S]{bindErr: ErrStatementAuthority}
	}
	if err := reg.checkSession(s); err != nil {
		return sqlSessionBinding[S]{reg: reg, bindErr: err}
	}
	driver, ok := s.(AuthorizedSession)
	if !ok || !eligibleOwner(s) {
		return sqlSessionBinding[S]{reg: reg, bindErr: ErrStatementAuthority}
	}
	var observed ObserveMask
	if obs, ok := s.(interface{ TabulaObserved() ObserveMask }); ok {
		observed = obs.TabulaObserved()
	}
	return sqlSessionBinding[S]{
		reg:             reg,
		s:               s,
		driver:          driver,
		sessionObserved: observed,
	}
}

func (b *sqlSessionBinding[S]) authorize(shape *planrt.CompiledShape, method CallMethod, values []any, secret []bool, meta stmtMeta, requiresScope bool) (AuthorizedStatement, error) {
	if b.bindErr != nil {
		return AuthorizedStatement{}, b.bindErr
	}
	if shape == nil || shape.Authority == nil || shape.Authority != b.reg.statementIssuer || !eligibleOwner(b.s) {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	if !shape.PermitsMethod(uint8(method)) {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	if requiresScope && b.s.Unscoped() {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	effects := ShapeEffects(shape.Effects)
	if effects&(EffectWrite|EffectRowLock) != 0 {
		if ro, ok := b.s.(interface{ ReadOnly() bool }); ok && ro.ReadOnly() {
			return AuthorizedStatement{}, ErrReadOnly
		}
	}
	if effects&EffectWrite != 0 && b.reg.deny&DenyUnscopedWrites != 0 && b.s.Unscoped() {
		return AuthorizedStatement{}, ErrCapabilityDenied
	}
	return AuthorizedStatement{
		method:    method,
		shape:     shape,
		owner:     b.s,
		args:      values,
		secret:    secret,
		quiet:     meta.quiet,
		allSecret: meta.secret,
	}, nil
}

func (p *compiledSQLPlan[Args, S]) bindValues(s Session[S], args *Args) ([]any, []bool, error) {
	values := make([]any, len(p.paramOrder))
	secret := make([]bool, len(p.paramOrder))
	for i, slot := range p.paramOrder {
		secret[i] = slot.secret
		if slot.isScope {
			if s.Unscoped() {
				return nil, nil, ErrStatementAuthority
			}
			values[i] = p.reg.scopeValue(s.Scope())
			continue
		}
		v, err := p.args.fields[slot.fieldIdx].bind(args)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: encode parameter @%s: %w", p.reg.prefix, slot.name, err)
		}
		values[i] = v
	}
	return values, secret, nil
}

type sqlCallbackError struct{ err error }

func (e sqlCallbackError) Error() string { return e.err.Error() }
func (e sqlCallbackError) Unwrap() error { return e.err }

func (b *sqlSessionBinding[S]) runSQL(
	ctx context.Context,
	table, action string,
	meta stmtMeta,
	call AuthorizedStatement,
	execute func(context.Context) (int64, error),
) (int64, error) {
	m := meta
	m.key = call.shape.ObservationKey
	ctx, span := b.reg.startOp(ctx, b.logger, b.s.Scope(), table, action, m, call.secret, b.sessionObserved)
	n, raw := execute(ctx)
	if cbErr, ok := errors.AsType[sqlCallbackError](raw); ok {
		span.finish(ctx, call.shape.SQL, n, cbErr.err, call.args, call.secret, m.secret, m.quiet)
		return n, cbErr.err
	}
	span.finish(ctx, call.shape.SQL, n, raw, call.args, call.secret, m.secret, m.quiet)
	if raw == nil {
		return n, nil
	}
	if errors.Is(raw, ErrNotFound) || errors.Is(raw, ErrMultipleRows) {
		return n, raw
	}
	return n, b.reg.classifyErr(ctx, raw)
}

func scanSQLRow[Row, S any](d *descriptor[Row, S], scan func(...any) error, dst []any, row *Row) error {
	for i, c := range d.cols {
		dst[i] = c.target(row)
	}
	return scan(dst...)
}

func execSQLOne[Row, S any](ctx context.Context, b *sqlSessionBinding[S], d *descriptor[Row, S], table string, meta stmtMeta, call AuthorizedStatement, allowEmpty bool) (Row, bool, error) {
	var row Row
	var found bool
	action := "one"
	if allowEmpty {
		action = "maybe_one"
	}
	_, err := b.runSQL(ctx, table, action, meta, call, func(ctx context.Context) (int64, error) {
		var seen int64
		dst := make([]any, len(d.cols))
		err := b.driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
			seen++
			if seen == 1 {
				return scanSQLRow(d, scan, dst, &row)
			}
			return ErrMultipleRows
		})
		switch {
		case err != nil:
			return seen, err
		case seen > 1:
			return seen, ErrMultipleRows
		case seen == 0:
			if allowEmpty {
				return 0, nil
			}
			return 0, ErrNotFound
		default:
			found = true
			return 1, nil
		}
	})
	if err != nil || !found {
		var zero Row
		return zero, false, err
	}
	return row, true, nil
}

func execSQLFirst[Row, S any](ctx context.Context, b *sqlSessionBinding[S], d *descriptor[Row, S], table string, meta stmtMeta, call AuthorizedStatement) (Row, bool, error) {
	var row Row
	var found bool
	_, err := b.runSQL(ctx, table, "first", meta, call, func(ctx context.Context) (int64, error) {
		dst := make([]any, len(d.cols))
		err := b.driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
			if err := scanSQLRow(d, scan, dst, &row); err != nil {
				return err
			}
			found = true
			return stopIteration
		})
		if errors.Is(err, stopIteration) {
			return 1, nil
		}
		if err != nil {
			return 0, err
		}
		if found {
			return 1, nil
		}
		return 0, nil
	})
	if err != nil || !found {
		var zero Row
		return zero, false, err
	}
	return row, true, nil
}

func execSQLAll[Row, S any](ctx context.Context, b *sqlSessionBinding[S], d *descriptor[Row, S], table, action string, meta stmtMeta, call AuthorizedStatement) ([]Row, error) {
	var rows []Row
	_, err := b.runSQL(ctx, table, action, meta, call, func(ctx context.Context) (int64, error) {
		dst := make([]any, len(d.cols))
		err := b.driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
			var row Row
			if err := scanSQLRow(d, scan, dst, &row); err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		})
		return int64(len(rows)), err
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func execSQLEach[Row, S any](ctx context.Context, b *sqlSessionBinding[S], d *descriptor[Row, S], table, action string, meta stmtMeta, call AuthorizedStatement, fn func(Row) error) error {
	if fn == nil {
		return errors.New("tabula: Each callback is required")
	}
	_, err := b.runSQL(ctx, table, action, meta, call, func(ctx context.Context) (int64, error) {
		var count int64
		dst := make([]any, len(d.cols))
		err := b.driver.EachAuthorized(ctx, call, func(scan func(...any) error) error {
			var row Row
			if err := scanSQLRow(d, scan, dst, &row); err != nil {
				return err
			}
			count++
			if err := fn(row); err != nil {
				return sqlCallbackError{err: err}
			}
			return nil
		})
		return count, err
	})
	return err
}
