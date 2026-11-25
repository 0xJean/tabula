# Process login roles for disposable PostgreSQL fixtures

A suite can acquire one login for its process and use it across database clones. Each acquisition creates a distinct role, including when two workers use the same installation, maintenance account or admin DSN. The application chooses the member roles and grants CONNECT to each clone. Template publication and template cache retention keep their existing lifetimes.

## Public interface

```go
type LoginRoleOptions struct {
    MemberOf        []string
    Inherit         bool
    ConnectionLimit *int
}

func (Fixture) AcquireLoginRole(context.Context, LoginRoleOptions) (*LoginRole, error)
func (*LoginRole) Name() string
func (*LoginRole) Password() string
func (*LoginRole) Release(context.Context) error
func (Fixture) PruneLoginRoles(context.Context) ([]string, error)
```

The zero options create a LOGIN NOINHERIT role with no memberships and PostgreSQL's unlimited connection count. Callers can enable inheritance or set a connection limit. The role has NOSUPERUSER, NOCREATEDB, NOCREATEROLE, NOREPLICATION and NOBYPASSRLS. Memberships refer to existing catalog roles. The database's PUBLIC privileges and the properties of those roles are part of the host installation. Use the generated login name when granting CONNECT on a new test database. Keep the raw password in the test process. Role creation sends PostgreSQL a salted SCRAM-SHA-256 verifier. [PostgreSQL role options](https://www.postgresql.org/docs/17/sql-createrole.html)

## Ownership and liveness

Acquire pins a maintenance connection before publishing the role. It takes a session advisory lock on the generated name and creates the role, memberships and versioned ownership comment in one transaction. The comment identifies the maintenance database OID and the created role OID. All workers use the same maintenance database. A pruner connected to a different database skips these roles because advisory locks are database-local. Use a direct or session-pooled admin connection, and budget one checked-out connection per live handle in addition to normal fixture work.

The pinned server session is the lease. It remains active between tests and after the last clone or business connection closes. A killed test process closes its connection and PostgreSQL releases its lock. Losing the administrative session, including through a server restart or network failure, also ends the lease. The suite must retire that handle and acquire a fresh login before resuming. The database cannot distinguish a remote process crash from permanent loss of its connection. [PostgreSQL advisory locks](https://www.postgresql.org/docs/17/explicit-locking.html#ADVISORY-LOCKS)

## Release and pruning

Release is explicit. Close the business pools at suite shutdown and release the role, even when all test databases have already been dropped. On a failed release, return the error and leave the role identifiable for retry. The library disposes of the pinned backend so an idle pooled connection cannot retain a lease.

Prune discovers roles directly in the shared catalog, independently of its template scan and maxIdle. Candidates must match both the generated name format and the ownership comment for their OIDs. Before removal it acquires the same key with a nonblocking transaction advisory lock and rechecks identity under that lock. Busy roles are kept. Cleanup revokes outstanding direct database CONNECT grants and drops the role in one transaction. PostgreSQL removes its memberships. Unexpected dependencies such as an application object owned by the login abort the cleanup and preserve its grants. This avoids dropping unrelated objects. Prune preserves its existing result of dropped database names. PruneLoginRoles can run independently of database cleanup and reports the names of the logins it removed. [PostgreSQL role removal](https://www.postgresql.org/docs/17/sql-droprole.html)

The integration suite uses separate OS processes on a disposable cluster. Workers authenticate with independent credentials, close their business connections, and wait on a pipe while keeping their administrative leases. Tests exercise normal release, SIGKILL and prune, concurrent pruning, foreign maintenance databases, ownership marks, leftover CONNECT grants and unexpected dependencies.
