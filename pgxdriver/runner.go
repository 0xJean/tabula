package pgxdriver

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/0xJean/tabula"
)

// Runner opens role-bound, guarded PostgreSQL transactions on a *pgxpool.Pool.
type Runner[S any] struct {
	pool     *pgxpool.Pool
	compiled compiledPolicy[S]
	opts     RunnerOptions
	obs      *tabula.Observer
	readObs  *tabula.Observer
}

// NewRunner validates policy and opts and creates a guarded Runner backed by pool.
func NewRunner[S any](pool *pgxpool.Pool, policy ScopePolicy[S], opts RunnerOptions) (*Runner[S], error) {
	if pool == nil {
		return nil, errors.New("pgxdriver.NewRunner: pool is required")
	}
	compiled, err := compilePolicy(policy, opts)
	if err != nil {
		return nil, err
	}
	obsConfig := tabula.ObserverConfig{
		Role:                opts.Role,
		Tracer:              opts.Tracer,
		RawErrorDiagnostics: opts.RawErrorDiagnostics,
		Metrics:             opts.Metrics,
		Log:                 opts.Log,
		Sanitize:            opts.Sanitize,
		TagStatements:       opts.TagStatements,
	}
	obs := tabula.NewObserver(obsConfig)
	var readObs *tabula.Observer
	if opts.UnscopedReadRole != "" {
		obsConfig.Role = opts.UnscopedReadRole
		readObs = tabula.NewObserver(obsConfig)
	}
	return &Runner[S]{
		pool:     pool,
		compiled: compiled,
		opts:     opts,
		obs:      obs,
		readObs:  readObs,
	}, nil
}

// Within runs fn in a read-write transaction. On a ReadOnly runner, Within
// fails closed with tabula.ErrReadOnly before BEGIN.
// A top-level commit error matches tabula.ErrCommit and wraps its driver cause.
func (r *Runner[S]) Within(ctx context.Context, scope S, fn func(context.Context, *Session[S]) error) error {
	return r.WithinWith(ctx, scope, RunOptions[S]{}, fn)
}

// WithinReadOnly runs fn in a READ ONLY transaction. The runner establishes
// the first PostgreSQL snapshot before calling fn, even with an empty policy.
func (r *Runner[S]) WithinReadOnly(ctx context.Context, scope S, fn func(context.Context, *Session[S]) error) error {
	return r.WithinWith(ctx, scope, RunOptions[S]{TxOptions: pgx.TxOptions{AccessMode: pgx.ReadOnly}}, fn)
}

// WithinTxOptions runs fn inside a transaction opened with opts.
func (r *Runner[S]) WithinTxOptions(ctx context.Context, scope S, opts pgx.TxOptions, fn func(context.Context, *Session[S]) error) error {
	return r.WithinWith(ctx, scope, RunOptions[S]{TxOptions: opts}, fn)
}

// WithinWith runs fn inside a transaction configured with runOpts, including an
// optional BeforeCommit step executed on a fresh handle before COMMIT.
func (r *Runner[S]) WithinWith(ctx context.Context, scope S, runOpts RunOptions[S], fn func(context.Context, *Session[S]) error) error {
	if fn == nil {
		return errors.New("pgxdriver: callback is required")
	}
	return r.runTx(ctx, scope, runOpts, false, func(txCtx context.Context, s *Session[S], _ *RawSession[S]) error {
		return fn(txCtx, s)
	})
}

// WithinRaw runs fn with a *RawSession[S] exposing Tx() pgx.Tx. Requires
// RunnerOptions.ExposeTx == true; otherwise returns tabula.ErrCapabilityDenied
// before BEGIN.
func (r *Runner[S]) WithinRaw(ctx context.Context, scope S, fn func(context.Context, *RawSession[S]) error) error {
	return r.WithinRawTxOptions(ctx, scope, pgx.TxOptions{}, fn)
}

// WithinRawTxOptions runs fn with a *RawSession[S] under explicit pgx.TxOptions.
func (r *Runner[S]) WithinRawTxOptions(ctx context.Context, scope S, opts pgx.TxOptions, fn func(context.Context, *RawSession[S]) error) error {
	if fn == nil {
		return errors.New("pgxdriver: callback is required")
	}
	if r.opts.SQLAuthority.DeniesLegacySQL() {
		return tabula.ErrLegacySQL
	}
	if !r.opts.ExposeTx {
		return fmt.Errorf("%w: pgxdriver runner does not enable ExposeTx", tabula.ErrCapabilityDenied)
	}
	return r.runTx(ctx, scope, RunOptions[S]{TxOptions: opts}, true, func(txCtx context.Context, _ *Session[S], raw *RawSession[S]) error {
		return fn(txCtx, raw)
	})
}

// WithinRetry runs the full transaction attempt up to cfg.MaxAttempts times via
// tabula.Retry whenever the attempt fails with a retryable error. The callback
// must be idempotent apart from database writes.
func (r *Runner[S]) WithinRetry(ctx context.Context, scope S, cfg tabula.RetryConfig, fn func(context.Context, *Session[S]) error) error {
	return r.WithinRetryWith(ctx, scope, cfg, RunOptions[S]{}, fn)
}

// WithinRetryWith is WithinRetry with explicit RunOptions (isolation, read-only,
// BeforeCommit). Each retry that completes fn runs BeforeCommit once.
func (r *Runner[S]) WithinRetryWith(ctx context.Context, scope S, cfg tabula.RetryConfig, runOpts RunOptions[S], fn func(context.Context, *Session[S]) error) error {
	return tabula.Retry(ctx, cfg, func(attemptCtx context.Context) error {
		return r.WithinWith(attemptCtx, scope, runOpts, fn)
	})
}

func (r *Runner[S]) checkMode(readOnly, unscoped bool) error {
	if r.opts.ReadOnly && !readOnly {
		return tabula.ErrReadOnly
	}
	if unscoped {
		switch r.opts.Unscoped {
		case UnscopedDenied:
			return fmt.Errorf("%w: unscoped sessions are denied on this runner", tabula.ErrCapabilityDenied)
		case UnscopedReadOnly:
			if !readOnly {
				return tabula.ErrUnscopedRequiresReadOnly
			}
		}
	}
	return nil
}

func (r *Runner[S]) callbackContext(ctx context.Context, lock *statementLock) context.Context {
	if r.opts.RequireCallbackContext {
		ctx, lock.checkCallback = tabula.BindCallbackContext(ctx)
		return ctx
	}
	return tabula.MarkTransaction(ctx)
}

func (r *Runner[S]) newBoundSession(tx pgx.Tx, scope S, unscoped, readOnly bool) *Session[S] {
	s := &Session[S]{
		mu:          new(statementLock),
		tx:          tx,
		scope:       scope,
		unscoped:    unscoped,
		readOnly:    readOnly,
		key:         r.opts.SessionKey,
		obs:         r.obs,
		scopeLog:    r.compiled.policy.ScopeLog,
		statements:  r.compiled.statements,
		authority:   r.opts.SQLAuthority,
		readQueries: readQueries{db: tx, guard: r.compiled.guard},
	}
	s.proof = r.opts.SessionKey.Sign(s)
	return s
}

func (r *Runner[S]) setupTx(ctx context.Context, tx pgx.Tx, pairs []gucPair, readOnly bool) error {
	if readOnly && r.opts.Role != "" && len(pairs) == 0 {
		// Apply the role in the snapshot query when it is the only setting.
		pairs = append(pairs, gucPair{"role", r.opts.Role})
	} else if r.opts.Role != "" {
		if err := SetLocalRole(ctx, tx, r.opts.Role); err != nil {
			return err
		}
	}
	if readOnly {
		// The setup SELECT takes the first snapshot. PostgreSQL then keeps
		// this transaction read-only for its remaining lifetime.
		pairs = append(pairs, gucPair{"transaction_read_only", "on"})
	}
	return execSetConfigBatch(ctx, tx, pairs)
}

func (r *Runner[S]) runTx(
	ctx context.Context,
	scope S,
	runOpts RunOptions[S],
	wantRaw bool,
	invoke func(context.Context, *Session[S], *RawSession[S]) error,
) error {
	if tabula.InTransaction(ctx) {
		return tabula.ErrNested
	}
	readOnly := runOpts.TxOptions.AccessMode == pgx.ReadOnly
	unscoped := r.compiled.policy.IsUnscoped != nil && r.compiled.policy.IsUnscoped(scope)
	if err := r.checkMode(readOnly, unscoped); err != nil {
		return err
	}
	gucs := timeoutPairs(r.opts.Timeouts)
	varPairs, err := r.compiled.collectVars(ctx, scope, unscoped, false)
	if err != nil {
		return err
	}
	gucs = append(gucs, varPairs...)

	var cancel context.CancelFunc
	if r.opts.Timeouts.Transaction > 0 {
		ctx, cancel = context.WithTimeout(ctx, r.opts.Timeouts.Transaction)
		defer cancel()
	}

	tx, err := r.pool.BeginTx(ctx, runOpts.TxOptions)
	if err != nil {
		return fmt.Errorf("pgxdriver: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err := r.setupTx(ctx, tx, gucs, readOnly); err != nil {
		return err
	}

	sess := r.newBoundSession(tx, scope, unscoped, readOnly)
	txCtx := r.callbackContext(ctx, sess.mu)
	defer sess.close()

	var raw *RawSession[S]
	if wantRaw {
		raw = &RawSession[S]{sess: sess}
		raw.proof = r.opts.SessionKey.Sign(raw)
	}

	if err := invoke(txCtx, sess, raw); err != nil {
		return err
	}
	sess.close()

	if runOpts.BeforeCommit != nil {
		stepSess := r.newBoundSession(tx, scope, unscoped, readOnly)
		defer stepSess.close()
		stepCtx := r.callbackContext(txCtx, stepSess.mu)
		if err := runOpts.BeforeCommit(stepCtx, stepSess); err != nil {
			return err
		}
		stepSess.close()
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgxdriver: %w: %w", tabula.ErrCommit, err)
	}
	committed = true
	return nil
}

// Shared exposes scoped and unscoped Session handles sharing a single pgx
// transaction. Before every user statement on a child handle, the
// complete managed GUC state is reapplied in one batch. With UnscopedReadRole
// the runner also selects the handle's database role before each statement.
type Shared[S any] struct {
	state *sharedState[S]
}

type sharedState[S any] struct {
	mu       statementLock
	tx       pgx.Tx
	runner   *Runner[S]
	readOnly bool
}

func (st *sharedState[S]) closeAll() {
	st.mu.close()
}

func (st *sharedState[S]) applyFor(ctx context.Context, tx pgx.Tx, scope S, unscoped bool) error {
	pairs, err := st.runner.compiled.collectVars(ctx, scope, unscoped, true)
	if err != nil {
		return err
	}
	if opts := st.runner.opts; opts.UnscopedReadRole != "" {
		role := opts.Role
		if unscoped {
			role = opts.UnscopedReadRole
		}
		if err := SetLocalRole(ctx, tx, role); err != nil {
			return err
		}
	}
	return execSetConfigBatch(ctx, tx, pairs)
}

func (st *sharedState[S]) mint(scope S, unscoped bool) *Session[S] {
	readRole := unscoped && st.runner.opts.UnscopedReadRole != ""
	var denied error
	obs := st.runner.obs
	if readRole {
		obs = st.runner.readObs
	} else {
		denied = st.runner.checkMode(st.readOnly, unscoped)
	}
	s := &Session[S]{
		mu:          &st.mu,
		tx:          st.tx,
		scope:       scope,
		unscoped:    unscoped,
		readOnly:    st.readOnly || readRole,
		readRole:    readRole,
		denied:      denied,
		key:         st.runner.opts.SessionKey,
		obs:         obs,
		scopeLog:    st.runner.compiled.policy.ScopeLog,
		statements:  st.runner.compiled.statements,
		authority:   st.runner.opts.SQLAuthority,
		shared:      st,
		readQueries: readQueries{db: st.tx, guard: st.runner.compiled.guard},
	}
	s.proof = st.runner.opts.SessionKey.Sign(s)
	return s
}

// Unscoped returns an unscoped Session handle sharing this transaction.
// UnscopedReadOnly requires a READ ONLY transaction unless UnscopedReadRole
// is configured. That role permits Scan and Each while scoped handles use Role.
// UnscopedDenied rejects all operations with tabula.ErrCapabilityDenied.
func (s *Shared[S]) Unscoped() *Session[S] {
	var zero S
	return s.state.mint(zero, true)
}

// Scoped returns a scoped Session handle sharing this transaction.
func (s *Shared[S]) Scoped(scope S) *Session[S] {
	unscoped := s.state.runner.compiled.policy.IsUnscoped != nil && s.state.runner.compiled.policy.IsUnscoped(scope)
	return s.state.mint(scope, unscoped)
}

// WithinShared opens one transaction that can switch between scoped and
// unscoped handles via Shared.Scoped and Shared.Unscoped.
func (r *Runner[S]) WithinShared(ctx context.Context, fn func(context.Context, *Shared[S]) error) error {
	var txOpts pgx.TxOptions
	if r.opts.ReadOnly {
		txOpts.AccessMode = pgx.ReadOnly
	}
	return r.WithinSharedOptions(ctx, txOpts, fn)
}

// WithinSharedOptions is WithinShared with explicit pgx.TxOptions.
// With UnscopedReadRole configured, the runner restores Role before COMMIT.
func (r *Runner[S]) WithinSharedOptions(ctx context.Context, opts pgx.TxOptions, fn func(context.Context, *Shared[S]) error) error {
	if fn == nil {
		return errors.New("pgxdriver: callback is required")
	}
	if tabula.InTransaction(ctx) {
		return tabula.ErrNested
	}
	readOnly := opts.AccessMode == pgx.ReadOnly
	if r.opts.ReadOnly && !readOnly {
		return tabula.ErrReadOnly
	}
	var cancel context.CancelFunc
	if r.opts.Timeouts.Transaction > 0 {
		ctx, cancel = context.WithTimeout(ctx, r.opts.Timeouts.Transaction)
		defer cancel()
	}
	tx, err := r.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("pgxdriver: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err := r.setupTx(ctx, tx, timeoutPairs(r.opts.Timeouts), readOnly); err != nil {
		return err
	}
	state := &sharedState[S]{
		tx:       tx,
		runner:   r,
		readOnly: readOnly,
	}
	defer state.closeAll()
	txCtx := r.callbackContext(ctx, &state.mu)
	if err := fn(txCtx, &Shared[S]{state: state}); err != nil {
		return err
	}
	state.closeAll()
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.opts.UnscopedReadRole != "" {
		// Deferred triggers can run under the role active at COMMIT.
		if err := SetLocalRole(ctx, tx, r.opts.Role); err != nil {
			return fmt.Errorf("pgxdriver: restore role before commit: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgxdriver: %w: %w", tabula.ErrCommit, err)
	}
	committed = true
	return nil
}
