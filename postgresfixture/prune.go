package postgresfixture

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

const (
	templatePrefix = "tabula_tpl_"
	stagePrefix    = "tabula_stg_"
	// usedMark prefixes the database comment recording the last Clone, in
	// UTC from the server clock.
	usedMark = "tabula fixture template; last used "
)

// stageName is the staging database protected by the publication lock.
func stageName(template string) string {
	return stagePrefix + strings.TrimPrefix(template, templatePrefix)
}

func databaseExists(ctx context.Context, conn *sql.Conn, name string) (bool, error) {
	var exists bool
	err := conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT FROM pg_catalog.pg_database WHERE datname=$1)", name).Scan(&exists)
	return exists, err
}

// markUsed records the server time of the latest Clone on the template.
func markUsed(ctx context.Context, conn *sql.Conn, template string) error {
	var stmt string
	if err := conn.QueryRowContext(ctx, `SELECT pg_catalog.format('COMMENT ON DATABASE %I IS %L', $1::text,
		$2::text || pg_catalog.to_char(pg_catalog.now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'))`, template, usedMark).Scan(&stmt); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, stmt)
	return err
}

const usageQuery = `SELECT d.datname, coalesce(pg_catalog.shobj_description(d.oid, 'pg_database'), ''), pg_catalog.now()
	FROM pg_catalog.pg_database AS d`

type pruneCandidate struct {
	name    string
	comment string
	now     time.Time
}

func (c pruneCandidate) eligible(maxIdle time.Duration) bool {
	if strings.HasPrefix(c.name, stagePrefix) {
		return true
	}
	stamp, ok := strings.CutPrefix(c.comment, usedMark)
	if !ok {
		return false
	}
	lastUsed, err := time.Parse(time.RFC3339Nano, stamp)
	return err == nil && lastUsed.Before(c.now.Add(-maxIdle))
}

// Prune drops cached templates whose last Clone is older than maxIdle and
// staging databases abandoned by interrupted builds, except the templates of
// keep. Each database is dropped through Backend.Drop while holding the same
// advisory lock that Clone holds through the physical copy. Templates with
// missing or invalid last-use marks are kept. Prune returns the names it
// dropped, including partial results if another drop failed.
func (f Fixture) Prune(ctx context.Context, maxIdle time.Duration, keep ...Installation) ([]string, error) {
	if f.Admin == nil || f.Backend == nil {
		return nil, errors.New("fixture requires Admin and Backend")
	}
	if maxIdle <= 0 {
		return nil, errors.New("prune requires a positive maxIdle")
	}
	kept := map[string]bool{}
	for _, p := range keep {
		name, err := p.TemplateName()
		if err != nil {
			return nil, err
		}
		kept[name] = true
	}
	conn, err := f.Admin.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	candidates, err := pruneCandidates(ctx, conn)
	if err != nil {
		return nil, err
	}
	var dropped []string
	var errs []error
	for _, c := range candidates {
		if kept[c.name] || !c.eligible(maxIdle) {
			continue
		}
		ok, err := f.pruneLocked(ctx, conn, c.name, maxIdle)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			dropped = append(dropped, c.name)
		}
	}
	return dropped, errors.Join(errs...)
}

func pruneCandidates(ctx context.Context, conn *sql.Conn) ([]pruneCandidate, error) {
	// Only these names can be produced by TemplateName and stageName.
	rows, err := conn.QueryContext(ctx, usageQuery+` WHERE d.datname COLLATE "C" ~ '^tabula_(tpl|stg)_[0-9a-f]{48}$'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []pruneCandidate
	for rows.Next() {
		var c pruneCandidate
		if err := rows.Scan(&c.name, &c.comment, &c.now); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Re-read this database under its template lock. A concurrent worker may
// have refreshed the template or removed the stage since the initial scan.
func (f Fixture) pruneLocked(ctx context.Context, conn *sql.Conn, name string, maxIdle time.Duration) (dropped bool, err error) {
	template := name
	if digest, stage := strings.CutPrefix(name, stagePrefix); stage {
		template = templatePrefix + digest
	}
	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_catalog.pg_try_advisory_lock(pg_catalog.hashtextextended($1,0))", template).Scan(&locked); err != nil {
		discardConn(conn)
		return false, err
	}
	if !locked {
		return false, nil
	}
	defer func() {
		if e := releaseLock(ctx, conn, unlockExclusive, template); e != nil {
			err = errors.Join(err, e)
		}
	}()
	var current pruneCandidate
	err = conn.QueryRowContext(ctx, usageQuery+` WHERE d.datname=$1`, name).Scan(&current.name, &current.comment, &current.now)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || !current.eligible(maxIdle) {
		return false, err
	}
	if err := f.Backend.Drop(ctx, conn, name); err != nil {
		return false, err
	}
	return true, nil
}
