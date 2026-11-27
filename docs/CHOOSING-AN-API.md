# Choosing a Tabula API

Tabula offers three active SQL entry points on top of one shared `*tabula.Registry[S]`, `SQLAuthority`, `SessionGuard`, observability pipeline and `sqlcheck` verifier. Pick the lightest tier that satisfies your query's needs.

## Decision matrix by task

| Task | Recommended API | Package | Why |
| --- | --- | --- | --- |
| Primary-key lookup, lock, insert, optimistic update, soft delete, controlled upsert, `CopyFrom` | `tabula.Define` + `Table.Bind(s)` (`Get`, `Lock`, `Insert`, `Save`, `Set`, `Delete`, `Upsert`) | `tabula` | Zero hand-written SQL for single-table lifecycle operations; enforces `once`, `version`, `scope`, `deleted` and `WriteMode` automatically. |
| Joins, CTEs, aggregates, window queries, `RETURNING` writes, `MERGE`, `COLLATE`, advisory locks | `tabula.DefineSQL`, `DefineSQLExec`, `DefineSQLReturning` | `tabula` | Write ordinary PostgreSQL with `@name` parameters and `[Row, Args]` structs; verified against PostgreSQL via `sqlcheck` without executing writes, compatible with `DenyLegacySQL`. |
| Filtered, sorted, offset or keyset-paginated list endpoints | `tabula.DefineSQLList` | `tabula` | Registers finite filter/sort/pagination variants from named `@name` SQL fragments with automatic `Count` and `KeysetPage.Next` support under `SQLAuthority`. |
| Programmatic query/write composition, finite filter/assignment families, catalog-proven keyset cursors, or AST-derived column-level privileges | `plan.MapTable`, `plan.DefineTableRows`, `plan.DefineRead`, `plan.DefineWrite`, `plan.DefineReturning`, `plan.DefineRoutine`, `plan.DefineAdvisoryLock` | `tabula/plan` | Derives exact column-level `SELECT`/`INSERT`/`UPDATE`/`DELETE` grants, `pg_attribute.atttypmod` checks, conflict-arbiter proofs and keyset uniqueness premises directly from the Go AST. |
| Table-level vs column-level grant auditing | `sqlcheck.GranularityTable` (table-level) or `sqlcheck.GranularityAll` (column-level, default) | `tabula/sqlcheck` | Pair `DefineSQL*` with `GranularityTable` for fast adoption, or pair `tabula/plan` with `GranularityAll` when proving minimum column ACLs per role. |

### Legacy `v0.3` string-fragment APIs

The earlier string-fragment helpers (`Rows.Find`/`List`/`ListKeyset`/`Select`/`Exec`, `Filter`, `View.Statement`, `View.DefineQuery`, and `UpsertSpec.Where`) remain in `tabula` for backward compatibility on registries that do not enable `DenyLegacySQL`. For new code—especially on strict registries (`Deny: tabula.DenyLegacySQL`)—prefer `DefineSQL*` or `tabula/plan` instead.

## How `tabula` and `tabula/plan` fit together

Both tiers register their compiled SQL and `StatementShape` metadata on the same `*tabula.Registry[S]`:

- Declare your `Registry`, `Table` (or `DefinePlanSource`), and `View` once in `tabula`.
- Use `tabula.Table` for primary-key operations, `tabula.DefineSQL*` for everyday explicit SQL, and import `github.com/0xJean/tabula/plan` only in packages that need AST composition or column-level privilege derivation.
- Pass `reg.SQLAuthority()` to `stdsql.NewRunner` or `pgxdriver.NewRunner` once; every `Table` fixed operation, `DefineSQL*` declaration, and `plan.Define*` declaration executes through the same `AuthorizedSession` channel.

## Minimal application binding

Most services wrap Tabula in one small internal `db` package that owns the scope type, registry, session key, runner and startup readiness check:

```go
package db

import (
	"context"
	"fmt"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ScopeID int64

var (
	sessionKey = tabula.NewSessionKey()
	Registry   = tabula.NewRegistry(tabula.RegistryConfig[ScopeID]{
		SessionKey:   sessionKey,
		Deny:         tabula.DenyLegacySQL | tabula.DenyDelete,
		TypeFamilies: []tabula.TypeFamily{tabula.TypeFamilyOf[ScopeID]("bigint")},
	})
)

type Item struct {
	ID      int64   `db:"id,key,serial"`
	ScopeID ScopeID `db:"scope_id,scope"`
	Code    string  `db:"code"`
	Name    string  `db:"name"`
	Version int     `db:"version,version"`
}

var Items = tabula.Define(Registry, tabula.TableSpec[Item, int64]{
	Name: "public.items",
})

type CodeArgs struct {
	Code string `db:"code"`
}

var FindItemByCode = tabula.DefineSQL[Item, CodeArgs](Registry, tabula.SQLSpec{
	Name:  "items.by_code",
	SQL:   `SELECT id, scope_id, code, name, version FROM public.items WHERE scope_id = @scope_id AND code = @code`,
	Scope: tabula.ScopeParam("scope_id"),
})

func NewRunner(pool *pgxpool.Pool, role string) (*pgxdriver.Runner[ScopeID], error) {
	policy := pgxdriver.ScopePolicy[ScopeID]{
		ManagedVars: []string{"demo.scope"},
		ScopeVars:   func(id ScopeID) map[string]string { return map[string]string{"demo.scope": fmt.Sprintf("%d", id)} },
	}
	return pgxdriver.NewRunner(pool, policy, pgxdriver.RunnerOptions{
		Role:         role,
		SessionKey:   sessionKey,
		SQLAuthority: Registry.SQLAuthority(),
	})
}

func VerifyAtStartup(ctx context.Context, pool *pgxpool.Pool, role string) error {
	// Verify all registered statements against PostgreSQL without executing writes.
	return sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), role, Registry.Defined(), Registry.DefinedViews())
}
```

Add `github.com/0xJean/tabula/plan` later only if a specific repository needs AST-derived column grants, finite query/mutation families, or catalog-verified keyset continuation proofs.
