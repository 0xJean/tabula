//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/0xJean/tabula/stdsql"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestStructSQLIntegerInputWideningAndResultBounds(t *testing.T) {
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
	schema := fmt.Sprintf("tabula_intw_%d", suffix)
	appRole := fmt.Sprintf("tabula_intw_app_%d", suffix)
	notesTable := schema + ".notes"

	exec := func(q string) {
		t.Helper()
		_, err := db.ExecContext(ctx, q)
		require.NoError(t, err, q)
	}
	exec("CREATE ROLE " + appRole + " NOLOGIN NOBYPASSRLS")
	t.Cleanup(func() { _, _ = db.Exec("DROP ROLE IF EXISTS " + appRole) })
	exec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { _, _ = db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE") })
	exec(`CREATE TABLE ` + notesTable + ` (
		id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
		group_id bigint NOT NULL,
		state smallint,
		state_nn smallint NOT NULL,
		rank integer NOT NULL
	)`)
	exec("GRANT USAGE ON SCHEMA " + schema + " TO " + appRole)
	exec("GRANT SELECT, INSERT, UPDATE, DELETE ON " + notesTable + " TO " + appRole)
	exec("INSERT INTO " + notesTable + " (group_id, state, state_nn, rank) VALUES (7, 2, 2, 10), (7, NULL, 0, 20)")

	type noteRow struct {
		ID      int64 `db:"id"`
		StateNN int16 `db:"state_nn"`
		Rank    int32 `db:"rank"`
	}
	type int16CaseArgs struct {
		Status  int16  `db:"status"`
		RankPtr *int16 `db:"rank_ptr"`
		SmallID int16  `db:"small_id"`
	}
	type int32BigintArgs struct {
		ID int32 `db:"id"`
	}
	type int16ArrayArgs struct {
		Ranks []int16 `db:"ranks,array"`
	}

	reg := tabula.NewRegistry(tabula.RegistryConfig[int16]{
		Deny:        tabula.DenyLegacySQL,
		ArrayArg:    pqdriver.Array,
		ArrayColumn: pqdriver.ArrayColumn,
	})

	// Positive 1: int16 and *int16 inputs against PG integer (CASE expression & rank) and bigint (id & ScopeParam).
	qInt16Widened := tabula.DefineSQL[noteRow, int16CaseArgs](reg, tabula.SQLSpec{
		Name: "notes.int16_widened",
		SQL: `SELECT id, state_nn, rank FROM ` + notesTable + `
WHERE group_id = @group_id
  AND CASE WHEN state IS NULL THEN 0 ELSE state END = @status
  AND rank >= @rank_ptr
  AND id >= @small_id`,
		Scope: tabula.ScopeParam("group_id"),
	})
	// Positive 2: int32 input against PG bigint.
	qInt32ToBigint := tabula.DefineSQL[noteRow, int32BigintArgs](reg, tabula.SQLSpec{
		Name:  "notes.int32_bigint",
		SQL:   `SELECT id, state_nn, rank FROM ` + notesTable + ` WHERE group_id = @group_id AND id = @id`,
		Scope: tabula.ScopeParam("group_id"),
	})
	// Positive 3: []int16 (,array) input against PG integer[].
	qInt16Array := tabula.DefineSQL[noteRow, int16ArrayArgs](reg, tabula.SQLSpec{
		Name:  "notes.int16_array",
		SQL:   `SELECT id, state_nn, rank FROM ` + notesTable + ` WHERE group_id = @group_id AND rank = ANY(@ranks)`,
		Scope: tabula.ScopeParam("group_id"),
	})
	// Positive 4: DefineSQLList keyset cursor on int16 column with integer-widened sort expression.
	qWidenedKeyset := tabula.DefineSQLList[noteRow, tabula.NoArgs](reg, tabula.SQLListSpec{
		Name:   "notes.keyset_int16",
		Select: `SELECT id, state_nn, rank FROM ` + notesTable,
		Where:  `group_id = @group_id`,
		Scope:  tabula.ScopeParam("group_id"),
		Order: tabula.QueryOrderSpec{
			Default:          "state",
			DefaultDirection: tabula.SortAsc,
			Sorts: []tabula.QuerySort{
				{Key: "state", Expr: "CASE WHEN state_nn >= 0 THEN state_nn ELSE 0 END", Column: "state_nn"},
			},
			TieBreaker: []tabula.QuerySortTerm{{Expr: "id", Column: "id"}},
		},
		MaxLimit: 20,
		Keyset:   true,
	})

	// All positive declarations pass both pgxdriver.SQLVerifier and database/sql, live and via snapshot.
	allDefs := sqlcheck.ViewsToDefinitions(reg.DefinedViews())
	failures, err := sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), allDefs, appRole)
	require.NoError(t, err)
	require.Empty(t, failures)

	failures, err = sqlcheck.VerifyStatements(ctx, db, allDefs, appRole)
	require.NoError(t, err)
	require.Empty(t, failures)

	snap := sqlcheck.RenderAllSnapshot(reg.Defined(), reg.DefinedViews())
	parsed, err := sqlcheck.ParseSnapshot(snap)
	require.NoError(t, err)
	failures, err = sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), parsed, appRole)
	require.NoError(t, err)
	require.Empty(t, failures)

	// Negative 1: Go int16 result scanning PG integer (COALESCE(smallint, 0)) must still be rejected.
	type narrowInt16Row struct {
		State int16 `db:"state"`
	}
	negReg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	badInt16Result := tabula.DefineSQL[narrowInt16Row, tabula.NoArgs](negReg, tabula.SQLSpec{
		Name: "neg.int16_from_integer",
		SQL:  `SELECT COALESCE(state, 0) AS state FROM ` + notesTable,
	})
	for _, verify := range []func([]tabula.Definition) ([]string, error){
		func(d []tabula.Definition) ([]string, error) {
			return sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), d, appRole)
		},
		func(d []tabula.Definition) ([]string, error) {
			return sqlcheck.VerifyStatements(ctx, db, d, appRole)
		},
	} {
		f, err := verify([]tabula.Definition{badInt16Result.Definition()})
		require.NoError(t, err)
		require.Len(t, f, 1)
		require.Contains(t, f[0], "not compatible")
	}

	// Negative 2: Go int32 result scanning PG bigint (COUNT(*)) must still be rejected.
	type narrowInt32Row struct {
		Total int32 `db:"total"`
	}
	badInt32Result := tabula.DefineSQL[narrowInt32Row, tabula.NoArgs](negReg, tabula.SQLSpec{
		Name: "neg.int32_from_bigint",
		SQL:  `SELECT count(*) AS total FROM ` + notesTable,
	})
	for _, verify := range []func([]tabula.Definition) ([]string, error){
		func(d []tabula.Definition) ([]string, error) {
			return sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), d, appRole)
		},
		func(d []tabula.Definition) ([]string, error) {
			return sqlcheck.VerifyStatements(ctx, db, d, appRole)
		},
	} {
		f, err := verify([]tabula.Definition{badInt32Result.Definition()})
		require.NoError(t, err)
		require.Len(t, f, 1)
		require.Contains(t, f[0], "not compatible")
	}

	// Runtime smoke on both stdsql (lib/pq) and pgxdriver (pgx/v5).
	minRank := int16(10)
	checkRuntime := func(c context.Context, s tabula.Session[int16]) error {
		rows, err := qInt16Widened.Bind(s).All(c, int16CaseArgs{Status: 2, RankPtr: &minRank, SmallID: 1})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, int16(2), rows[0].StateNN)

		got, err := qInt32ToBigint.Bind(s).One(c, int32BigintArgs{ID: int32(rows[0].ID)})
		require.NoError(t, err)
		require.Equal(t, rows[0].ID, got.ID)

		arrRows, err := qInt16Array.Bind(s).All(c, int16ArrayArgs{Ranks: []int16{10, 20}})
		require.NoError(t, err)
		require.Len(t, arrRows, 2)

		p1, err := qWidenedKeyset.Bind(s).ListKeyset(c, tabula.NoArgs{}, tabula.QueryKeyset{Limit: 1})
		require.NoError(t, err)
		require.True(t, p1.HasMore)
		require.Equal(t, int16(0), p1.Items[0].StateNN)

		p2, err := qWidenedKeyset.Bind(s).ListKeyset(c, tabula.NoArgs{}, tabula.QueryKeyset{Limit: 1, After: p1.Next})
		require.NoError(t, err)
		require.False(t, p2.HasMore)
		require.Equal(t, int16(2), p2.Items[0].StateNN)
		return nil
	}

	stdRunner, err := stdsql.NewRunner(db, stdsql.ScopePolicy[int16]{}, stdsql.RunnerOptions{
		Role:         appRole,
		SQLAuthority: reg.SQLAuthority(),
	})
	require.NoError(t, err)
	require.NoError(t, stdRunner.WithinReadOnly(ctx, 7, func(c context.Context, s *stdsql.Session[int16]) error {
		return checkRuntime(c, s)
	}))

	// pgxdriver uses pgxdriver.ArrayColumn for []int16 arrays.
	pgxReg := tabula.NewRegistry(tabula.RegistryConfig[int16]{
		Deny:        tabula.DenyLegacySQL,
		ArrayColumn: pgxdriver.ArrayColumn,
	})
	qInt16WidenedPGX := tabula.DefineSQL[noteRow, int16CaseArgs](pgxReg, tabula.SQLSpec{
		Name: "notes.int16_widened",
		SQL: `SELECT id, state_nn, rank FROM ` + notesTable + `
WHERE group_id = @group_id
  AND CASE WHEN state IS NULL THEN 0 ELSE state END = @status
  AND rank >= @rank_ptr
  AND id >= @small_id`,
		Scope: tabula.ScopeParam("group_id"),
	})
	qInt32ToBigintPGX := tabula.DefineSQL[noteRow, int32BigintArgs](pgxReg, tabula.SQLSpec{
		Name:  "notes.int32_bigint",
		SQL:   `SELECT id, state_nn, rank FROM ` + notesTable + ` WHERE group_id = @group_id AND id = @id`,
		Scope: tabula.ScopeParam("group_id"),
	})
	qInt16ArrayPGX := tabula.DefineSQL[noteRow, int16ArrayArgs](pgxReg, tabula.SQLSpec{
		Name:  "notes.int16_array",
		SQL:   `SELECT id, state_nn, rank FROM ` + notesTable + ` WHERE group_id = @group_id AND rank = ANY(@ranks)`,
		Scope: tabula.ScopeParam("group_id"),
	})
	qWidenedKeysetPGX := tabula.DefineSQLList[noteRow, tabula.NoArgs](pgxReg, tabula.SQLListSpec{
		Name:   "notes.keyset_int16",
		Select: `SELECT id, state_nn, rank FROM ` + notesTable,
		Where:  `group_id = @group_id`,
		Scope:  tabula.ScopeParam("group_id"),
		Order: tabula.QueryOrderSpec{
			Default:          "state",
			DefaultDirection: tabula.SortAsc,
			Sorts: []tabula.QuerySort{
				{Key: "state", Expr: "CASE WHEN state_nn >= 0 THEN state_nn ELSE 0 END", Column: "state_nn"},
			},
			TieBreaker: []tabula.QuerySortTerm{{Expr: "id", Column: "id"}},
		},
		MaxLimit: 20,
		Keyset:   true,
	})
	pgxRunner, err := pgxdriver.NewRunner(pool, pgxdriver.ScopePolicy[int16]{}, pgxdriver.RunnerOptions{
		Role:         appRole,
		SQLAuthority: pgxReg.SQLAuthority(),
	})
	require.NoError(t, err)
	require.NoError(t, pgxRunner.WithinReadOnly(ctx, 7, func(c context.Context, s *pgxdriver.Session[int16]) error {
		rows, err := qInt16WidenedPGX.Bind(s).All(c, int16CaseArgs{Status: 2, RankPtr: &minRank, SmallID: 1})
		require.NoError(t, err)
		require.Len(t, rows, 1)

		got, err := qInt32ToBigintPGX.Bind(s).One(c, int32BigintArgs{ID: int32(rows[0].ID)})
		require.NoError(t, err)
		require.Equal(t, rows[0].ID, got.ID)

		arrRows, err := qInt16ArrayPGX.Bind(s).All(c, int16ArrayArgs{Ranks: []int16{10, 20}})
		require.NoError(t, err)
		require.Len(t, arrRows, 2)

		p1, err := qWidenedKeysetPGX.Bind(s).ListKeyset(c, tabula.NoArgs{}, tabula.QueryKeyset{Limit: 1})
		require.NoError(t, err)
		require.True(t, p1.HasMore)
		p2, err := qWidenedKeysetPGX.Bind(s).ListKeyset(c, tabula.NoArgs{}, tabula.QueryKeyset{Limit: 1, After: p1.Next})
		require.NoError(t, err)
		require.False(t, p2.HasMore)
		require.Equal(t, int16(2), p2.Items[0].StateNN)
		return nil
	}))
}
