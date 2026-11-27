//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/0xJean/tabula/stdsql"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type sqlTestUUID string

func (u sqlTestUUID) Value() (driver.Value, error) { return string(u), nil }
func (u *sqlTestUUID) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*u = sqlTestUUID(v)
		return nil
	case []byte:
		*u = sqlTestUUID(string(v))
		return nil
	default:
		return fmt.Errorf("cannot scan %T into sqlTestUUID", src)
	}
}

type sqlTestDecimal string

func (d sqlTestDecimal) Value() (driver.Value, error) { return string(d), nil }
func (d *sqlTestDecimal) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*d = sqlTestDecimal(v)
		return nil
	case []byte:
		*d = sqlTestDecimal(string(v))
		return nil
	default:
		return fmt.Errorf("cannot scan %T into sqlTestDecimal", src)
	}
}

// encodedCursorCode fails if passed raw via driver.Valuer, proving ListKeyset
// cursor encoding uses the RegistryConfig.DefaultTypes encoder.
type encodedCursorCode struct {
	raw string
}

func (c encodedCursorCode) Value() (driver.Value, error) {
	return nil, errors.New("encodedCursorCode must be encoded through RegistryConfig.DefaultTypes")
}

func (c *encodedCursorCode) Scan(src any) error {
	switch v := src.(type) {
	case string:
		c.raw = v
		return nil
	case []byte:
		c.raw = string(v)
		return nil
	default:
		return fmt.Errorf("cannot scan %T into encodedCursorCode", src)
	}
}

func TestStructSQLDualDriverAndSQLCheck(t *testing.T) {
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "TABULA_TEST_DSN is required for integration tests")
	ctx := t.Context()

	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	suffix := time.Now().UnixNano()
	schema := fmt.Sprintf("tabula_sql_%d", suffix)
	appRole := fmt.Sprintf("tabula_sql_app_%d", suffix)
	noPrivRole := fmt.Sprintf("tabula_sql_none_%d", suffix)

	exec := func(q string) {
		t.Helper()
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	exec("CREATE ROLE " + appRole + " NOLOGIN NOBYPASSRLS")
	t.Cleanup(func() { _, _ = db.Exec("DROP ROLE IF EXISTS " + appRole) })
	exec("CREATE ROLE " + noPrivRole + " NOLOGIN NOBYPASSRLS")
	t.Cleanup(func() { _, _ = db.Exec("DROP ROLE IF EXISTS " + noPrivRole) })
	exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE") })

	codeDomain := schema + ".item_code"
	itemsTable := schema + ".items"
	archiveTable := schema + ".entries"

	exec("CREATE DOMAIN " + codeDomain + " AS varchar(50) CHECK (length(VALUE) > 0)")
	exec(`CREATE TABLE ` + itemsTable + ` (
		id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		group_id bigint NOT NULL,
		uid uuid NOT NULL,
		code ` + codeDomain + ` NOT NULL,
		name varchar(50) NOT NULL,
		status varchar(20) NOT NULL,
		rank integer NOT NULL,
		amount numeric(12,2) NOT NULL,
		note text,
		tags text[] NOT NULL DEFAULT '{}',
		meta jsonb NOT NULL DEFAULT '{}'::jsonb,
		created_at timestamptz NOT NULL DEFAULT now()
	)`)
	exec(`CREATE TABLE ` + archiveTable + ` (
		id bigint PRIMARY KEY,
		group_id bigint NOT NULL,
		code varchar(50) NOT NULL,
		name varchar(50) NOT NULL
	)`)
	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + appRole + ", " + noPrivRole)
	exec("GRANT SELECT, INSERT, UPDATE, DELETE ON " + itemsTable + " TO " + appRole)
	exec("GRANT SELECT, INSERT, UPDATE, DELETE ON " + archiveTable + " TO " + appRole)
	exec("ALTER TABLE " + itemsTable + " ENABLE ROW LEVEL SECURITY")
	exec("ALTER TABLE " + itemsTable + " FORCE ROW LEVEL SECURITY")
	exec("CREATE POLICY items_all ON " + itemsTable + " FOR ALL TO " + appRole +
		" USING (group_id = nullif(current_setting('demo.scope', true), '')::bigint) WITH CHECK (group_id = nullif(current_setting('demo.scope', true), '')::bigint)")

	type itemRow struct {
		ID        int64          `db:"id"`
		UID       sqlTestUUID    `db:"uid"`
		Code      string         `db:"code"`
		Name      string         `db:"name"`
		Status    string         `db:"status"`
		Rank      int32          `db:"rank"`
		Amount    sqlTestDecimal `db:"amount"`
		Note      string         `db:"note,zero"`
		Tags      []string       `db:"tags,array"`
		Meta      map[string]any `db:"meta,json"`
		CreatedAt time.Time      `db:"created_at"`
	}
	type insertArgs struct {
		UID    sqlTestUUID    `db:"uid"`
		Code   string         `db:"code"`
		Name   string         `db:"name"`
		Status string         `db:"status"`
		Rank   int32          `db:"rank"`
		Amount sqlTestDecimal `db:"amount"`
		Note   *string        `db:"note,secret"`
		Tags   []string       `db:"tags,array"`
		Meta   map[string]any `db:"meta,json"`
	}
	type idArgs struct {
		ID int64 `db:"id"`
	}
	type renameArgs struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
	}
	type archivedRow struct {
		ID   int64  `db:"id"`
		Code string `db:"code"`
		Name string `db:"name"`
	}
	type listArgs struct {
		Status   *string `db:"status"`
		MinRank  *int32  `db:"min_rank"`
		Search   *string `db:"search,secret"`
		HighOnly bool    `db:"high_only"`
	}
	type lockKeyArgs struct {
		Key int64 `db:"lock_key"`
	}
	type nestedItemPart struct {
		ID   int64  `db:"id"`
		Code string `db:"code"`
	}
	type nestedRow struct {
		Item nestedItemPart `db:"i_"`
		Name string         `db:"name"`
	}
	type cursorCodeRow struct {
		ID   int64             `db:"id"`
		Code encodedCursorCode `db:"code"`
	}

	buildRegistry := func(driverName string, logBuf *bytes.Buffer) (*tabula.Registry[int64], tabula.SessionKey) {
		key := tabula.NewSessionKey()
		cfg := tabula.RegistryConfig[int64]{
			Deny:       tabula.DenyLegacySQL,
			SessionKey: key,
			DefaultTypes: []tabula.TypeDefault{
				tabula.TypeMapping(pgtype.UUID[sqlTestUUID]()),
				tabula.TypeMapping(tabula.CatalogType("pg_catalog", "varchar", func(v encodedCursorCode) any {
					return v.raw
				})),
			},
			TypeFamilies: []tabula.TypeFamily{
				tabula.TypeFamilyOf[sqlTestDecimal]("numeric"),
				tabula.TypeFamilyOf[encodedCursorCode]("text", "character varying"),
			},
			Log: tabula.LogConfig{
				Logger: slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
				Level:  tabula.LogAll,
				Values: tabula.ValuesAllowed,
			},
		}
		if driverName == "stdsql" {
			cfg.ArrayArg = pqdriver.Array
			cfg.ArrayColumn = pqdriver.ArrayColumn
		} else {
			cfg.ArrayColumn = pgxdriver.ArrayColumn
		}
		return tabula.NewRegistry(cfg), key
	}

	// 1. Bidirectional sqlcheck verification (positive + negative cases) with BOTH pgxdriver.SQLVerifier and database/sql.
	t.Run("sqlcheck_bidirectional_verification", func(t *testing.T) {
		var logBuf bytes.Buffer
		reg, _ := buildRegistry("pgx", &logBuf)

		qInsertRet := tabula.DefineSQLReturning[itemRow, insertArgs](reg, tabula.SQLSpec{
			Name: "items.insert_returning",
			SQL: `INSERT INTO ` + itemsTable + ` (group_id, uid, code, name, status, rank, amount, note, tags, meta)
VALUES (@group_id, @uid, @code, @name, @status, @rank, @amount, @note, @tags, @meta)
RETURNING id, uid, code, name, status, rank, amount, note, tags, meta, created_at`,
			Scope: tabula.ScopeParam("group_id"),
		})
		qGet := tabula.DefineSQL[itemRow, idArgs](reg, tabula.SQLSpec{
			Name:  "items.get",
			SQL:   `SELECT id, uid, code, name, status, rank, amount, note, tags, meta, created_at FROM ` + itemsTable + ` WHERE group_id = @group_id AND id = @id`,
			Scope: tabula.ScopeParam("group_id"),
		})
		qLock := tabula.DefineSQL[itemRow, idArgs](reg, tabula.SQLSpec{
			Name:  "items.lock",
			SQL:   `SELECT id, uid, code, name, status, rank, amount, note, tags, meta, created_at FROM ` + itemsTable + ` WHERE id = @id FOR UPDATE`,
			Scope: tabula.ScopeByRLS(),
		})
		qRename := tabula.DefineSQLExec[renameArgs](reg, tabula.SQLSpec{
			Name:  "items.rename",
			SQL:   `UPDATE ` + itemsTable + ` SET name = @name WHERE id = @id`,
			Scope: tabula.ScopeByRLS(),
		})
		qMoveRet := tabula.DefineSQLReturning[archivedRow, idArgs](reg, tabula.SQLSpec{
			Name: "items.archive_returning",
			SQL: `WITH moved AS (
	DELETE FROM ` + itemsTable + ` WHERE group_id = @group_id AND id = @id RETURNING id, group_id, code, name
), ins AS (
	INSERT INTO ` + archiveTable + ` (id, group_id, code, name) SELECT id, group_id, code, name FROM moved RETURNING id, code, name
)
SELECT id, code, name FROM ins`,
			Scope: tabula.ScopeParam("group_id"),
		})
		qAdvLock := tabula.DefineSQLExec[lockKeyArgs](reg, tabula.SQLSpec{
			Name:         "items.xact_lock",
			SQL:          `SELECT pg_advisory_xact_lock(@lock_key)`,
			Scope:        tabula.ScopeGlobal(),
			AdvisoryLock: true,
		})
		qCleanupNoArgs := tabula.DefineSQLExec[tabula.NoArgs](reg, tabula.SQLSpec{
			Name:  "items.cleanup_negative",
			SQL:   `DELETE FROM ` + itemsTable + ` WHERE rank < 0`,
			Scope: tabula.ScopeByRLS(),
		})
		qList := tabula.DefineSQLList[itemRow, listArgs](reg, tabula.SQLListSpec{
			Name:      "items.list",
			Select:    `SELECT id, uid, code, name, status, rank, amount, note, tags, meta, created_at FROM ` + itemsTable,
			Where:     `group_id = @group_id`,
			CountBase: `SELECT count(*) FROM ` + itemsTable,
			Scope:     tabula.ScopeParam("group_id"),
			Filters: []tabula.SQLFilter{
				{Name: "status", SQL: `status = @status`},
				{Name: "min_rank", SQL: `rank >= @min_rank`},
				{Name: "search", SQL: `(code ILIKE @search OR name ILIKE @search)`},
				{Name: "high", SQL: `rank >= 50`, When: "high_only"},
			},
			Order: tabula.QueryOrderSpec{
				Default:          "id",
				DefaultDirection: tabula.SortAsc,
				Sorts: []tabula.QuerySort{
					{Key: "id", Expr: "id", Column: "id"},
					{Key: "rank", Expr: "rank", Column: "rank"},
				},
				TieBreaker: []tabula.QuerySortTerm{{Expr: "id", Column: "id"}},
			},
			MaxLimit: 100,
			Keyset:   true,
		})
		qCursorCodeList := tabula.DefineSQLList[cursorCodeRow, tabula.NoArgs](reg, tabula.SQLListSpec{
			Name:   "items.cursor_code_list",
			Select: `SELECT id, code FROM ` + itemsTable,
			Where:  `group_id = @group_id`,
			Scope:  tabula.ScopeParam("group_id"),
			Order: tabula.QueryOrderSpec{
				Default:          "code",
				DefaultDirection: tabula.SortAsc,
				Sorts:            []tabula.QuerySort{{Key: "code", Expr: "code", Column: "code"}},
				TieBreaker:       []tabula.QuerySortTerm{{Expr: "id", Column: "id"}},
			},
			MaxLimit: 50,
			Keyset:   true,
		})
		_ = qInsertRet
		_ = qGet
		_ = qLock
		_ = qRename
		_ = qMoveRet
		_ = qAdvLock
		_ = qCleanupNoArgs
		_ = qList
		_ = qCursorCodeList

		// Full registry passes pgxdriver.SQLVerifier with zero execution!
		allDefs := sqlcheck.ViewsToDefinitions(reg.DefinedViews())
		failures, err := sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), allDefs, appRole)
		require.NoError(t, err)
		require.Empty(t, failures)

		// Snapshot round-trip preserves StructSQL and passes pgxdriver.SQLVerifier!
		snap := sqlcheck.RenderAllSnapshot(reg.Defined(), reg.DefinedViews())
		parsed, err := sqlcheck.ParseSnapshot(snap)
		require.NoError(t, err)
		failures, err = sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), parsed, appRole)
		require.NoError(t, err)
		require.Empty(t, failures)

		// Regression: zero-arg, zero-result DefineSQLExec[NoArgs] preserves StructSQL across
		// snapshot round-trip and passes CheckPolicyCoverage via inferred table access (no false
		// "typed statement requires reviewed Access or NoAccess" error).
		var parsedCleanup tabula.Definition
		for _, d := range parsed {
			if d.Name == "items.cleanup_negative" {
				parsedCleanup = d
				break
			}
		}
		require.Equal(t, "items.cleanup_negative", parsedCleanup.Name)
		require.True(t, parsedCleanup.StatementShapes["sql"].StructSQL)
		cleanupShape := parsedCleanup.StatementShapes["sql"]
		cleanupStmt := sqlcheck.Statement{
			Name:  parsedCleanup.Name + "/sql",
			SQL:   parsedCleanup.Statements["sql"],
			Shape: &cleanupShape,
		}
		covFindings, err := sqlcheck.CheckPolicyCoverage(ctx, db, sqlcheck.RoleStatements{
			appRole: {cleanupStmt},
		})
		require.NoError(t, err)
		require.Empty(t, covFindings)

		// Negative coverage check: role without DELETE policy on itemsTable is flagged.
		noCovFindings, err := sqlcheck.CheckPolicyCoverage(ctx, db, sqlcheck.RoleStatements{
			noPrivRole: {cleanupStmt},
		})
		require.NoError(t, err)
		require.NotEmpty(t, noCovFindings)
		require.NotContains(t, noCovFindings[0], "typed statement requires reviewed Access or NoAccess")

		// Read-only statements + Exec + List + FOR UPDATE also pass standard database/sql verifier.
		readOnlyDefs := []tabula.Definition{
			qGet.Definition(),
			qLock.Definition(),
			qRename.Definition(),
			qAdvLock.Definition(),
			qList.Definition(),
		}
		failures, err = sqlcheck.VerifyStatements(ctx, db, readOnlyDefs, appRole)
		require.NoError(t, err)
		require.Empty(t, failures)

		// Returning writes require Describe on database/sql (fail closed with ErrDescribeRequired).
		_, err = sqlcheck.VerifyStatements(ctx, db, []tabula.Definition{qInsertRet.Definition()}, appRole)
		require.ErrorIs(t, err, sqlcheck.ErrDescribeRequired)
		_, err = sqlcheck.VerifyStatements(ctx, db, []tabula.Definition{qMoveRet.Definition()}, appRole)
		require.ErrorIs(t, err, sqlcheck.ErrDescribeRequired)

		// Confirm zero rows were inserted during verification.
		var count int64
		require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+itemsTable).Scan(&count))
		require.Zero(t, count)

		// Negative case 1: missing role table privilege -> RED.
		failures, err = sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), []tabula.Definition{qGet.Definition()}, noPrivRole)
		require.NoError(t, err)
		require.NotEmpty(t, failures)

		// Negative case 2: misspelled result column name -> RED (both pgx verifier and database/sql).
		type typoRow struct {
			ID  int64  `db:"id"`
			Cod string `db:"cod"`
		}
		badTypo := tabula.DefineSQL[typoRow, tabula.NoArgs](reg, tabula.SQLSpec{
			Name: "neg.typo_col", SQL: "SELECT id, code FROM " + itemsTable, Scope: tabula.ScopeByRLS(),
		})
		for _, checkFn := range []func([]tabula.Definition) ([]string, error){
			func(d []tabula.Definition) ([]string, error) {
				return sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), d, appRole)
			},
			func(d []tabula.Definition) ([]string, error) {
				return sqlcheck.VerifyStatements(ctx, db, d, appRole)
			},
		} {
			f, err := checkFn([]tabula.Definition{badTypo.Definition()})
			require.NoError(t, err)
			require.Len(t, f, 1)
			require.Contains(t, f[0], `expected "cod"`)
		}

		// Negative case 3: swapped SELECT vs Row field order -> RED (both verifiers).
		type swappedRow struct {
			ID   int64  `db:"id"`
			Name string `db:"name"`
		}
		badOrder := tabula.DefineSQL[swappedRow, tabula.NoArgs](reg, tabula.SQLSpec{
			Name: "neg.swapped_order", SQL: "SELECT name, id FROM " + itemsTable, Scope: tabula.ScopeByRLS(),
		})
		for _, checkFn := range []func([]tabula.Definition) ([]string, error){
			func(d []tabula.Definition) ([]string, error) {
				return sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), d, appRole)
			},
			func(d []tabula.Definition) ([]string, error) {
				return sqlcheck.VerifyStatements(ctx, db, d, appRole)
			},
		} {
			f, err := checkFn([]tabula.Definition{badOrder.Definition()})
			require.NoError(t, err)
			require.Len(t, f, 1)
			require.Contains(t, f[0], `is "name", expected "id"`)
		}

		// Negative case 4: incompatible result column type (text/varchar column into int64, bigint into int32, uuid into string) -> RED.
		type wrongColTypeRow struct {
			ID   int32  `db:"id"` // bigint column cannot fit in int32
			Name int64  `db:"name"`
			UID  string `db:"uid"`
		}
		badColType := tabula.DefineSQL[wrongColTypeRow, tabula.NoArgs](reg, tabula.SQLSpec{
			Name: "neg.col_type", SQL: "SELECT id, name, uid FROM " + itemsTable, Scope: tabula.ScopeByRLS(),
		})
		f, err := sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), []tabula.Definition{badColType.Definition()}, appRole)
		require.NoError(t, err)
		require.Len(t, f, 1)
		require.Contains(t, f[0], "not compatible")

		f, err = sqlcheck.VerifyStatements(ctx, db, []tabula.Definition{badColType.Definition()}, appRole)
		require.NoError(t, err)
		require.Len(t, f, 1)
		require.Contains(t, f[0], "not compatible")

		// Negative case 5: incompatible parameter type (string arg for bigint column) -> RED.
		type wrongParamArgs struct {
			ID string `db:"id"`
		}
		badParam := tabula.DefineSQL[swappedRow, wrongParamArgs](reg, tabula.SQLSpec{
			Name: "neg.param_type", SQL: "SELECT id, name FROM " + itemsTable + " WHERE id = @id", Scope: tabula.ScopeByRLS(),
		})
		f, err = sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), []tabula.Definition{badParam.Definition()}, appRole)
		require.NoError(t, err)
		require.Len(t, f, 1)
		require.Contains(t, f[0], "not compatible")

		f, err = sqlcheck.VerifyStatements(ctx, db, []tabula.Definition{badParam.Definition()}, appRole)
		require.NoError(t, err)
		require.Len(t, f, 1)
		require.Contains(t, f[0], "not compatible")
	})

	// 2. Full runtime execution on BOTH stdsql (lib/pq) AND pgxdriver (pgx/v5) under DenyLegacySQL + RequireSealed + StatementPolicy!
	for _, driverName := range []string{"stdsql", "pgx"} {
		t.Run("runtime_"+driverName, func(t *testing.T) {
			exec("TRUNCATE " + itemsTable + ", " + archiveTable + " RESTART IDENTITY")

			var logBuf bytes.Buffer
			reg, key := buildRegistry(driverName, &logBuf)

			qInsert := tabula.DefineSQLReturning[itemRow, insertArgs](reg, tabula.SQLSpec{
				Name: "rt.insert",
				SQL: `INSERT INTO ` + itemsTable + ` (group_id, uid, code, name, status, rank, amount, note, tags, meta)
VALUES (@group_id, @uid, @code, @name, @status, @rank, @amount, @note, @tags, @meta)
RETURNING id, uid, code, name, status, rank, amount, note, tags, meta, created_at`,
				Scope: tabula.ScopeParam("group_id"),
			})
			qGet := tabula.DefineSQL[itemRow, idArgs](reg, tabula.SQLSpec{
				Name:  "rt.get",
				SQL:   `SELECT id, uid, code, name, status, rank, amount, note, tags, meta, created_at FROM ` + itemsTable + ` WHERE group_id = @group_id AND id = @id`,
				Scope: tabula.ScopeParam("group_id"),
			})
			qNested := tabula.DefineSQL[nestedRow, idArgs](reg, tabula.SQLSpec{
				Name:  "rt.nested",
				SQL:   `SELECT id AS i_id, code AS i_code, name FROM ` + itemsTable + ` WHERE id = @id`,
				Scope: tabula.ScopeByRLS(),
			})
			qLock := tabula.DefineSQL[itemRow, idArgs](reg, tabula.SQLSpec{
				Name:  "rt.lock",
				SQL:   `SELECT id, uid, code, name, status, rank, amount, note, tags, meta, created_at FROM ` + itemsTable + ` WHERE id = @id FOR UPDATE`,
				Scope: tabula.ScopeByRLS(),
			})
			qRename := tabula.DefineSQLExec[renameArgs](reg, tabula.SQLSpec{
				Name:  "rt.rename",
				SQL:   `UPDATE ` + itemsTable + ` SET name = @name WHERE id = @id`,
				Scope: tabula.ScopeByRLS(),
			})
			qMove := tabula.DefineSQLReturning[archivedRow, idArgs](reg, tabula.SQLSpec{
				Name: "rt.move",
				SQL: `WITH moved AS (
	DELETE FROM ` + itemsTable + ` WHERE group_id = @group_id AND id = @id RETURNING id, group_id, code, name
), ins AS (
	INSERT INTO ` + archiveTable + ` (id, group_id, code, name) SELECT id, group_id, code, name FROM moved RETURNING id, code, name
)
SELECT id, code, name FROM ins`,
				Scope: tabula.ScopeParam("group_id"),
			})
			qAdvLock := tabula.DefineSQLExec[lockKeyArgs](reg, tabula.SQLSpec{
				Name:         "rt.adv_lock",
				SQL:          `SELECT pg_advisory_xact_lock(@lock_key)`,
				Scope:        tabula.ScopeGlobal(),
				AdvisoryLock: true,
			})
			qList := tabula.DefineSQLList[itemRow, listArgs](reg, tabula.SQLListSpec{
				Name:      "rt.list",
				Select:    `SELECT id, uid, code, name, status, rank, amount, note, tags, meta, created_at FROM ` + itemsTable,
				Where:     `group_id = @group_id`,
				CountBase: `SELECT count(*) FROM ` + itemsTable,
				Scope:     tabula.ScopeParam("group_id"),
				Filters: []tabula.SQLFilter{
					{Name: "status", SQL: `status = @status`},
					{Name: "min_rank", SQL: `rank >= @min_rank`},
					{Name: "search", SQL: `(code ILIKE @search OR name ILIKE @search)`},
					{Name: "high", SQL: `rank >= 50`, When: "high_only"},
				},
				Order: tabula.QueryOrderSpec{
					Default:          "id",
					DefaultDirection: tabula.SortAsc,
					Sorts: []tabula.QuerySort{
						{Key: "id", Expr: "id", Column: "id"},
						{Key: "rank", Expr: "rank", Column: "rank"},
					},
					TieBreaker: []tabula.QuerySortTerm{{Expr: "id", Column: "id"}},
				},
				MaxLimit: 100,
				Keyset:   true,
			})
			qCursorCodeList := tabula.DefineSQLList[cursorCodeRow, tabula.NoArgs](reg, tabula.SQLListSpec{
				Name:   "rt.cursor_code_list",
				Select: `SELECT id, code FROM ` + itemsTable,
				Where:  `group_id = @group_id`,
				Scope:  tabula.ScopeParam("group_id"),
				Order: tabula.QueryOrderSpec{
					Default:          "code",
					DefaultDirection: tabula.SortAsc,
					Sorts:            []tabula.QuerySort{{Key: "code", Expr: "code", Column: "code"}},
					TieBreaker:       []tabula.QuerySortTerm{{Expr: "id", Column: "id"}},
				},
				MaxLimit: 50,
				Keyset:   true,
			})

			stmtPolicy := tabula.StatementPolicy{
				Read:                    []string{"select", "values", "with"},
				Write:                   []string{"insert", "update", "delete", "merge", "with"},
				RequireReturning:        true,
				AllowAuthorizedWriteCTE: true,
			}
			scopeVars := func(s int64) map[string]string {
				return map[string]string{"demo.scope": strconv.FormatInt(s, 10)}
			}
			runTx := func(scope int64, readOnly bool, fn func(context.Context, tabula.Session[int64]) error) error {
				if driverName == "stdsql" {
					r, err := stdsql.NewRunner(db, stdsql.ScopePolicy[int64]{
						ManagedVars: []string{"demo.scope"},
						ScopeVars:   scopeVars,
						GuardQuery:  tabula.GuardNoSessionEscape,
					}, stdsql.RunnerOptions{
						Role:         appRole,
						SessionKey:   key,
						SQLAuthority: reg.SQLAuthority(),
						Statements:   stmtPolicy,
					})
					require.NoError(t, err)
					if readOnly {
						return r.WithinReadOnly(ctx, scope, func(c context.Context, s *stdsql.Session[int64]) error { return fn(c, s) })
					}
					return r.Within(ctx, scope, func(c context.Context, s *stdsql.Session[int64]) error { return fn(c, s) })
				}
				r, err := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[int64]{
					ManagedVars: []string{"demo.scope"},
					ScopeVars:   scopeVars,
					GuardQuery:  tabula.GuardNoSessionEscape,
				}, pgxdriver.RunnerOptions{
					Role:         appRole,
					SessionKey:   key,
					SQLAuthority: reg.SQLAuthority(),
					Statements:   stmtPolicy,
				})
				require.NoError(t, err)
				if readOnly {
					return r.WithinReadOnly(ctx, scope, func(c context.Context, s *pgxdriver.Session[int64]) error { return fn(c, s) })
				}
				return r.Within(ctx, scope, func(c context.Context, s *pgxdriver.Session[int64]) error { return fn(c, s) })
			}

			var firstID, secondID int64
			secretNote := "classified-note-value"
			require.NoError(t, runTx(10, false, func(c context.Context, s tabula.Session[int64]) error {
				// Advisory lock succeeds inside RW transaction.
				_, err := qAdvLock.Bind(s).Exec(c, lockKeyArgs{Key: 991001})
				require.NoError(t, err)

				// Insert two rows via DefineSQLReturning.
				r1, err := qInsert.Bind(s).One(c, insertArgs{
					UID:    "11111111-1111-1111-1111-111111111111",
					Code:   "A-1",
					Name:   "Alpha",
					Status: "open",
					Rank:   20,
					Amount: "12.50",
					Note:   &secretNote,
					Tags:   []string{"blue", "fast"},
					Meta:   map[string]any{"k": "v"},
				})
				require.NoError(t, err)
				require.Equal(t, sqlTestUUID("11111111-1111-1111-1111-111111111111"), r1.UID)
				require.Equal(t, "Alpha", r1.Name)
				require.Equal(t, sqlTestDecimal("12.50"), r1.Amount)
				require.Equal(t, secretNote, r1.Note)
				require.Equal(t, []string{"blue", "fast"}, r1.Tags)
				require.Equal(t, "v", r1.Meta["k"])
				firstID = r1.ID

				r2, err := qInsert.Bind(s).One(c, insertArgs{
					UID:    "22222222-2222-2222-2222-222222222222",
					Code:   "B-2",
					Name:   "Beta",
					Status: "closed",
					Rank:   80,
					Amount: "99.95",
					Note:   nil, // NULL scans as "" via ,zero
					Tags:   []string{"green"},
					Meta:   map[string]any{"n": json.Number("2")},
				})
				require.NoError(t, err)
				require.Equal(t, "", r2.Note)
				secondID = r2.ID

				// Rename via DefineSQLExec.
				n, err := qRename.Bind(s).Exec(c, renameArgs{ID: firstID, Name: "Alpha-Updated"})
				require.NoError(t, err)
				require.Equal(t, int64(1), n)

				// Nested struct row scan + row lock inside RW tx.
				nr, err := qNested.Bind(s).One(c, idArgs{ID: firstID})
				require.NoError(t, err)
				require.Equal(t, nestedRow{Item: nestedItemPart{ID: firstID, Code: "A-1"}, Name: "Alpha-Updated"}, nr)

				locked, err := qLock.Bind(s).One(c, idArgs{ID: firstID})
				require.NoError(t, err)
				require.Equal(t, "Alpha-Updated", locked.Name)
				return nil
			}))

			// Secret arg was redacted in structured logs.
			require.NotContains(t, logBuf.String(), secretNote)
			require.Contains(t, logBuf.String(), "[redacted]")

			// Read-only transaction allows DefineSQL & DefineSQLList, rejects FOR UPDATE and writes with ErrReadOnly.
			require.NoError(t, runTx(10, true, func(c context.Context, s tabula.Session[int64]) error {
				got, err := qGet.Bind(s).One(c, idArgs{ID: firstID})
				require.NoError(t, err)
				require.Equal(t, "Alpha-Updated", got.Name)

				_, ok, err := qGet.Bind(s).MaybeOne(c, idArgs{ID: 999999})
				require.NoError(t, err)
				require.False(t, ok)

				_, err = qLock.Bind(s).One(c, idArgs{ID: firstID})
				require.ErrorIs(t, err, tabula.ErrReadOnly)

				_, err = qRename.Bind(s).Exec(c, renameArgs{ID: firstID, Name: "nope"})
				require.ErrorIs(t, err, tabula.ErrReadOnly)

				// SQLList: baseline, filtered subset, Count, All, Each, ListKeyset.
				all, total, err := qList.Bind(s).List(c, listArgs{}, tabula.QueryPage{Sort: "rank", Direction: tabula.SortDesc, Size: 10})
				require.NoError(t, err)
				require.Equal(t, int64(2), total)
				require.Len(t, all, 2)
				require.Equal(t, secondID, all[0].ID)

				openStatus := "open"
				minRank := int32(10)
				filtered, fTotal, err := qList.Bind(s).List(c, listArgs{Status: &openStatus, MinRank: &minRank}, tabula.QueryPage{Size: 10})
				require.NoError(t, err)
				require.Equal(t, int64(1), fTotal)
				require.Len(t, filtered, 1)
				require.Equal(t, firstID, filtered[0].ID)

				highOnly, err := qList.Bind(s).All(c, listArgs{HighOnly: true}, tabula.QueryOrder{Sort: "id", Direction: tabula.SortAsc})
				require.NoError(t, err)
				require.Len(t, highOnly, 1)
				require.Equal(t, secondID, highOnly[0].ID)

				cnt, err := qList.Bind(s).Count(c, listArgs{HighOnly: true})
				require.NoError(t, err)
				require.Equal(t, int64(1), cnt)

				var streamed []int64
				stopErr := errors.New("stop after first")
				err = qList.Bind(s).Each(c, listArgs{}, tabula.QueryOrder{Sort: "id", Direction: tabula.SortAsc}, func(r itemRow) error {
					streamed = append(streamed, r.ID)
					return stopErr
				})
				require.Same(t, stopErr, err)
				require.Equal(t, []int64{firstID}, streamed)

				// Keyset page 1 -> page 2.
				p1, err := qList.Bind(s).ListKeyset(c, listArgs{}, tabula.QueryKeyset{Sort: "id", Direction: tabula.SortAsc, Limit: 1})
				require.NoError(t, err)
				require.True(t, p1.HasMore)
				require.Len(t, p1.Items, 1)
				require.Equal(t, firstID, p1.Items[0].ID)

				p2, err := qList.Bind(s).ListKeyset(c, listArgs{}, tabula.QueryKeyset{Sort: "id", Direction: tabula.SortAsc, Limit: 1, After: p1.Next})
				require.NoError(t, err)
				require.False(t, p2.HasMore)
				require.Len(t, p2.Items, 1)
				require.Equal(t, secondID, p2.Items[0].ID)

				// Regression: ListKeyset cursor After column encoded via RegistryConfig.DefaultTypes.
				cp1, err := qCursorCodeList.Bind(s).ListKeyset(c, tabula.NoArgs{}, tabula.QueryKeyset{Sort: "code", Direction: tabula.SortAsc, Limit: 1})
				require.NoError(t, err)
				require.True(t, cp1.HasMore)
				require.Len(t, cp1.Items, 1)
				require.Equal(t, encodedCursorCode{raw: "A-1"}, cp1.Items[0].Code)

				cp2, err := qCursorCodeList.Bind(s).ListKeyset(c, tabula.NoArgs{}, tabula.QueryKeyset{Sort: "code", Direction: tabula.SortAsc, Limit: 1, After: cp1.Next})
				require.NoError(t, err)
				require.False(t, cp2.HasMore)
				require.Len(t, cp2.Items, 1)
				require.Equal(t, encodedCursorCode{raw: "B-2"}, cp2.Items[0].Code)
				return nil
			}))

			// Scope isolation: tenant 20 sees 0 rows.
			require.NoError(t, runTx(20, true, func(c context.Context, s tabula.Session[int64]) error {
				_, ok, err := qGet.Bind(s).MaybeOne(c, idArgs{ID: firstID})
				require.NoError(t, err)
				require.False(t, ok)
				return nil
			}))

			// Modifying CTE via DefineSQLReturning moves row from items -> entries.
			require.NoError(t, runTx(10, false, func(c context.Context, s tabula.Session[int64]) error {
				moved, err := qMove.Bind(s).One(c, idArgs{ID: firstID})
				require.NoError(t, err)
				require.Equal(t, archivedRow{ID: firstID, Code: "A-1", Name: "Alpha-Updated"}, moved)
				return nil
			}))

			// Direct pool rejects AdvisoryLock and FOR UPDATE with ErrReadOnly.
			if driverName == "stdsql" {
				direct, err := stdsql.Direct("postgres", dsn, stdsql.ScopePolicy[int64]{}, stdsql.DirectSessionKey(key), stdsql.DirectSQLAuthority(reg.SQLAuthority()))
				require.NoError(t, err)
				defer func() { _ = direct.Close() }()
				_, err = qAdvLock.Bind(direct.Session(10)).Exec(ctx, lockKeyArgs{Key: 1})
				require.ErrorIs(t, err, tabula.ErrReadOnly)
				_, err = qLock.Bind(direct.Session(10)).One(ctx, idArgs{ID: secondID})
				require.ErrorIs(t, err, tabula.ErrReadOnly)
			} else {
				direct, err := pgxdriver.DirectWithConfig(ctx, dsn, pgxdriver.ScopePolicy[int64]{}, pgxdriver.DirectConfig{
					SessionKey:   key,
					SQLAuthority: reg.SQLAuthority(),
				})
				require.NoError(t, err)
				defer func() { _ = direct.Close() }()
				_, err = qAdvLock.Bind(direct.Session(10)).Exec(ctx, lockKeyArgs{Key: 1})
				require.ErrorIs(t, err, tabula.ErrReadOnly)
				_, err = qLock.Bind(direct.Session(10)).One(ctx, idArgs{ID: secondID})
				require.ErrorIs(t, err, tabula.ErrReadOnly)
			}
		})
	}
}
