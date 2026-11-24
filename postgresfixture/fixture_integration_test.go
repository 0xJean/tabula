//go:build integration

package postgresfixture_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/0xJean/tabula/postgresfixture"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type installer struct {
	dsn      string
	installs atomic.Int32
	fail     atomic.Bool
}

func (b *installer) Clone(ctx context.Context, c *sql.Conn, from, to string) error {
	_, err := c.ExecContext(ctx, "CREATE DATABASE "+pq.QuoteIdentifier(to)+" TEMPLATE "+pq.QuoteIdentifier(from))
	return err
}
func (b *installer) Drop(ctx context.Context, c *sql.Conn, name string) error {
	_, err := c.ExecContext(ctx, "DROP DATABASE "+pq.QuoteIdentifier(name))
	return err
}
func (b *installer) open(name string) (*sql.DB, error) {
	cfg, err := pq.NewConfig(b.dsn)
	if err != nil {
		return nil, err
	}
	cfg.Database = name
	c, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}
func (b *installer) Install(ctx context.Context, name string, p postgresfixture.Installation) error {
	b.installs.Add(1)
	db, err := b.open(name)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, string(p.Base)); err != nil {
		return err
	}
	if b.fail.Swap(false) {
		return errors.New("installer stopped")
	}
	for _, m := range p.Migrations {
		if _, err := db.ExecContext(ctx, string(m.Content)); err != nil {
			return err
		}
	}
	return nil
}

func TestTemplatePublicationAndIsolation(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	b := &installer{dsn: dsn}
	f := postgresfixture.Fixture{Admin: db, Backend: b}
	full := postgresfixture.Installation{Revision: fmt.Sprintf("test-%d", time.Now().UnixNano()), Base: []byte("CREATE TABLE notes(id int)"),
		Migrations: []postgresfixture.Migration{{Name: "01", Content: []byte("INSERT INTO notes VALUES(1)")}, {Name: "02", Content: []byte("INSERT INTO notes VALUES(2)")}}}
	first, err := full.Through("01")
	require.NoError(t, err)
	for _, p := range []postgresfixture.Installation{full, first} {
		n, err := p.TemplateName()
		require.NoError(t, err)
		t.Cleanup(func() { _, err := db.Exec("DROP DATABASE IF EXISTS " + pq.QuoteIdentifier(n)); require.NoError(t, err) })
	}
	b.fail.Store(true)
	_, err = f.Clone(ctx, first)
	require.ErrorContains(t, err, "installer stopped")
	names := make([]postgresfixture.Database, 2)
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i := range names {
		wg.Go(func() { names[i], errs[i] = f.Clone(ctx, first) })
	}
	wg.Wait()
	for i, e := range errs {
		require.NoError(t, e)
		t.Cleanup(func() { require.NoError(t, names[i].Drop(context.Background())) })
	}
	require.Equal(t, int32(2), b.installs.Load())
	require.NotEqual(t, names[0].Name(), names[1].Name())
	clone, err := f.Clone(ctx, full)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, clone.Drop(context.Background())) })
	check := func(d postgresfixture.Database, want int) {
		c, err := b.open(d.Name())
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		var count int
		require.NoError(t, c.QueryRowContext(ctx, "SELECT count(*) FROM notes").Scan(&count))
		require.Equal(t, want, count)
		_, err = c.ExecContext(ctx, "INSERT INTO notes VALUES (3)")
		require.NoError(t, err)
	}
	check(names[0], 1)
	check(names[1], 1)
	check(clone, 2)
}

func TestPruneDropsIdleTemplatesAndAbandonedStages(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	b := &installer{dsn: dsn}
	f := postgresfixture.Fixture{Admin: db, Backend: b}
	plan := func(label string) (postgresfixture.Installation, string) {
		p := postgresfixture.Installation{Revision: fmt.Sprintf("prune-%s-%d", label, time.Now().UnixNano()), Base: []byte("CREATE TABLE notes(id int)")}
		name, err := p.TemplateName()
		require.NoError(t, err)
		stage := "tabula_stg_" + name[len("tabula_tpl_"):]
		t.Cleanup(func() {
			for _, n := range []string{name, stage} {
				_, err := db.Exec("DROP DATABASE IF EXISTS " + pq.QuoteIdentifier(n))
				require.NoError(t, err)
			}
		})
		return p, name
	}
	clone := func(p postgresfixture.Installation) {
		d, err := f.Clone(ctx, p)
		require.NoError(t, err)
		require.NoError(t, d.Drop(ctx))
	}
	stale := func(name string) {
		_, err := db.Exec("COMMENT ON DATABASE " + pq.QuoteIdentifier(name) + " IS 'tabula fixture template; last used 2000-01-01T00:00:00Z'")
		require.NoError(t, err)
	}
	kept, keptName := plan("kept")
	recent, recentName := plan("recent")
	idle, idleName := plan("idle")
	busy, busyName := plan("busy")
	_, abandonedName := plan("abandoned")
	for _, p := range []postgresfixture.Installation{kept, recent, idle, busy} {
		clone(p)
	}
	stale(keptName)
	stale(idleName)
	stale(busyName)
	abandonedStage := "tabula_stg_" + abandonedName[len("tabula_tpl_"):]
	_, err = db.Exec("CREATE DATABASE " + pq.QuoteIdentifier(abandonedStage))
	require.NoError(t, err)
	holder, err := db.Conn(ctx)
	require.NoError(t, err)
	_, err = holder.ExecContext(ctx, "SELECT pg_advisory_lock(hashtextextended($1,0))", busyName)
	require.NoError(t, err)

	dropped, err := f.Prune(ctx, time.Hour, kept)
	require.NoError(t, err)
	require.Contains(t, dropped, idleName)
	require.Contains(t, dropped, abandonedStage)
	for _, name := range []string{keptName, recentName, busyName} {
		require.NotContains(t, dropped, name)
	}

	_, err = holder.ExecContext(ctx, "SELECT pg_advisory_unlock(hashtextextended($1,0))", busyName)
	require.NoError(t, err)
	require.NoError(t, holder.Close())
	dropped, err = f.Prune(ctx, time.Hour)
	require.NoError(t, err)
	require.Contains(t, dropped, busyName)
	require.Contains(t, dropped, keptName)
	require.NotContains(t, dropped, recentName)

	installs := b.installs.Load()
	clone(idle)
	require.Equal(t, installs+1, b.installs.Load(), "a pruned template is rebuilt on the next Clone")
	clone(idle)
	require.Equal(t, installs+1, b.installs.Load(), "the rebuilt template is cached again")
}

func TestCloneReplacesAnAbandonedStage(t *testing.T) {
	ctx := t.Context()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	f := postgresfixture.Fixture{Admin: db, Backend: &installer{dsn: dsn}}
	p := postgresfixture.Installation{Revision: fmt.Sprintf("stage-%d", time.Now().UnixNano()), Base: []byte("CREATE TABLE notes(id int)")}
	name, err := p.TemplateName()
	require.NoError(t, err)
	stage := "tabula_stg_" + name[len("tabula_tpl_"):]
	t.Cleanup(func() {
		for _, n := range []string{name, stage} {
			_, err := db.Exec("DROP DATABASE IF EXISTS " + pq.QuoteIdentifier(n))
			require.NoError(t, err)
		}
	})
	_, err = db.Exec("CREATE DATABASE " + pq.QuoteIdentifier(stage))
	require.NoError(t, err)
	d, err := f.Clone(ctx, p)
	require.NoError(t, err)
	require.NoError(t, d.Drop(ctx))
	var stages int
	require.NoError(t, db.QueryRow("SELECT count(*) FROM pg_database WHERE datname=$1", stage).Scan(&stages))
	require.Zero(t, stages, "the build removes its stage after publishing")
}
