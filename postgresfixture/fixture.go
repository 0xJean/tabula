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
	return templatePrefix + hex.EncodeToString(h.Sum(nil)[:24]), nil
}

// Backend is supplied by the test suite. Clone and Drop use the supplied
// maintenance connection in autocommit. Install must disconnect from its
// target before returning so PostgreSQL can clone the installed database.
// Fixture.Prune also calls Drop for cached templates and abandoned staging
// databases, so Drop must accept a template (clearing IS_TEMPLATE if Clone set
// it) and may release host resources tied to it.
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
	mark    string // Nonempty for a clone bound to a process login.
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
	if d.mark != "" {
		return d.dropLeased(ctx, conn)
	}
	return d.fixture.Backend.Drop(ctx, conn, d.name)
}

func freshName(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

const (
	lockExclusive   = "SELECT pg_catalog.pg_advisory_lock(pg_catalog.hashtextextended($1,0))"
	unlockExclusive = "SELECT pg_catalog.pg_advisory_unlock(pg_catalog.hashtextextended($1,0))"
	lockShared      = "SELECT pg_catalog.pg_advisory_lock_shared(pg_catalog.hashtextextended($1,0))"
	unlockShared    = "SELECT pg_catalog.pg_advisory_unlock_shared(pg_catalog.hashtextextended($1,0))"
)

func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}

func acquireLock(ctx context.Context, conn *sql.Conn, query, key string) error {
	_, err := conn.ExecContext(ctx, query, key)
	if err != nil {
		// A canceled request may have acquired the lock on the server.
		discardConn(conn)
	}
	return err
}

func tryExclusive(ctx context.Context, conn *sql.Conn, key string) (bool, error) {
	var locked bool
	err := conn.QueryRowContext(ctx, "SELECT pg_catalog.pg_try_advisory_lock(pg_catalog.hashtextextended($1,0))", key).Scan(&locked)
	if err != nil {
		discardConn(conn)
	}
	return locked, err
}

func releaseLock(ctx context.Context, conn *sql.Conn, query, key string) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var unlocked bool
	err := conn.QueryRowContext(cleanup, query, key).Scan(&unlocked)
	if err != nil || !unlocked {
		discardConn(conn)
		return errors.Join(errors.New("fixture advisory lock was not released"), err)
	}
	return nil
}

// Clone installs a template once per digest and clones it for one test. A
// shared advisory lock protects the template through the physical copy,
// allowing concurrent copies. Publication is serialized on the staging name.
func (f Fixture) Clone(ctx context.Context, p Installation) (Database, error) {
	return f.clone(ctx, p, nil)
}

func (f Fixture) clone(ctx context.Context, p Installation, login *LoginRole) (Database, error) {
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
	if login != nil {
		if err := login.checkCloneLease(ctx, conn); err != nil {
			return Database{}, err
		}
	}
	if err := acquireLock(ctx, conn, lockShared, template); err != nil {
		return Database{}, err
	}
	// If unlocking fails, releaseLock discards the connection. A successfully
	// created database still belongs to the caller and can be dropped normally.
	defer func() { _ = releaseLock(ctx, conn, unlockShared, template) }()
	if err := f.prepare(ctx, conn, template, p); err != nil {
		return Database{}, err
	}
	name, err := freshName(clonePrefix)
	if err != nil {
		return Database{}, err
	}
	if err := f.Backend.Clone(ctx, conn, template, name); err != nil {
		return Database{}, err
	}
	return Database{name: name, fixture: f}, nil
}

func (f Fixture) prepare(ctx context.Context, conn *sql.Conn, template string, p Installation) (err error) {
	stage := stageName(template)
	if err = acquireLock(ctx, conn, lockExclusive, stage); err != nil {
		return err
	}
	defer func() {
		if e := releaseLock(ctx, conn, unlockExclusive, stage); e != nil {
			err = errors.Join(err, e)
		}
	}()
	exists, err := databaseExists(ctx, conn, template)
	if err != nil {
		return err
	}
	if exists {
		return markUsed(ctx, conn, template)
	}
	// Only the holder of the publication lock builds the stage, so a stage that
	// already exists was abandoned by an interrupted process.
	if exists, err = databaseExists(ctx, conn, stage); err != nil {
		return err
	}
	if exists {
		if err = f.Backend.Drop(ctx, conn, stage); err != nil {
			return err
		}
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
	if err = f.Backend.Clone(ctx, conn, stage, template); err != nil {
		return err
	}
	return markUsed(ctx, conn, template)
}
