package postgresfixture

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/0xJean/tabula"
)

const (
	cloneComment = "coalesce(pg_catalog.shobj_description(d.oid, 'pg_database'), '')"
	cloneFrom    = ` FROM pg_catalog.pg_database d, pg_catalog.pg_database m
		WHERE m.datname=pg_catalog.current_database() AND d.datname COLLATE "C" ~ '^` + clonePrefix + `[0-9a-f]{24}$'`
	cloneCatalog = "SELECT d.oid,m.oid," + cloneComment + cloneFrom
)

var validLoginName = regexp.MustCompile(loginNamePattern)

type leasedClone struct {
	name  string
	oid   int64
	login string
	mark  string
}

func cloneOwner(maintenanceOID, oid int64, name, base string) (string, bool) {
	login, ok := strings.CutPrefix(base, cloneIdentity(maintenanceOID, oid, name, ""))
	return login, ok && validLoginName.MatchString(login)
}

// PruneClones removes abandoned CloneWithLease databases owned by this
// maintenance database. A live lease or any connection preserves the clone.
// A positive maxIdle starts at the first observed idle sweep; zero requests
// immediate cleanup. It returns names removed by this call and any errors.
func (f Fixture) PruneClones(ctx context.Context, maxIdle time.Duration) ([]string, error) {
	if maxIdle < 0 {
		return nil, errors.New("clone prune requires a nonnegative maxIdle")
	}
	if f.Admin == nil {
		return nil, errors.New("fixture clone cleanup requires Admin")
	}
	conn, err := f.Admin.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return pruneClones(ctx, conn, maxIdle)
}

func cloneCandidates(ctx context.Context, conn *sql.Conn) ([]leasedClone, error) {
	rows, err := conn.QueryContext(ctx, "SELECT d.datname,d.oid,m.oid,"+cloneComment+cloneFrom+
		" AND "+cloneComment+" LIKE $1 ORDER BY d.datname", cloneMark+"%")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []leasedClone
	for rows.Next() {
		var c leasedClone
		var maintenanceOID int64
		var comment string
		if err := rows.Scan(&c.name, &c.oid, &maintenanceOID, &comment); err != nil {
			return nil, err
		}
		c.mark, _, _ = strings.Cut(comment, idleMark)
		login, ok := cloneOwner(maintenanceOID, c.oid, c.name, c.mark)
		if ok {
			c.login = login
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

func pruneClones(ctx context.Context, conn *sql.Conn, maxIdle time.Duration) ([]string, error) {
	candidates, err := cloneCandidates(ctx, conn)
	if err != nil {
		return nil, err
	}
	var dropped []string
	var errs []error
	for _, c := range candidates {
		ok, err := c.prune(ctx, conn, maxIdle)
		if err != nil {
			errs = append(errs, fmt.Errorf("prune clone %s: %w", c.name, err))
		}
		if ok {
			dropped = append(dropped, c.name)
		}
	}
	return dropped, errors.Join(errs...)
}

func (c leasedClone) prune(ctx context.Context, conn *sql.Conn, maxIdle time.Duration) (dropped bool, err error) {
	// Acquire in this order. The original login or another pruner can hold
	// the first lock. Explicit Database.Drop takes only the second lock.
	for _, key := range []string{c.login, c.name} {
		var locked bool
		locked, err = tryExclusive(ctx, conn, key)
		if err != nil || !locked {
			return false, err
		}
		defer func() { err = errors.Join(err, releaseLock(ctx, conn, unlockExclusive, key)) }()
	}
	var comment string
	var active bool
	var now time.Time
	err = conn.QueryRowContext(ctx, "SELECT "+cloneComment+`,
		EXISTS(SELECT FROM pg_catalog.pg_stat_activity a WHERE a.datid=d.oid), pg_catalog.clock_timestamp()
		FROM pg_catalog.pg_database d WHERE d.datname=$1 AND d.oid=$2`, c.name, c.oid).Scan(&comment, &active, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	base, _, _ := strings.Cut(comment, idleMark)
	if base != c.mark {
		return false, nil
	}
	eligible, mark, err := fixtureIdle(comment, active, now, maxIdle)
	if err != nil {
		return false, err
	}
	if mark != "" {
		return false, commentClone(ctx, conn, c.name, mark)
	}
	if !eligible {
		return false, nil
	}
	// A client can connect after the activity check. Let PostgreSQL arbitrate
	// this race; a host Backend.Drop may use FORCE and disconnect that client.
	_, err = conn.ExecContext(ctx, "DROP DATABASE "+quoteLoginIdent(c.name))
	if tabula.SQLState(err) == "55006" {
		if comment != base {
			return false, commentClone(ctx, conn, c.name, base)
		}
		return false, nil
	}
	return err == nil, err
}
