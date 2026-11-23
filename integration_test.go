//go:build integration

package tabula_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pqdriver"
)

type pgTxSession[S any] struct {
	tx       *sql.Tx
	tenant   S
	platform bool
}

func (s *pgTxSession[S]) Tenant() S      { return s.tenant }
func (s *pgTxSession[S]) Platform() bool { return s.platform }
func (s *pgTxSession[S]) Exec(ctx context.Context, _ S, query string, args ...any) (int64, error) {
	res, err := s.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
func (s *pgTxSession[S]) Scan(ctx context.Context, _ S, query string, args []any, dest ...any) error {
	return s.tx.QueryRowContext(ctx, query, args...).Scan(dest...)
}
func (s *pgTxSession[S]) Returning(ctx context.Context, _ S, query string, args []any, dest ...any) error {
	return s.tx.QueryRowContext(ctx, query, args...).Scan(dest...)
}
func (s *pgTxSession[S]) Each(ctx context.Context, _ S, query string, args []any, decode func(func(...any) error) error) error {
	rows, err := s.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := decode(rows.Scan); err != nil {
			return err
		}
	}
	return rows.Err()
}

func openIntegrationDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("TABULA_TEST_DSN")
	require.NotEmpty(t, dsn, "TABULA_TEST_DSN is required for integration tests")
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	schema := fmt.Sprintf("tabula_core_it_%d", time.Now().UnixNano())
	_, err = db.ExecContext(t.Context(), `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	return db, schema
}

func TestV03AgainstRealPostgres(t *testing.T) {
	db, schema := openIntegrationDB(t)
	ctx := t.Context()

	for _, ddl := range []string{
		`CREATE TABLE ` + schema + `.parents (id bigserial PRIMARY KEY, name text NOT NULL)`,
		`CREATE TABLE ` + schema + `.items (
			id bigserial PRIMARY KEY,
			tenant_id text NOT NULL,
			parent_id bigint REFERENCES ` + schema + `.parents(id),
			code text NOT NULL,
			amount integer NOT NULL CHECK (amount >= 0),
			tags text[],
			note text,
			created_by text NOT NULL,
			version integer NOT NULL DEFAULT 1,
			created_at timestamptz NOT NULL DEFAULT now(),
			deleted_at timestamptz,
			UNIQUE (tenant_id, code)
		)`,
		`CREATE TABLE ` + schema + `.pairs (
			chain_code text NOT NULL,
			symbol text NOT NULL,
			decimals integer NOT NULL,
			created_by text NOT NULL,
			PRIMARY KEY (chain_code, symbol)
		)`,
		`CREATE TABLE ` + schema + `.flags (
			id bigserial PRIMARY KEY,
			name text NOT NULL,
			is_deleted boolean NOT NULL DEFAULT false
		)`,
		`CREATE TABLE ` + schema + `.orders (
			id bigserial PRIMARY KEY,
			code text NOT NULL,
			status text NOT NULL DEFAULT 'active'
		)`,
	} {
		_, err := db.ExecContext(ctx, ddl)
		require.NoError(t, err, ddl)
	}

	type tenantScope string
	type itemRow struct {
		ID        int64       `db:"id,key,serial"`
		Tenant    tenantScope `db:"tenant_id,tenant"`
		ParentID  *int64      `db:"parent_id"`
		Code      string      `db:"code"`
		Amount    int         `db:"amount"`
		Tags      []string    `db:"tags,array"`
		Note      string      `db:"note,zero"`
		CreatedBy string      `db:"created_by,once"`
		Version   int         `db:"version,version"`
		CreatedAt time.Time   `db:"created_at,once"`
		DeletedAt *time.Time  `db:"deleted_at,deleted"`
	}

	reg := tabula.NewRegistry(tabula.RegistryConfig[tenantScope]{
		TenantID:    func(s tenantScope) any { return string(s) },
		ArrayArg:    pqdriver.Array,
		ArrayColumn: pqdriver.ArrayColumn,
	})
	items := tabula.Define(reg, tabula.Table[itemRow, int64, tenantScope]{
		Name:  schema + ".items",
		Order: "id ASC",
	})
	upsertItem := items.DefineUpsert("upsert_by_code", tabula.UpsertSpec{
		Conflict:  []string{"tenant_id", "code"},
		Update:    []string{"amount", "note", "version"},
		Where:     schema + ".items.deleted_at IS NULL",
		Returning: []string{"id", "version", "created_at"},
	})
	ignoreItem := items.DefineUpsert("ignore_by_code", tabula.UpsertSpec{
		Conflict: []string{"tenant_id", "code"},
	})

	t.Run("insert_returning_upsert_conditional_writes_and_errors", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		sess := &pgTxSession[tenantScope]{tx: tx, tenant: "t1"}
		r := items.Bind(sess)

		// InsertReturning populates serial id and DB-generated created_at.
		row := itemRow{Tenant: "t1", Code: "btc", Amount: 100, Tags: []string{"l1", "pow"}, CreatedBy: "alice", Version: 1, CreatedAt: time.Now().UTC()}
		require.NoError(t, r.InsertReturning(ctx, &row, "id", "created_at"))
		require.NotZero(t, row.ID)
		require.False(t, row.CreatedAt.IsZero())

		// SaveReturning updates and reads back values.
		row.Amount = 200
		row.Version = 2
		require.NoError(t, r.SaveReturning(ctx, &row, "amount", "version"))
		require.Equal(t, 200, row.Amount)

		// UpdateWhere: non-matching condition returns (false, nil).
		row.Version = 3
		matched, err := r.UpdateWhere(ctx, row, "amount = ?", 999)
		require.NoError(t, err)
		require.False(t, matched)

		// UpdateWhere: matching condition returns (true, nil).
		row.Amount = 250
		matched, err = r.UpdateWhere(ctx, row, "amount = ?", 200)
		require.NoError(t, err)
		require.True(t, matched)

		// SetWhere: conditional partial update.
		row.Note = "verified"
		matched, err = r.SetWhere(ctx, row, []string{"note"}, "code = ?", "btc")
		require.NoError(t, err)
		require.True(t, matched)

		// Upsert DO NOTHING returns false on duplicate.
		dup := itemRow{Tenant: "t1", Code: "btc", Amount: 1, CreatedBy: "bob", Version: 1, CreatedAt: time.Now().UTC()}
		ok, err := r.Upsert(ctx, ignoreItem, &dup)
		require.NoError(t, err)
		require.False(t, ok)

		// Upsert DO UPDATE updates amount/note/version and returns id.
		up := itemRow{Tenant: "t1", Code: "btc", Amount: 500, Note: "upserted", CreatedBy: "ignored_once", Version: 4, CreatedAt: time.Now().UTC()}
		ok, err = r.Upsert(ctx, upsertItem, &up)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, row.ID, up.ID)

		got, err := r.Get(ctx, "t1", row.ID)
		require.NoError(t, err)
		require.Equal(t, "alice", got.CreatedBy, "once column was not overwritten by upsert")
		require.Equal(t, 500, got.Amount)
		require.Equal(t, []string{"l1", "pow"}, got.Tags)

		// InsertMany writes multiple rows in one statement.
		require.NoError(t, r.InsertMany(ctx, []itemRow{
			{Tenant: "t1", Code: "eth", Amount: 10, CreatedBy: "alice", Version: 1, CreatedAt: time.Now().UTC()},
			{Tenant: "t1", Code: "sol", Amount: 20, CreatedBy: "alice", Version: 1, CreatedAt: time.Now().UTC()},
		}))
		all, err := r.Find(ctx, "t1", nil, tabula.Listing{})
		require.NoError(t, err)
		require.Len(t, all, 3)

		// Real PostgreSQL constraint errors map to distinct sentinels.
		_, err = tx.ExecContext(ctx, "SAVEPOINT sp_err")
		require.NoError(t, err)
		err = r.Insert(ctx, &itemRow{Tenant: "t1", Code: "eth", Amount: 1, CreatedBy: "a", Version: 1, CreatedAt: time.Now().UTC()})
		require.ErrorIs(t, err, tabula.ErrUnique)
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT sp_err")

		err = r.Insert(ctx, &itemRow{Tenant: "t1", Code: "neg", Amount: -5, CreatedBy: "a", Version: 1, CreatedAt: time.Now().UTC()})
		require.ErrorIs(t, err, tabula.ErrCheck)
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT sp_err")

		badParent := int64(999999)
		err = r.Insert(ctx, &itemRow{Tenant: "t1", ParentID: &badParent, Code: "fk", Amount: 1, CreatedBy: "a", Version: 1, CreatedAt: time.Now().UTC()})
		require.ErrorIs(t, err, tabula.ErrForeignKey)
		_, _ = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT sp_err")
	})

	t.Run("composite_keys_and_configurable_soft_delete", func(t *testing.T) {
		noTenantReg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})

		type pairKey struct {
			Chain  string `db:"chain_code"`
			Symbol string `db:"symbol"`
		}
		type pairRow struct {
			Chain     string `db:"chain_code,key"`
			Symbol    string `db:"symbol,key"`
			Decimals  int    `db:"decimals"`
			CreatedBy string `db:"created_by,once"`
		}
		pairs := tabula.Define(noTenantReg, tabula.Table[pairRow, pairKey, struct{}]{
			Name:  schema + ".pairs",
			Order: "chain_code ASC, symbol ASC",
		})

		type flagRow struct {
			ID        int64  `db:"id,key,serial"`
			Name      string `db:"name"`
			IsDeleted bool   `db:"is_deleted,deleted"`
		}
		flags := tabula.Define(noTenantReg, tabula.Table[flagRow, int64, struct{}]{
			Name: schema + ".flags",
		})

		type orderRow struct {
			ID     int64  `db:"id,key,serial"`
			Code   string `db:"code"`
			Status string `db:"status,deleted"`
		}
		orders := tabula.Define(noTenantReg, tabula.Table[orderRow, int64, struct{}]{
			Name:       schema + ".orders",
			SoftDelete: &tabula.SoftDelete{DeletedValue: "deleted"},
		})

		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		sess := &pgTxSession[struct{}]{tx: tx}

		pr := pairs.Bind(sess)
		require.NoError(t, pr.InsertMany(ctx, []pairRow{
			{Chain: "ETH", Symbol: "USDC", Decimals: 6, CreatedBy: "sys"},
			{Chain: "SOL", Symbol: "USDT", Decimals: 6, CreatedBy: "sys"},
		}))
		locked, err := pr.LockMany(ctx, struct{}{}, []pairKey{
			{Chain: "SOL", Symbol: "USDT"},
			{Chain: "ETH", Symbol: "USDC"},
		})
		require.NoError(t, err)
		require.Len(t, locked, 2)
		require.Equal(t, "ETH", locked[0].Chain)
		locked[0].Decimals = 18
		require.NoError(t, pr.Save(ctx, locked[0]))
		gotPair, err := pr.Get(ctx, struct{}{}, pairKey{Chain: "ETH", Symbol: "USDC"})
		require.NoError(t, err)
		require.Equal(t, 18, gotPair.Decimals)

		// Boolean soft delete.
		fr := flags.Bind(sess)
		fRow := flagRow{Name: "beta"}
		require.NoError(t, fr.Insert(ctx, &fRow))
		fRow.IsDeleted = true
		require.NoError(t, fr.SoftDelete(ctx, fRow))
		missingFlag, err := fr.Get(ctx, struct{}{}, fRow.ID)
		require.NoError(t, err)
		require.Nil(t, missingFlag)

		// Status-value soft delete.
		or := orders.Bind(sess)
		oRow := orderRow{Code: "ord-1", Status: "pending"}
		require.NoError(t, or.Insert(ctx, &oRow))
		foundOrder, err := or.Get(ctx, struct{}{}, oRow.ID)
		require.NoError(t, err)
		require.NotNil(t, foundOrder)
		oRow.Status = "deleted"
		require.NoError(t, or.SoftDelete(ctx, oRow))
		missingOrder, err := or.Get(ctx, struct{}{}, oRow.ID)
		require.NoError(t, err)
		require.Nil(t, missingOrder)
	})

	t.Run("keyset_pagination_count_modes_and_nested_view_join", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		sess := &pgTxSession[tenantScope]{tx: tx, tenant: "tk"}

		var parentID int64
		require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO `+schema+`.parents (name) VALUES ('primary') RETURNING id`).Scan(&parentID))

		r := items.Bind(sess)
		now := time.Now().UTC()
		require.NoError(t, r.InsertMany(ctx, []itemRow{
			{Tenant: "tk", ParentID: &parentID, Code: "a1", Amount: 10, CreatedBy: "u", Version: 1, CreatedAt: now},
			{Tenant: "tk", ParentID: &parentID, Code: "a2", Amount: 20, CreatedBy: "u", Version: 1, CreatedAt: now},
			{Tenant: "tk", ParentID: &parentID, Code: "a3", Amount: 20, CreatedBy: "u", Version: 1, CreatedAt: now},
			{Tenant: "tk", ParentID: &parentID, Code: "a4", Amount: 30, CreatedBy: "u", Version: 1, CreatedAt: now},
		}))

		// Keyset DESC on (amount, id): page 1 gets a4 (30) and a3 (20), HasMore=true.
		p1, err := r.ListKeyset(ctx, "tk", nil, tabula.KeysetListing{SortColumn: "amount", Direction: "DESC", Limit: 2})
		require.NoError(t, err)
		require.True(t, p1.HasMore)
		require.Len(t, p1.Items, 2)
		require.Equal(t, "a4", p1.Items[0].Code)
		require.Equal(t, "a3", p1.Items[1].Code)

		// Page 2 resumes from last item's (amount, id) cursor without duplicates.
		last := p1.Items[len(p1.Items)-1]
		p2, err := r.ListKeyset(ctx, "tk", nil, tabula.KeysetListing{
			SortColumn: "amount",
			Direction:  "DESC",
			After:      []any{last.Amount, last.ID},
			Limit:      2,
		})
		require.NoError(t, err)
		require.False(t, p2.HasMore)
		require.Len(t, p2.Items, 2)
		require.Equal(t, "a2", p2.Items[0].Code)
		require.Equal(t, "a1", p2.Items[1].Code)

		// CountNone and CountEstimate.
		listNone, totalNone, err := r.List(ctx, "tk", func(f *tabula.Filter) {
			f.In("code", "a1", "a4")
		}, tabula.Listing{Count: tabula.CountNone})
		require.NoError(t, err)
		require.Equal(t, int64(-1), totalNone)
		require.Len(t, listNone, 2)

		_, totalEst, err := r.List(ctx, "tk", nil, tabula.Listing{Count: tabula.CountEstimate})
		require.NoError(t, err)
		require.GreaterOrEqual(t, totalEst, int64(0))

		// Nested struct prefix View over real SQL JOIN.
		type itemPart struct {
			Code   string `db:"code"`
			Amount int    `db:"amount"`
		}
		type parentPart struct {
			Name string `db:"name"`
		}
		type itemWithParent struct {
			Item   itemPart   `db:"i_"`
			Parent parentPart `db:"p_"`
		}
		joinView := tabula.DefineView(reg, tabula.View[itemWithParent, tenantScope]{Name: schema + ".items_with_parent"})
		sel := joinView.SelectAs(map[string]string{"i_": "i", "p_": "p"})
		q := "SELECT " + sel + " FROM " + schema + ".items i JOIN " + schema + ".parents p ON p.id = i.parent_id WHERE i.tenant_id = $1 ORDER BY i.code ASC"
		joined, err := joinView.Bind(sess).Select(ctx, "tk", q, "tk")
		require.NoError(t, err)
		require.Len(t, joined, 4)
		require.Equal(t, "a1", joined[0].Item.Code)
		require.Equal(t, "primary", joined[0].Parent.Name)
	})
}
