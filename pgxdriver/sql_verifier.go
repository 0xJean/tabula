package pgxdriver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
)

// SQLVerifier supplies non-executing PostgreSQL statement descriptions to
// sqlcheck. It may be used independently of the application's runtime driver.
// Each verification uses one read-only transaction and always rolls it back.
func SQLVerifier(pool *pgxpool.Pool) sqlcheck.Verifier { return protocolVerifier{pool} }

type protocolVerifier struct{ pool *pgxpool.Pool }

func (v protocolVerifier) BeginVerification(ctx context.Context) (sqlcheck.VerificationTx, error) {
	if v.pool == nil {
		return nil, errors.New("pgxdriver: SQLVerifier requires a pool")
	}
	tx, err := v.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	return &protocolVerificationTx{tx: tx}, nil
}

type protocolVerificationTx struct {
	tx     pgx.Tx
	mu     sync.Mutex
	closed atomic.Bool
}

func (v *protocolVerificationTx) lock() error {
	if v.closed.Load() {
		return pgx.ErrTxClosed
	}
	if !v.mu.TryLock() {
		return tabula.ErrSessionBusy
	}
	if v.closed.Load() {
		v.mu.Unlock()
		return pgx.ErrTxClosed
	}
	return nil
}

func (v *protocolVerificationTx) Exec(ctx context.Context, query string, args ...any) error {
	if err := v.lock(); err != nil {
		return err
	}
	defer v.mu.Unlock()
	_, err := v.tx.Exec(ctx, query, verificationArguments(args)...)
	return err
}
func (v *protocolVerificationTx) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	if err := v.lock(); err != nil {
		return err
	}
	defer v.mu.Unlock()
	return v.tx.QueryRow(ctx, query, verificationArguments(args)...).Scan(dest...)
}
func (v *protocolVerificationTx) Each(ctx context.Context, query string, args []any, visit func(func(...any) error) error) error {
	if visit == nil {
		return errors.New("pgxdriver: verification row callback is required")
	}
	if err := v.lock(); err != nil {
		return err
	}
	defer v.mu.Unlock()
	rows, err := v.tx.Query(ctx, query, verificationArguments(args)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := visit(rows.Scan); err != nil {
			rows.Close()
			return errors.Join(err, rows.Err())
		}
	}
	return rows.Err()
}
func (v *protocolVerificationTx) Describe(ctx context.Context, name, query string) (out sqlcheck.Description, err error) {
	if name == "" || len(name) > 63 || strings.ContainsRune(name, 0) {
		return out, fmt.Errorf("pgxdriver: invalid verification statement name")
	}
	if err := v.lock(); err != nil {
		return out, err
	}
	defer v.mu.Unlock()
	conn := v.tx.Conn().PgConn()
	result, err := conn.Prepare(ctx, name, query, nil)
	created := err == nil
	if failure, ok := errors.AsType[*pgconn.PrepareError](err); ok && failure != nil {
		created = failure.ParseComplete
	}
	if created {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			err = errors.Join(err, conn.Deallocate(cleanup, name))
		}()
	}
	if err != nil {
		return out, err
	}
	out.Parameters = slices.Clone(result.ParamOIDs)
	out.Fields = make([]sqlcheck.DescribedField, len(result.Fields))
	for i, field := range result.Fields {
		out.Fields[i] = sqlcheck.DescribedField{
			Name: field.Name, TypeOID: field.DataTypeOID, TypeMod: field.TypeModifier,
			TableOID: field.TableOID, Attribute: int16(field.TableAttributeNumber),
		}
	}
	return out, nil
}
func (v *protocolVerificationTx) Rollback(ctx context.Context) error {
	if err := v.lock(); err != nil {
		return err
	}
	defer v.mu.Unlock()
	v.closed.Store(true)
	return v.tx.Rollback(ctx)
}

func verificationArguments(args []any) []any {
	return append([]any{pgx.QueryExecModeExec}, args...)
}
