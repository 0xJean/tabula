package postgresfixture

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const loginMark = "tabula fixture login v1 "

// PostgreSQL roles are cluster-wide, advisory locks are local to one database.
// Check both identities so another maintenance database or a replacement role
// cannot be mistaken for an orphan. The name format alone never establishes ownership.
const managedLogin = `r.rolname COLLATE "C" ~ '^tabula_login_[0-9a-f]{24}$'
	AND pg_catalog.shobj_description(r.oid, 'pg_authid') = '` + loginMark + `' ||
		(SELECT d.oid::text FROM pg_catalog.pg_database d WHERE d.datname = pg_catalog.current_database()) ||
		'/' || r.oid::text`

type loginCandidate struct {
	name string
	oid  int64
}

// PruneLoginRoles removes abandoned fixture logins from this maintenance
// database's namespace, including logins with no surviving clone. It returns
// the login names removed by this call and any cleanup errors. Fixture.Prune
// also performs this cleanup when pruning databases.
func (f Fixture) PruneLoginRoles(ctx context.Context) ([]string, error) {
	if f.Admin == nil {
		return nil, errors.New("fixture login cleanup requires Admin")
	}
	conn, err := f.Admin.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return pruneLogins(ctx, conn)
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

func pruneLogins(ctx context.Context, conn *sql.Conn) ([]string, error) {
	candidates, err := loginCandidates(ctx, conn)
	if err != nil {
		return nil, err
	}
	var dropped []string
	var errs []error
	for _, c := range candidates {
		ok, err := removeLogin(ctx, conn, c, true)
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
func removeLogin(ctx context.Context, conn *sql.Conn, c loginCandidate, pruning bool) (bool, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if pruning {
		var locked bool
		err = tx.QueryRowContext(ctx, "SELECT pg_catalog.pg_try_advisory_xact_lock(pg_catalog.hashtextextended($1,0))", c.name).Scan(&locked)
		if err != nil || !locked {
			return false, err
		}
	} else if _, err := tx.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))", c.name); err != nil {
		return false, err
	}
	var owned, active bool
	err = tx.QueryRowContext(ctx, `SELECT coalesce((`+managedLogin+`),false),
		EXISTS(SELECT FROM pg_catalog.pg_stat_activity a WHERE a.usesysid = r.oid)
		FROM pg_catalog.pg_roles r WHERE r.oid=$1 AND r.rolname=$2`, c.oid, c.name).Scan(&owned, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !owned {
		return false, fmt.Errorf("ownership mark changed for fixture login %s", c.name)
	}
	if active {
		if pruning {
			return false, nil
		}
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
