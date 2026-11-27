package sqlcheck

import (
	"context"
	"database/sql"
	"errors"
)

// The database/sql adapter intentionally has no protocol-description fallback.
// Only a positively classified read may use its legacy logical result probe.
type sqlVerifier struct{ db *sql.DB }

func (v sqlVerifier) BeginVerification(ctx context.Context) (VerificationTx, error) {
	if v.db == nil {
		return nil, errors.New("sqlcheck: verifier requires a database")
	}
	tx, err := v.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	return sqlVerificationTx{tx}, nil
}

type sqlVerificationTx struct{ tx *sql.Tx }

func (v sqlVerificationTx) Exec(ctx context.Context, query string, args ...any) error {
	_, err := v.tx.ExecContext(ctx, query, args...)
	return err
}
func (v sqlVerificationTx) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	return v.tx.QueryRowContext(ctx, query, args...).Scan(dest...)
}
func (v sqlVerificationTx) Each(ctx context.Context, query string, args []any, visit func(func(...any) error) error) error {
	rows, err := v.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := visit(rows.Scan); err != nil {
			return errors.Join(err, rows.Close(), rows.Err())
		}
	}
	return rows.Err()
}
func (v sqlVerificationTx) Describe(context.Context, string, string) (Description, error) {
	return Description{}, ErrDescribeRequired
}
func (v sqlVerificationTx) Rollback(context.Context) error { return v.tx.Rollback() }
