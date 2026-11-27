package tabula

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

type compiledSQLPlan[Args, S any] struct {
	reg        *Registry[S]
	name       string
	entry      *registration
	shape      *compiledShape
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
		Inputs:  inputs,
		Result:  result,
		Effects: cls.effects,
	}
	compiled := &compiledShape{
		observationKey: name + ".sql",
		tableName:      name,
		action:         "sql",
		fixedMethod:    cls.fixedMethod,
		authority:      r.statementIssuer,
		key:            "sql",
		sql:            rewritten,
		shape:          cloneShape(stmtShape),
		effects:        cls.stmtEffects,
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

func (b *sqlSessionBinding[S]) authorize(shape *compiledShape, method CallMethod, values []any, secret []bool, meta stmtMeta, requiresScope bool) (AuthorizedStatement, error) {
	if b.bindErr != nil {
		return AuthorizedStatement{}, b.bindErr
	}
	if shape == nil || shape.authority == nil || shape.authority != b.reg.statementIssuer || !eligibleOwner(b.s) {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	if !shape.permitsMethod(method) {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	if requiresScope && b.s.Unscoped() {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	effects := shape.shape.Effects
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
	m.key = call.shape.observationKey
	ctx, span := b.reg.startOp(ctx, b.logger, b.s.Scope(), table, action, m, call.secret, b.sessionObserved)
	n, raw := execute(ctx)
	if cbErr, ok := errors.AsType[sqlCallbackError](raw); ok {
		span.finish(ctx, call.shape.sql, n, cbErr.err, call.args, call.secret, m.secret, m.quiet)
		return n, cbErr.err
	}
	span.finish(ctx, call.shape.sql, n, raw, call.args, call.secret, m.secret, m.quiet)
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

// SQLQuery is a compiled, registered read-only named-parameter SQL query
// scanning results into Row and binding parameters from Args.
type SQLQuery[Row, Args, S any] struct {
	plan *compiledSQLPlan[Args, S]
	row  *descriptor[Row, S]
}

// DefineSQL validates and registers a read-only named-parameter SQL query on r.
// It accepts SELECT, WITH ... SELECT, VALUES, and row-locking SELECT ... FOR
// UPDATE/SHARE (automatically marking EffectRowLock), and rejects writes and
// modifying CTEs. Panics on declaration mistakes at initialization.
func DefineSQL[Row, Args, S any](r *Registry[S], spec SQLSpec) *SQLQuery[Row, Args, S] {
	q, err := TryDefineSQL[Row, Args](r, spec)
	if err != nil {
		panic(err)
	}
	return q
}

// TryDefineSQL validates and atomically registers a read-only named-parameter
// SQL query on r, publishing nothing on error.
func TryDefineSQL[Row, Args, S any](r *Registry[S], spec SQLSpec) (*SQLQuery[Row, Args, S], error) {
	if r == nil {
		return nil, errors.New("tabula.DefineSQL: registry is required")
	}
	name := strings.TrimSpace(spec.Name)
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s.DefineSQL %s: "+format, append([]any{r.prefix, name}, args...)...)
	}
	rowDesc, resultFields, err := compileSQLRow[Row](r, name, fail)
	if err != nil {
		return nil, err
	}
	plan, err := compileSQLPlan[Args](r, spec, sqlKindRead, "DefineSQL", resultFields, rowDesc.definedColumns())
	if err != nil {
		return nil, err
	}
	return &SQLQuery[Row, Args, S]{plan: plan, row: rowDesc}, nil
}

// Name returns the registered declaration name.
func (q *SQLQuery[Row, Args, S]) Name() string { return q.plan.name }

// SQL returns the compiled positional PostgreSQL SQL ($1..$n).
func (q *SQLQuery[Row, Args, S]) SQL() string { return q.plan.shape.sql }

// Definition returns a deep copy of the registered Definition for sqlcheck.
func (q *SQLQuery[Row, Args, S]) Definition() Definition { return q.plan.definition() }

// Bind attaches this query to an authorized session from the owning Registry.
func (q *SQLQuery[Row, Args, S]) Bind(s Session[S]) *BoundSQLQuery[Row, Args, S] {
	if q == nil || q.plan == nil {
		return &BoundSQLQuery[Row, Args, S]{binding: sqlSessionBinding[S]{bindErr: ErrStatementAuthority}}
	}
	return &BoundSQLQuery[Row, Args, S]{q: q, binding: bindSQLSession(q.plan.reg, s)}
}

// BoundSQLQuery executes a registered SQLQuery on one authorized session.
type BoundSQLQuery[Row, Args, S any] struct {
	q       *SQLQuery[Row, Args, S]
	binding sqlSessionBinding[S]
}

// WithLogger returns a handle with a per-binding logger override.
func (b *BoundSQLQuery[Row, Args, S]) WithLogger(logger *slog.Logger) *BoundSQLQuery[Row, Args, S] {
	clone := *b
	clone.binding.logger = logger
	return &clone
}

func (b *BoundSQLQuery[Row, Args, S]) prepare(args *Args) (AuthorizedStatement, error) {
	if b == nil || b.q == nil || b.q.plan == nil {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return AuthorizedStatement{}, b.binding.bindErr
	}
	values, secret, err := b.q.plan.bindValues(b.binding.s, args)
	if err != nil {
		return AuthorizedStatement{}, err
	}
	return b.binding.authorize(b.q.plan.shape, CallEach, values, secret, b.q.plan.meta, b.q.plan.scope.mode == scopeParam)
}

// One returns the single matching row, ErrNotFound on 0 rows, or ErrMultipleRows on >1 rows.
func (b *BoundSQLQuery[Row, Args, S]) One(ctx context.Context, args Args) (Row, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, err
	}
	row, _, err := execSQLOne(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call, false)
	return row, err
}

// MaybeOne returns the single matching row and true, (zero, false, nil) on 0 rows,
// or ErrMultipleRows on >1 rows.
func (b *BoundSQLQuery[Row, Args, S]) MaybeOne(ctx context.Context, args Args) (Row, bool, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, false, err
	}
	return execSQLOne(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call, true)
}

// First returns the first matching row and true, or (zero, false, nil) on 0 rows.
func (b *BoundSQLQuery[Row, Args, S]) First(ctx context.Context, args Args) (Row, bool, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, false, err
	}
	return execSQLFirst(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call)
}

// All decodes and returns all matching rows.
func (b *BoundSQLQuery[Row, Args, S]) All(ctx context.Context, args Args) ([]Row, error) {
	call, err := b.prepare(&args)
	if err != nil {
		return nil, err
	}
	return execSQLAll(ctx, &b.binding, b.q.row, b.q.plan.name, "all", b.q.plan.meta, call)
}

// Each streams every matching row to fn, stopping on the first error returned by fn.
func (b *BoundSQLQuery[Row, Args, S]) Each(ctx context.Context, args Args, fn func(Row) error) error {
	call, err := b.prepare(&args)
	if err != nil {
		return err
	}
	return execSQLEach(ctx, &b.binding, b.q.row, b.q.plan.name, "each", b.q.plan.meta, call, fn)
}

// SQLExec is a compiled, registered row-count-only named-parameter SQL mutation
// (or explicit transaction-level advisory lock) binding parameters from Args.
type SQLExec[Args, S any] struct {
	plan         *compiledSQLPlan[Args, S]
	advisoryLock bool
}

// DefineSQLExec validates and registers a non-returning write statement
// (INSERT, UPDATE, DELETE, MERGE, or modifying CTE without top-level RETURNING)
// or an explicit AdvisoryLock transaction-level advisory lock call on r.
// Panics on declaration mistakes at initialization.
func DefineSQLExec[Args, S any](r *Registry[S], spec SQLSpec) *SQLExec[Args, S] {
	q, err := TryDefineSQLExec[Args](r, spec)
	if err != nil {
		panic(err)
	}
	return q
}

// TryDefineSQLExec validates and atomically registers a non-returning write or
// transaction-level advisory lock on r, publishing nothing on error.
func TryDefineSQLExec[Args, S any](r *Registry[S], spec SQLSpec) (*SQLExec[Args, S], error) {
	plan, err := compileSQLPlan[Args](r, spec, sqlKindExec, "DefineSQLExec", nil, nil)
	if err != nil {
		return nil, err
	}
	return &SQLExec[Args, S]{plan: plan, advisoryLock: spec.AdvisoryLock}, nil
}

// Name returns the registered declaration name.
func (q *SQLExec[Args, S]) Name() string { return q.plan.name }

// SQL returns the compiled positional PostgreSQL SQL ($1..$n).
func (q *SQLExec[Args, S]) SQL() string { return q.plan.shape.sql }

// Definition returns a deep copy of the registered Definition for sqlcheck.
func (q *SQLExec[Args, S]) Definition() Definition { return q.plan.definition() }

// Bind attaches this statement to an authorized session from the owning Registry.
func (q *SQLExec[Args, S]) Bind(s Session[S]) *BoundSQLExec[Args, S] {
	if q == nil || q.plan == nil {
		return &BoundSQLExec[Args, S]{binding: sqlSessionBinding[S]{bindErr: ErrStatementAuthority}}
	}
	return &BoundSQLExec[Args, S]{q: q, binding: bindSQLSession(q.plan.reg, s)}
}

// BoundSQLExec executes a registered SQLExec on one authorized session.
type BoundSQLExec[Args, S any] struct {
	q       *SQLExec[Args, S]
	binding sqlSessionBinding[S]
}

// WithLogger returns a handle with a per-binding logger override.
func (b *BoundSQLExec[Args, S]) WithLogger(logger *slog.Logger) *BoundSQLExec[Args, S] {
	clone := *b
	clone.binding.logger = logger
	return &clone
}

// Exec executes the statement and returns the affected row count.
func (b *BoundSQLExec[Args, S]) Exec(ctx context.Context, args Args) (int64, error) {
	if b == nil || b.q == nil || b.q.plan == nil {
		return 0, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return 0, b.binding.bindErr
	}
	values, secret, err := b.q.plan.bindValues(b.binding.s, &args)
	if err != nil {
		return 0, err
	}
	method := CallExec
	if b.q.advisoryLock {
		method = CallEach
	}
	call, err := b.binding.authorize(b.q.plan.shape, method, values, secret, b.q.plan.meta, b.q.plan.scope.mode == scopeParam)
	if err != nil {
		return 0, err
	}
	if b.q.advisoryLock {
		return b.binding.runSQL(ctx, b.q.plan.name, "exec", b.q.plan.meta, call, func(ctx context.Context) (int64, error) {
			var count int64
			err := b.binding.driver.EachAuthorized(ctx, call, func(func(...any) error) error {
				count++
				return nil
			})
			return count, err
		})
	}
	return b.binding.runSQL(ctx, b.q.plan.name, "exec", b.q.plan.meta, call, func(ctx context.Context) (int64, error) {
		return b.binding.driver.ExecAuthorized(ctx, call)
	})
}

// SQLReturning is a compiled, registered row-returning named-parameter SQL
// write (INSERT/UPDATE/DELETE/MERGE ... RETURNING ... or a modifying CTE
// returning rows) scanning results into Row and binding parameters from Args.
type SQLReturning[Row, Args, S any] struct {
	plan *compiledSQLPlan[Args, S]
	row  *descriptor[Row, S]
}

// DefineSQLReturning validates and registers a returning write statement on r.
// Panics on declaration mistakes at initialization.
func DefineSQLReturning[Row, Args, S any](r *Registry[S], spec SQLSpec) *SQLReturning[Row, Args, S] {
	q, err := TryDefineSQLReturning[Row, Args](r, spec)
	if err != nil {
		panic(err)
	}
	return q
}

// TryDefineSQLReturning validates and atomically registers a returning write
// statement on r, publishing nothing on error.
func TryDefineSQLReturning[Row, Args, S any](r *Registry[S], spec SQLSpec) (*SQLReturning[Row, Args, S], error) {
	if r == nil {
		return nil, errors.New("tabula.DefineSQLReturning: registry is required")
	}
	name := strings.TrimSpace(spec.Name)
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%s.DefineSQLReturning %s: "+format, append([]any{r.prefix, name}, args...)...)
	}
	rowDesc, resultFields, err := compileSQLRow[Row](r, name, fail)
	if err != nil {
		return nil, err
	}
	plan, err := compileSQLPlan[Args](r, spec, sqlKindReturning, "DefineSQLReturning", resultFields, rowDesc.definedColumns())
	if err != nil {
		return nil, err
	}
	return &SQLReturning[Row, Args, S]{plan: plan, row: rowDesc}, nil
}

// Name returns the registered declaration name.
func (q *SQLReturning[Row, Args, S]) Name() string { return q.plan.name }

// SQL returns the compiled positional PostgreSQL SQL ($1..$n).
func (q *SQLReturning[Row, Args, S]) SQL() string { return q.plan.shape.sql }

// Definition returns a deep copy of the registered Definition for sqlcheck.
func (q *SQLReturning[Row, Args, S]) Definition() Definition { return q.plan.definition() }

// Bind attaches this statement to an authorized session from the owning Registry.
func (q *SQLReturning[Row, Args, S]) Bind(s Session[S]) *BoundSQLReturning[Row, Args, S] {
	if q == nil || q.plan == nil {
		return &BoundSQLReturning[Row, Args, S]{binding: sqlSessionBinding[S]{bindErr: ErrStatementAuthority}}
	}
	return &BoundSQLReturning[Row, Args, S]{q: q, binding: bindSQLSession(q.plan.reg, s)}
}

// BoundSQLReturning executes a registered SQLReturning on one authorized session.
type BoundSQLReturning[Row, Args, S any] struct {
	q       *SQLReturning[Row, Args, S]
	binding sqlSessionBinding[S]
}

// WithLogger returns a handle with a per-binding logger override.
func (b *BoundSQLReturning[Row, Args, S]) WithLogger(logger *slog.Logger) *BoundSQLReturning[Row, Args, S] {
	clone := *b
	clone.binding.logger = logger
	return &clone
}

func (b *BoundSQLReturning[Row, Args, S]) prepare(args *Args) (AuthorizedStatement, error) {
	if b == nil || b.q == nil || b.q.plan == nil {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return AuthorizedStatement{}, b.binding.bindErr
	}
	values, secret, err := b.q.plan.bindValues(b.binding.s, args)
	if err != nil {
		return AuthorizedStatement{}, err
	}
	return b.binding.authorize(b.q.plan.shape, CallReturningEach, values, secret, b.q.plan.meta, b.q.plan.scope.mode == scopeParam)
}

// One executes the write and returns the single returned row, ErrNotFound on 0
// rows, or ErrMultipleRows on >1 rows.
func (b *BoundSQLReturning[Row, Args, S]) One(ctx context.Context, args Args) (Row, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, err
	}
	row, _, err := execSQLOne(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call, false)
	return row, err
}

// MaybeOne executes the write and returns the single returned row and true,
// (zero, false, nil) on 0 rows, or ErrMultipleRows on >1 rows.
func (b *BoundSQLReturning[Row, Args, S]) MaybeOne(ctx context.Context, args Args) (Row, bool, error) {
	call, err := b.prepare(&args)
	if err != nil {
		var zero Row
		return zero, false, err
	}
	return execSQLOne(ctx, &b.binding, b.q.row, b.q.plan.name, b.q.plan.meta, call, true)
}

// All executes the write and decodes every returned row.
func (b *BoundSQLReturning[Row, Args, S]) All(ctx context.Context, args Args) ([]Row, error) {
	call, err := b.prepare(&args)
	if err != nil {
		return nil, err
	}
	return execSQLAll(ctx, &b.binding, b.q.row, b.q.plan.name, "returning_all", b.q.plan.meta, call)
}

// Each executes the write and streams every returned row to fn.
func (b *BoundSQLReturning[Row, Args, S]) Each(ctx context.Context, args Args, fn func(Row) error) error {
	call, err := b.prepare(&args)
	if err != nil {
		return err
	}
	return execSQLEach(ctx, &b.binding, b.q.row, b.q.plan.name, "returning_each", b.q.plan.meta, call, fn)
}
