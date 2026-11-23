package tabula

import (
	"context"
	"database/sql"
	"errors"
	"iter"
	"log/slog"
)

type readBinding[T, S any] struct {
	d              *descriptor[T, S]
	s              Session[S]
	logger         *slog.Logger
	directReadOnly bool
}

func bindReader[T, S any](d *descriptor[T, S], s Session[S]) readBinding[T, S] {
	readOnly, ok := s.(interface{ ReadOnly() bool })
	return readBinding[T, S]{d: d, s: s, directReadOnly: ok && readOnly.ReadOnly()}
}

func (r *readBinding[T, S]) start(ctx context.Context, action, key string) (context.Context, opSpan[S]) {
	return r.d.reg.startOp(ctx, r.logger, r.s.Scope(), r.d.name, action, key)
}

func (r *readBinding[T, S]) fail(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	return r.d.reg.classifyErr(ctx, err)
}

func (r *readBinding[T, S]) fillAddrs(dst []any, row *T) []any {
	for i, c := range r.d.cols {
		dst[i] = c.target(row)
	}
	return dst
}

func (r *readBinding[T, S]) addrs(row *T) []any { return r.fillAddrs(make([]any, len(r.d.cols)), row) }
func (r *readBinding[T, S]) one(ctx context.Context, action string, meta stmtMeta, query string, args []any) (*T, error) {
	ctx, span := r.start(ctx, action, meta.key)
	var row T
	err := r.s.Scan(ctx, query, args, r.addrs(&row)...)
	if errors.Is(err, sql.ErrNoRows) {
		span.finish(ctx, query, 0, nil, args, nil, meta.secret, meta.quiet)
		return nil, nil
	}
	if err != nil {
		span.finish(ctx, query, 0, err, args, nil, meta.secret, meta.quiet)
		return nil, r.fail(ctx, err)
	}
	span.finish(ctx, query, 1, nil, args, nil, meta.secret, meta.quiet)
	return &row, nil
}

func (r *readBinding[T, S]) scan(ctx context.Context, action string, meta stmtMeta, query string, args []any, dest ...any) error {
	ctx, span := r.start(ctx, action, meta.key)
	err := r.s.Scan(ctx, query, args, dest...)
	span.finish(ctx, query, -1, err, args, nil, meta.secret, meta.quiet)
	return r.fail(ctx, err)
}

func (r *readBinding[T, S]) selectOp(ctx context.Context, action string, meta stmtMeta, query string, args []any) ([]T, error) {
	return collect(r.stream(ctx, action, meta, query, args))
}

func collect[T any](seq iter.Seq2[T, error]) ([]T, error) {
	var rows []T
	for row, err := range seq {
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

const stopIteration staticError = "tabula: iteration stopped"

func (r *readBinding[T, S]) stream(ctx context.Context, action string, meta stmtMeta, query string, args []any) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		opCtx, span := r.start(ctx, action, meta.key)
		var count int64
		dst := make([]any, len(r.d.cols))
		stopped := false
		err := r.s.Each(opCtx, query, args, func(scan func(...any) error) error {
			var row T
			if err := scan(r.fillAddrs(dst, &row)...); err != nil {
				return err
			}
			count++
			if !yield(row, nil) {
				stopped = true
				return stopIteration
			}
			return nil
		})
		if errors.Is(err, stopIteration) {
			err = nil
		}
		span.finish(opCtx, query, count, err, args, nil, meta.secret, meta.quiet)
		if err != nil && !stopped {
			var zero T
			yield(zero, r.fail(opCtx, err))
		}
	}
}

// execWrite applies the affected-row contract and observes the driver error
// before application-level classification. A negative bound is unbounded.
func (r *Rows[T, K, S]) execWrite(ctx context.Context, action string, meta stmtMeta, query string, args []any, secret []bool, minRows, maxRows int64, mismatch error) (int64, error) {
	ctx, span := r.start(ctx, action, meta.key)
	n, raw := r.s.Exec(ctx, query, args...)
	err := r.fail(ctx, raw)
	if raw == nil && (n < minRows || maxRows >= 0 && n > maxRows) {
		err = mismatch
	}
	span.finish(ctx, query, n, logErr(raw, err), args, secret, meta.secret, meta.quiet)
	return n, err
}

// returning executes a single-row write. missing defines the result of a
// skipped write (nil for upsert, conflict for CAS, sql.ErrNoRows for insert).
func (r *Rows[T, K, S]) returning(ctx context.Context, action string, meta stmtMeta, query string, args []any, secret []bool, missing error, dest ...any) (bool, error) {
	ctx, span := r.start(ctx, action, meta.key)
	raw := r.s.Returning(ctx, query, args, dest...)
	n := int64(1)
	observed := raw
	var err error
	if raw != nil {
		n = 0
		if errors.Is(raw, sql.ErrNoRows) && missing != sql.ErrNoRows {
			observed, err = missing, missing
		} else {
			err = r.fail(ctx, raw)
		}
	}
	span.finish(ctx, query, n, observed, args, secret, meta.secret, meta.quiet)
	return n == 1, err
}
