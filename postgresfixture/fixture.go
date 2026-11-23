// Package postgresfixture caches installed PostgreSQL test databases by content
// and gives each test a fresh clone. The application supplies installation and
// database lifecycle operations on its disposable PostgreSQL server.
package postgresfixture

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"slices"
	"time"
)

// Migration carries the stable name and exact input bytes of one migration.
type Migration struct {
	Name    string
	Content []byte
}

// Installation describes an ordered install. Include base dumps, install
// settings and installer revisions affecting the result in Base and Revision.
type Installation struct {
	Revision   string
	Base       []byte
	Migrations []Migration
}

func (p Installation) validate() error {
	if p.Revision == "" {
		return errors.New("installation revision is required")
	}
	seen := map[string]bool{}
	for _, m := range p.Migrations {
		if m.Name == "" || seen[m.Name] {
			return fmt.Errorf("empty or duplicate migration %q", m.Name)
		}
		seen[m.Name] = true
	}
	return nil
}

// Through returns the inclusive prefix ending at name, preserving install order.
func (p Installation) Through(name string) (Installation, error) {
	if err := p.validate(); err != nil {
		return Installation{}, err
	}
	for i, m := range p.Migrations {
		if m.Name == name {
			p.Migrations = slices.Clip(p.Migrations[:i+1])
			return p, nil
		}
	}
	return Installation{}, fmt.Errorf("unknown migration %q", name)
}

func hashPart(h hash.Hash, b []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(b)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(b)
}

// TemplateName hashes length-framed install inputs into a PostgreSQL identifier.
func (p Installation) TemplateName() (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	h := sha256.New()
	hashPart(h, []byte(p.Revision))
	hashPart(h, p.Base)
	for _, m := range p.Migrations {
		hashPart(h, []byte(m.Name))
		hashPart(h, m.Content)
	}
	return "tabula_tpl_" + hex.EncodeToString(h.Sum(nil)[:24]), nil
}

// Backend is supplied by the test suite. Clone and Drop use the supplied
// maintenance connection in autocommit. Install must disconnect from its
// target before returning so PostgreSQL can clone the installed database.
type Backend interface {
	Clone(ctx context.Context, admin *sql.Conn, source, target string) error
	Install(ctx context.Context, target string, plan Installation) error
	Drop(ctx context.Context, admin *sql.Conn, target string) error
}

// Fixture uses an explicit maintenance database and host-defined backend.
type Fixture struct {
	Admin   *sql.DB
	Backend Backend
}

// Database is an isolated per-test clone.
type Database struct {
	name    string
	fixture Fixture
}

// Name is the database name to use in the test connection string.
func (d Database) Name() string { return d.name }

// Drop removes this clone through its host backend.
func (d Database) Drop(ctx context.Context) error {
	if d.fixture.Admin == nil {
		return errors.New("uninitialized fixture database")
	}
	conn, err := d.fixture.Admin.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return d.fixture.Backend.Drop(ctx, conn, d.name)
}

func freshName(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// Clone installs a template once per digest and clones it for one test. A
// session-level advisory lock serializes publishers across processes. Only a
// completely installed staging database is copied to the stable cache name.
func (f Fixture) Clone(ctx context.Context, p Installation) (Database, error) {
	if f.Admin == nil || f.Backend == nil {
		return Database{}, errors.New("fixture requires Admin and Backend")
	}
	template, err := p.TemplateName()
	if err != nil {
		return Database{}, err
	}
	conn, err := f.Admin.Conn(ctx)
	if err != nil {
		return Database{}, err
	}
	defer func() { _ = conn.Close() }()
	if err := f.prepare(ctx, conn, template, p); err != nil {
		return Database{}, err
	}
	name, err := freshName("tabula_test_")
	if err != nil {
		return Database{}, err
	}
	if err := f.Backend.Clone(ctx, conn, template, name); err != nil {
		return Database{}, err
	}
	return Database{name: name, fixture: f}, nil
}

func (f Fixture) prepare(ctx context.Context, conn *sql.Conn, template string, p Installation) (err error) {
	// An interrupted lock request may have acquired its lock on the server.
	discard := func() { _ = conn.Raw(func(any) error { return driver.ErrBadConn }) }
	if _, err = conn.ExecContext(ctx, "SELECT pg_catalog.pg_advisory_lock(pg_catalog.hashtextextended($1,0))", template); err != nil {
		discard()
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		var unlocked bool
		e := conn.QueryRowContext(cleanup, "SELECT pg_catalog.pg_advisory_unlock(pg_catalog.hashtextextended($1,0))", template).Scan(&unlocked)
		if e != nil || !unlocked {
			discard()
			err = errors.Join(err, errors.New("template advisory lock was not released"), e)
		}
	}()
	var exists bool
	if err = conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT FROM pg_catalog.pg_database WHERE datname=$1)", template).Scan(&exists); err != nil || exists {
		return err
	}
	stage, err := freshName("tabula_stage_")
	if err != nil {
		return err
	}
	if err = f.Backend.Clone(ctx, conn, "template0", stage); err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		err = errors.Join(err, f.Backend.Drop(cleanup, conn, stage))
	}()
	if err = f.Backend.Install(ctx, stage, p); err != nil {
		return err
	}
	return f.Backend.Clone(ctx, conn, stage, template)
}
