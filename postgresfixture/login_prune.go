package postgresfixture

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const loginMark = "tabula fixture login v1 "
const idleMark = " idle "

// PostgreSQL roles are cluster-wide, advisory locks are local to one database.
// Check both identities so another maintenance database or a replacement role
// cannot be mistaken for an orphan. The name format alone never establishes ownership.
const managedLogin = `r.rolname COLLATE "C" ~ '` + loginNamePattern + `'
	AND pg_catalog.split_part(pg_catalog.shobj_description(r.oid, 'pg_authid'), '` + idleMark + `', 1) = '` + loginMark + `' ||
		(SELECT d.oid::text FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()) ||
		'/' || r.oid::text`

type loginCandidate struct {
	name string
	oid  int64
}

// PruneLoginRoles removes abandoned fixture logins from this maintenance
// database's namespace, including logins with no surviving clone. A positive
// maxIdle starts at the first prune that observes both the lease and all client
// sessions gone. Zero reclaims them immediately. It returns the names removed
// by this call and any cleanup errors. Fixture.Prune applies its own maxIdle.
func (f Fixture) PruneLoginRoles(ctx context.Context, maxIdle time.Duration) ([]string, error) {
	if maxIdle < 0 {
		return nil, errors.New("login prune requires a nonnegative maxIdle")
	}
	if f.Admin == nil {
		return nil, errors.New("fixture login cleanup requires Admin")
	}
	conn, err := f.Admin.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return pruneLogins(ctx, conn, maxIdle)
}

func loginCandidates(ctx context.Context, conn *sql.Conn) ([]loginCandidate, error) {
	rows, err := conn.QueryContext(ctx, `SELECT r.rolname,r.oid FROM pg_catalog.pg_roles r WHERE `+managedLogin+` ORDER BY r.rolname`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var candidates []loginCandidate
	for rows.Next() {
		var c loginCandidate
		if err := rows.Scan(&c.name, &c.oid); err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

func pruneLogins(ctx context.Context, conn *sql.Conn, maxIdle time.Duration) ([]string, error) {
	candidates, err := loginCandidates(ctx, conn)
	if err != nil {
		return nil, err
	}
	var dropped []string
	var errs []error
	for _, c := range candidates {
		ok, err := removeLogin(ctx, conn, c, &maxIdle)
		if err != nil {
			errs = append(errs, fmt.Errorf("prune login %s: %w", c.name, err))
		}
		if ok {
			dropped = append(dropped, c.name)
		}
	}
	return dropped, errors.Join(errs...)
}

// Pruners take the transaction lock without waiting. Release may wait for
// an earlier cleanup attempt. A pinned owner can acquire its own lock.
// A nil maxIdle means an explicit release.
func removeLogin(ctx context.Context, conn *sql.Conn, c loginCandidate, maxIdle *time.Duration) (bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if maxIdle != nil {
		var locked bool
		err = tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_try_advisory_xact_lock(pg_catalog.hashtextextended($1,0))", c.name).Scan(&locked)
		if err != nil || !locked {
			return false, err
		}
	} else if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))", c.name); err != nil {
		return false, err
	}
	var owned, active bool
	var comment string
	var now time.Time
	err = tx.QueryRowContext(ctx, `SELECT coalesce((`+managedLogin+`),false),
		EXISTS(SELECT FROM pg_catalog.pg_stat_activity a WHERE a.usesysid = r.oid),
		coalesce(pg_catalog.shobj_description(r.oid,'pg_authid'),''), pg_catalog.clock_timestamp()
		FROM pg_catalog.pg_roles r WHERE r.oid=$1 AND r.rolname=$2`, c.oid, c.name).Scan(&owned, &active, &comment, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !owned {
		return false, fmt.Errorf("ownership mark changed for fixture login %s", c.name)
	}
	if maxIdle != nil {
		eligible, mark, err := fixtureIdle(comment, active, now, *maxIdle)
		if err != nil {
			return false, err
		}
		if mark != "" {
			if err := commentLogin(ctx, tx, c.name, mark); err != nil {
				return false, err
			}
			return false, tx.Commit()
		}
		if !eligible {
			return false, nil
		}
	} else if active {
		return false, fmt.Errorf("fixture login %s still has open sessions", c.name)
	}
	grants, err := connectGrants(ctx, tx, c.oid)
	if err != nil {
		return false, err
	}
	for _, name := range grants {
		if _, err := tx.ExecContext(ctx, "REVOKE CONNECT ON DATABASE "+quoteLoginIdent(name)+" FROM "+quoteLoginIdent(c.name)+" RESTRICT"); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, "DROP ROLE "+quoteLoginIdent(c.name)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// The identity portion of comment has already been verified under the lock.
// A nonempty mark is written instead of dropping the object on this pass.
func fixtureIdle(comment string, active bool, now time.Time, maxIdle time.Duration) (eligible bool, mark string, err error) {
	base, stamp, marked := strings.Cut(comment, idleMark)
	var since time.Time
	if marked {
		since, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return false, "", fmt.Errorf("invalid fixture idle mark: %w", err)
		}
	}
	switch {
	case active && marked:
		return false, base, nil
	case active:
		return false, "", nil
	case maxIdle == 0:
		return true, "", nil
	case !marked:
		return false, base + idleMark + now.UTC().Format(time.RFC3339Nano), nil
	default:
		return now.Sub(since) >= maxIdle, "", nil
	}
}

func connectGrants(ctx context.Context, tx *sql.Tx, oid int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT d.datname FROM pg_catalog.pg_database d
		CROSS JOIN LATERAL pg_catalog.aclexplode(d.datacl) acl
		WHERE acl.grantee=$1 AND acl.privilege_type='CONNECT' ORDER BY d.datname`, oid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
