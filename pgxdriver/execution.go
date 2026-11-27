package pgxdriver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
)

// These helpers run only after text or signed-call validation.
func (s *Session[S]) execChecked(ctx context.Context, query string, args []any) (int64, error) {
	if err := s.tryLock(); err != nil {
		return 0, err
	}
	defer s.mu.Unlock()
	if err := s.applyScope(ctx); err != nil {
		return 0, err
	}
	tagged := s.obs.TagSQL(ctx, query)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), query, args)
	tag, err := s.tx.Exec(opCtx, tagged, args...)
	if err != nil {
		err = directReadError(err)
		done(0, err)
		return 0, err
	}
	n := tag.RowsAffected()
	done(n, nil)
	return n, nil
}

func (s *Session[S]) scanChecked(ctx context.Context, query string, args []any, dest ...any) error {
	if err := s.tryLock(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := s.applyScope(ctx); err != nil {
		return err
	}
	tagged := s.obs.TagSQL(ctx, query)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), query, args)
	err := s.tx.QueryRow(opCtx, tagged, args...).Scan(dest...)
	if errors.Is(err, pgx.ErrNoRows) {
		err = sql.ErrNoRows
	} else {
		err = directReadError(err)
	}
	done(-1, err)
	return err
}

func (s *Session[S]) eachChecked(ctx context.Context, query string, args []any, decode func(func(...any) error) error) error {
	if err := s.tryLock(); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := s.applyScope(ctx); err != nil {
		return err
	}
	tagged := s.obs.TagSQL(ctx, query)
	opCtx, done := s.obs.Start(ctx, s.scopeLabel(), query, args)
	rows, err := s.tx.Query(opCtx, tagged, args...)
	if err != nil {
		err = directReadError(err)
		done(0, err)
		return err
	}
	return s.mu.each(rows, decode, done)
}
