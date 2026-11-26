package postgresfixture

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// LoginRoleOptions configures a temporary LOGIN on a disposable cluster.
// MemberOf lists exact catalog names of existing roles. Inherit defaults to
// false. ConnectionLimit defaults to unlimited (nil or -1); zero denies
// ordinary connections. The login receives no administrative attributes.
type LoginRoleOptions struct {
	MemberOf        []string
	Inherit         bool
	ConnectionLimit *int
	// Label optionally appends 1–25 ASCII lowercase letters, digits or
	// underscores to the generated name. Empty preserves the unlabeled form.
	Label string
}

const (
	loginPrefix      = "tabula_login_"
	labelPattern     = "[a-z0-9_]{1,25}"
	loginNamePattern = "^" + loginPrefix + "[0-9a-f]{24}(_" + labelPattern + ")?$"
)

var validLabel = regexp.MustCompile("^" + labelPattern + "$")

func freshLoginName(label string) (string, error) {
	if label != "" && !validLabel.MatchString(label) {
		return "", errors.New("fixture login label must contain 1–25 ASCII lowercase letters, digits or underscores")
	}
	name, err := freshName(loginPrefix)
	if err == nil && label != "" {
		name += "_" + label
	}
	return name, err
}

func (o LoginRoleOptions) attributes() (string, error) {
	attrs := "LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS "
	if o.Inherit {
		attrs += "INHERIT"
	} else {
		attrs += "NOINHERIT"
	}
	if o.ConnectionLimit != nil {
		limit := *o.ConnectionLimit
		if limit < -1 || limit > math.MaxInt32 {
			return "", fmt.Errorf("invalid login connection limit %d", limit)
		}
		attrs += " CONNECTION LIMIT " + strconv.Itoa(limit)
	}
	seen := make(map[string]bool, len(o.MemberOf))
	members := make([]string, 0, len(o.MemberOf))
	for _, member := range o.MemberOf {
		if member == "" || len(member) > 63 || strings.ContainsRune(member, 0) || !utf8.ValidString(member) || seen[member] {
			return "", fmt.Errorf("invalid or duplicate member role %q", member)
		}
		seen[member] = true
		members = append(members, quoteLoginIdent(member))
	}
	if len(members) > 0 {
		attrs += " IN ROLE " + strings.Join(members, ", ")
	}
	return attrs, nil
}

func quoteLoginIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// LoginRole owns a session advisory lock on a dedicated maintenance connection.
// It can be used across multiple clones and test cases until Release. The
// admin pool needs one slot per live login in addition to its working slots.
type LoginRole struct {
	name  string
	lease *loginLease
}

type loginLease struct {
	mu       sync.Mutex
	admin    *sql.DB
	holder   *sql.Conn
	oid      int64
	password string
	released bool
}

// Name returns the generated PostgreSQL login name.
func (r *LoginRole) Name() string { return r.name }

// Password returns the random login password. It returns an empty string
// after Release has been called. Pass it directly to the test client's
// connection settings and keep credentials out of logs and fixture files.
func (r *LoginRole) Password() string {
	if r == nil || r.lease == nil {
		return ""
	}
	r.lease.mu.Lock()
	defer r.lease.mu.Unlock()
	return r.lease.password
}

// AcquireLoginRole creates a fresh process login. Supply a direct or
// session-pooled connection to the same maintenance database used by Prune.
// The host grants CONNECT to the login when preparing each test database.
// Losing the pinned server connection ends the lease.
func (f Fixture) AcquireLoginRole(ctx context.Context, opts LoginRoleOptions) (*LoginRole, error) {
	if f.Admin == nil {
		return nil, errors.New("fixture login requires Admin")
	}
	attrs, err := opts.attributes()
	if err != nil {
		return nil, err
	}
	name, err := freshLoginName(opts.Label)
	if err != nil {
		return nil, err
	}
	password := rand.Text()
	verifier, err := scramVerifier(password)
	if err != nil {
		return nil, err
	}
	conn, err := f.Admin.Conn(ctx)
	if err != nil {
		return nil, err
	}
	oid, err := createLogin(ctx, conn, name, attrs, verifier)
	if err != nil {
		// Includes an ambiguous commit: a committed role already has its mark.
		discardConn(conn)
		return nil, err
	}
	return &LoginRole{name: name, lease: &loginLease{admin: f.Admin, holder: conn, oid: oid, password: password}}, nil
}

func createLogin(ctx context.Context, conn *sql.Conn, name, attrs, verifier string) (int64, error) {
	// This connection is discarded at lease end. Its session settings and
	// advisory lock must never be inherited by another pool user.
	if _, err := conn.ExecContext(ctx, "SET idle_session_timeout = 0"); err != nil {
		return 0, err
	}
	if _, err := conn.ExecContext(ctx, lockExclusive, name); err != nil {
		return 0, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	// CREATE ROLE accepts no bind parameters. The name and verifier use fixed
	// generated alphabets; attrs quotes each validated membership identifier.
	if _, err = tx.ExecContext(ctx, "CREATE ROLE "+quoteLoginIdent(name)+" "+attrs+" PASSWORD '"+verifier+"'"); err != nil {
		return 0, err
	}
	var oid, dbOID int64
	if err = tx.QueryRowContext(ctx, `SELECT r.oid, d.oid FROM pg_catalog.pg_roles r
		CROSS JOIN pg_catalog.pg_database d WHERE r.rolname=$1 AND d.datname=pg_catalog.current_database()`, name).Scan(&oid, &dbOID); err != nil {
		return 0, err
	}
	if err = commentLogin(ctx, tx, name, fmt.Sprintf("%s%d/%d", loginMark, dbOID, oid)); err != nil {
		return 0, err
	}
	return oid, tx.Commit()
}

// Only validated fixture names, numeric OIDs and server timestamps enter marks.
func commentLogin(ctx context.Context, tx *sql.Tx, name, mark string) error {
	_, err := tx.ExecContext(ctx, "COMMENT ON ROLE "+quoteLoginIdent(name)+" IS '"+mark+"'")
	return err
}

// Release removes the login and its direct database CONNECT grants. Close
// its business connections first. The first call retires the credentials and
// disposes of the pinned backend even on error. A later Release or Prune can
// retry cleanup. Releasing an already removed login succeeds.
func (r *LoginRole) Release(ctx context.Context) error {
	if r == nil || r.lease == nil {
		return errors.New("uninitialized fixture login")
	}
	l := r.lease
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	conn := l.holder
	if conn != nil {
		l.holder, l.password = nil, ""
		defer discardConn(conn)
	} else {
		var err error
		conn, err = l.admin.Conn(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = conn.Close() }()
	}
	_, err := removeLogin(ctx, conn, loginCandidate{name: r.name, oid: l.oid}, nil)
	l.released = err == nil
	return err
}

// PostgreSQL accepts a SCRAM verifier in CREATE ROLE. Only this verifier,
// never the randomly generated plaintext password, is sent in utility SQL.
func scramVerifier(password string) (string, error) {
	const iterations = 4096
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt[:], iterations, sha256.Size)
	if err != nil {
		return "", err
	}
	sign := func(label string) []byte {
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(label))
		return mac.Sum(nil)
	}
	stored := sha256.Sum256(sign("Client Key"))
	encode := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, encode(salt[:]), encode(stored[:]), encode(sign("Server Key"))), nil
}
