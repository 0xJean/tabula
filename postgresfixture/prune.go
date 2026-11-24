package postgresfixture

import (
	"context"
	"database/sql"
	"database/sql/driver"
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

// stageName is the staging database of template; only the holder of the
// template lock creates it.
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
		$2::text || pg_catalog.to_char(pg_catalog.now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'))`, template, usedMark).Scan(&stmt); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, stmt)
	return err
}

// idleQuery reports, for each cached template or stage, whether its last-used
// mark is older than $2 seconds on the server clock. A missing or malformed
// mark counts as in use.
const idleQuery = `SELECT d.datname, d.datname LIKE 'tabula\_stg\_%',
	CASE WHEN c.comment ~ ('^' || $1 || '\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$')
		THEN pg_catalog.substr(c.comment, pg_catalog.length($1) + 1)::timestamptz < pg_catalog.now() - pg_catalog.make_interval(secs => $2)
		ELSE false END
	FROM pg_catalog.pg_database d
	CROSS JOIN LATERAL (SELECT coalesce(pg_catalog.shobj_description(d.oid, 'pg_database'), '') AS comment) c
	WHERE d.datname LIKE 'tabula\_tpl\_%' OR d.datname LIKE 'tabula\_stg\_%'`

type pruneCandidate struct {
	name  string
	stage bool
	idle  bool
}

// Prune drops cached templates whose last Clone is older than maxIdle and
// staging databases abandoned by interrupted builds, except the templates of
// keep. Each database is dropped through Backend.Drop while holding the same
// advisory lock that Clone takes, so a template being built or published is
// skipped; a Clone that starts after the drop rebuilds the template. Prune
// returns the dropped database names.
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
	candidates, err := pruneCandidates(ctx, conn, maxIdle)
	if err != nil {
		return nil, err
	}
	var dropped []string
	var errs []error
	for _, c := range candidates {
		template := c.name
		if c.stage {
			template = templatePrefix + strings.TrimPrefix(c.name, stagePrefix)
		} else if kept[c.name] || !c.idle {
			continue
		}
		ok, err := f.pruneLocked(ctx, conn, template, c, maxIdle)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			dropped = append(dropped, c.name)
		}
	}
	return dropped, errors.Join(errs...)
}

func pruneCandidates(ctx context.Context, conn *sql.Conn, maxIdle time.Duration) ([]pruneCandidate, error) {
	rows, err := conn.QueryContext(ctx, idleQuery, usedMark, maxIdle.Seconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []pruneCandidate
	for rows.Next() {
		var c pruneCandidate
		if err := rows.Scan(&c.name, &c.stage, &c.idle); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// pruneLocked drops c under the template lock when no Clone holds it,
// re-reading the template's mark so a Clone since the scan keeps it.
func (f Fixture) pruneLocked(ctx context.Context, conn *sql.Conn, template string, c pruneCandidate, maxIdle time.Duration) (dropped bool, err error) {
	var locked bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_catalog.pg_try_advisory_lock(pg_catalog.hashtextextended($1,0))", template).Scan(&locked); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return false, err
	}
	if !locked {
		return false, nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var unlocked bool
		e := conn.QueryRowContext(cleanup, "SELECT pg_catalog.pg_advisory_unlock(pg_catalog.hashtextextended($1,0))", template).Scan(&unlocked)
		if e != nil || !unlocked {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, errors.New("template advisory lock was not released"), e)
		}
	}()
	if !c.stage {
		current, err := pruneCandidates(ctx, conn, maxIdle)
		if err != nil {
			return false, err
		}
		idle := false
		for _, x := range current {
			if x.name == c.name {
				idle = x.idle
			}
		}
		if !idle {
			return false, nil
		}
	}
	if err := f.Backend.Drop(ctx, conn, c.name); err != nil {
		return false, err
	}
	return true, nil
}
