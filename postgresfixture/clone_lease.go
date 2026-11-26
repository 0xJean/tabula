package postgresfixture

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	clonePrefix = "tabula_test_"
	cloneMark   = "tabula fixture clone v1 "
)

// CloneWithLease creates a clone that PruneClones can reclaim after login's
// lease ends. Use a live login from the same maintenance database, close test
// clients and call Database.Drop during normal cleanup. If publication and
// cleanup both fail, the error comes with the database handle for a retry.
func (f Fixture) CloneWithLease(ctx context.Context, p Installation, login *LoginRole) (Database, error) {
	if login == nil || login.lease == nil {
		return Database{}, errors.New("leased clone requires a fixture login")
	}
	login.lease.mu.Lock()
	live := login.lease.holder != nil
	login.lease.mu.Unlock()
	if !live {
		return Database{}, errors.New("fixture login lease has ended")
	}
	db, err := f.clone(ctx, p, login)
	if err != nil {
		return Database{}, err
	}
	db.mark, err = login.markClone(ctx, db.name)
	if err == nil {
		return db, nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, dropErr := f.Admin.ExecContext(cleanup, "DROP DATABASE "+quoteLoginIdent(db.name)); dropErr != nil {
		return db, errors.Join(err, fmt.Errorf("remove unpublished fixture clone %s: %w", db.name, dropErr))
	}
	return Database{}, err
}

// The role OID, its random name and its ownership mark must all resolve through
// the cloning pool. Two pools can use the same maintenance database.
func (r *LoginRole) checkCloneLease(ctx context.Context, conn *sql.Conn) error {
	var owned bool
	err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT FROM pg_catalog.pg_roles r
		WHERE r.oid=$1 AND r.rolname=$2 AND `+managedLogin+")", r.lease.oid, r.name).Scan(&owned)
	if err != nil {
		return err
	}
	if !owned {
		return errors.New("fixture login does not belong to this maintenance database")
	}
	return nil
}

func cloneIdentity(maintenanceOID, cloneOID int64, name, login string) string {
	return fmt.Sprintf("%s%d/%d/%s/%s", cloneMark, maintenanceOID, cloneOID, name, login)
}

// Publish through the original pinned session. A lost lease or concurrent
// Release fails publication; the slow physical copy does not hold this mutex.
func (r *LoginRole) markClone(ctx context.Context, name string) (string, error) {
	r.lease.mu.Lock()
	defer r.lease.mu.Unlock()
	conn := r.lease.holder
	if conn == nil {
		return "", errors.New("fixture login lease has ended")
	}
	var maintenanceOID, cloneOID int64
	err := conn.QueryRowContext(ctx, `SELECT m.oid,d.oid FROM pg_catalog.pg_database m, pg_catalog.pg_database d
		WHERE m.datname=pg_catalog.current_database() AND d.datname=$1`, name).Scan(&maintenanceOID, &cloneOID)
	if err != nil {
		return "", err
	}
	mark := cloneIdentity(maintenanceOID, cloneOID, name, r.name)
	if err := commentClone(ctx, conn, name, mark); err != nil {
		return "", err
	}
	return mark, nil
}

// Marks contain only generated names, validated labels, numeric OIDs and
// timestamps formatted by the fixture.
func commentClone(ctx context.Context, conn *sql.Conn, name, mark string) error {
	_, err := conn.ExecContext(ctx, "COMMENT ON DATABASE "+quoteLoginIdent(name)+" IS '"+mark+"'")
	return err
}

// Serialize an explicit Drop with orphan collection, including a late cleanup
// after the collector has removed the database or the name has been reused.
func (d Database) dropLeased(ctx context.Context, conn *sql.Conn) (err error) {
	if err := acquireLock(ctx, conn, lockExclusive, d.name); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, releaseLock(ctx, conn, unlockExclusive, d.name)) }()
	var oid, maintenanceOID int64
	var comment string
	err = conn.QueryRowContext(ctx, cloneCatalog+" AND d.datname=$1", d.name).Scan(&oid, &maintenanceOID, &comment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	base, _, _ := strings.Cut(comment, idleMark)
	_, owned := cloneOwner(maintenanceOID, oid, d.name, base)
	if !owned || base != d.mark {
		return fmt.Errorf("ownership mark changed for fixture clone %s", d.name)
	}
	return d.fixture.Backend.Drop(ctx, conn, d.name)
}
