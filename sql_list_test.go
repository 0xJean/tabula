package tabula_test

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestDefineSQLListVariantsAndExecution(t *testing.T) {
	type itemRow struct {
		ID   int64  `db:"id"`
		Name string `db:"name"`
	}
	type listArgs struct {
		Status     *string `db:"status"`
		Search     *string `db:"search,secret"`
		OnlyActive bool    `db:"only_active"`
		MinID      int64   `db:"min_id"`
		MaxID      int64   `db:"max_id"`
		HasRange   bool    `db:"-"`
	}

	reg := tabula.NewRegistry(tabula.RegistryConfig[int64]{
		Deny: tabula.DenyLegacySQL,
	})

	list := tabula.DefineSQLList[itemRow, listArgs](reg, tabula.SQLListSpec{
		Name:      "items.search",
		Select:    "SELECT id, name FROM public.items",
		Where:     "tenant_id = @tenant_id",
		CountBase: "SELECT count(*) FROM public.items",
		Scope:     tabula.ScopeParam("tenant_id"),
		Filters: []tabula.SQLFilter{
			{Name: "status", SQL: "status = @status"}, // auto-inferred pointer
			{Name: "search", SQL: "(name ILIKE @search OR code ILIKE @search)"},
			{Name: "active", SQL: "deleted_at IS NULL", When: "only_active"},
			{Name: "id_range", SQL: "id BETWEEN @min_id AND @max_id", When: "HasRange"},
		},
		Order: tabula.QueryOrderSpec{
			Default:          "id",
			DefaultDirection: tabula.SortAsc,
			Sorts: []tabula.QuerySort{
				{Key: "id", Expr: "id", Column: "id"},
				{Key: "name", Expr: "name", Column: "name"},
			},
			TieBreaker: []tabula.QuerySortTerm{
				{Expr: "id", Column: "id"},
			},
		},
		MaxLimit:  100,
		MaxOffset: 1000,
		Keyset:    true,
	})

	def := list.Definition()
	for _, key := range []string{
		"find", "page", "count",
		"all.page", "all.count",
		"filter.status.page", "filter.status.count",
		"filter.search.page", "filter.search.count",
		"filter.active.page", "filter.active.count",
		"filter.id_range.page", "filter.id_range.count",
		"sort.name.page",
		"keyset.id.first", "keyset.id.after",
		"keyset.name.first", "keyset.name.after",
	} {
		require.Contains(t, def.Statements, key)
		require.Contains(t, def.StatementShapes, key)
		require.True(t, def.StatementShapes[key].StructSQL)
		require.Equal(t, "items.search", def.StatementQueries[key])
	}

	// Snapshot round-trip succeeds cleanly and preserves StructSQL.
	snap := sqlcheck.RenderAllSnapshot(reg.Defined(), reg.DefinedViews())
	parsed, err := sqlcheck.ParseSnapshot(snap)
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	require.Len(t, parsed[0].StatementShapes, len(def.StatementShapes))
	for _, shape := range parsed[0].StatementShapes {
		require.True(t, shape.StructSQL)
	}

	// Execute List with subset of filters {status, active} via AuthorizedSession (never text!).
	status := "open"
	sess := &fakeAuthSession[int64]{
		scope:     7,
		authority: reg.SQLAuthority(),
		scanVal:   int64(2),
		eachRows:  [][]any{{int64(10), "alpha"}, {int64(11), "beta"}},
	}
	bound := list.Bind(sess)
	rows, total, err := bound.List(t.Context(), listArgs{
		Status:     &status,
		OnlyActive: true,
	}, tabula.QueryPage{Sort: "name", Direction: tabula.SortDesc, Size: 20, Offset: 0})
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Equal(t, []itemRow{{ID: 10, Name: "alpha"}, {ID: 11, Name: "beta"}}, rows)
	require.Zero(t, sess.textCalls)
	require.Equal(t, 2, sess.authCalls)
	require.Contains(t, sess.lastSQL.Query, "status = $2")
	require.Contains(t, sess.lastSQL.Query, "deleted_at IS NULL")
	require.NotContains(t, sess.lastSQL.Query, "ILIKE")
	require.NotContains(t, sess.lastSQL.Query, "BETWEEN")
	require.Contains(t, sess.lastSQL.Query, "ORDER BY name DESC, id DESC LIMIT $3 OFFSET $4")
	require.Equal(t, []any{int64(7), "open", 20, 0}, sess.lastSQL.Args)

	// Keyset page extracts Next cursor and validates After types.
	sess.eachRows = [][]any{{int64(1), "a"}, {int64(2), "b"}, {int64(3), "c"}}
	kp, err := bound.ListKeyset(t.Context(), listArgs{}, tabula.QueryKeyset{Sort: "id", Direction: tabula.SortAsc, Limit: 2})
	require.NoError(t, err)
	require.True(t, kp.HasMore)
	require.Equal(t, []itemRow{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}}, kp.Items)
	require.Equal(t, []any{int64(2)}, kp.Next)

	// Reject bad filter When inference.
	type badArgs struct {
		Code string `db:"code"`
	}
	_, err = tabula.TryDefineSQLList[itemRow, badArgs](reg, tabula.SQLListSpec{
		Name:   "items.bad_when",
		Select: "SELECT id, name FROM public.items",
		Scope:  tabula.ScopeByRLS(),
		Filters: []tabula.SQLFilter{
			{Name: "code", SQL: "code = @code"}, // non-pointer without When
		},
		Order: tabula.QueryOrderSpec{
			Default: "id",
			Sorts:   []tabula.QuerySort{{Key: "id", Expr: "id"}},
		},
		MaxLimit: 50,
	})
	require.ErrorContains(t, err, "specify When explicitly")
}

type defaultEncodedKey struct {
	raw string
}

func (k defaultEncodedKey) Value() (driver.Value, error) {
	return nil, errors.New("defaultEncodedKey must be encoded through RegistryConfig.DefaultTypes")
}

func (k *defaultEncodedKey) Scan(src any) error {
	switch v := src.(type) {
	case string:
		k.raw = v
		return nil
	default:
		return fmt.Errorf("unexpected %T", src)
	}
}

func TestDefineSQLListKeysetDefaultTypesEncoder(t *testing.T) {
	type keyRow struct {
		ID  int64             `db:"id"`
		Key defaultEncodedKey `db:"key"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{
		Deny: tabula.DenyLegacySQL,
		DefaultTypes: []tabula.TypeDefault{
			tabula.TypeMapping(tabula.CatalogType("pg_catalog", "text", func(v defaultEncodedKey) any {
				return "enc:" + v.raw
			})),
		},
	})
	list := tabula.DefineSQLList[keyRow, tabula.NoArgs](reg, tabula.SQLListSpec{
		Name:   "items.by_key",
		Select: "SELECT id, key FROM public.items",
		Order: tabula.QueryOrderSpec{
			Default:          "key",
			DefaultDirection: tabula.SortAsc,
			Sorts:            []tabula.QuerySort{{Key: "key", Expr: "key", Column: "key"}},
			TieBreaker:       []tabula.QuerySortTerm{{Expr: "id", Column: "id"}},
		},
		MaxLimit: 10,
		Keyset:   true,
	})
	sess := &fakeAuthSession[struct{}]{
		authority: reg.SQLAuthority(),
		eachRows:  [][]any{{int64(2), "beta"}},
	}
	page, err := list.Bind(sess).ListKeyset(t.Context(), tabula.NoArgs{}, tabula.QueryKeyset{
		Sort:      "key",
		Direction: tabula.SortAsc,
		Limit:     5,
		After:     []any{defaultEncodedKey{raw: "alpha"}, int64(1)},
	})
	require.NoError(t, err)
	require.False(t, page.HasMore)
	require.Len(t, page.Items, 1)
	require.Equal(t, defaultEncodedKey{raw: "beta"}, page.Items[0].Key)
	require.Equal(t, []any{"enc:alpha", int64(1), 6}, sess.lastSQL.Args)
}

func TestDefineSQLListKeysetInputIntegerWidening(t *testing.T) {
	type statusRow struct {
		ID     int64 `db:"id"`
		Status int16 `db:"status"`
		Rank   int32 `db:"rank"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{
		Deny: tabula.DenyLegacySQL,
	})
	list := tabula.DefineSQLList[statusRow, tabula.NoArgs](reg, tabula.SQLListSpec{
		Name:   "items.by_status",
		Select: "SELECT id, status, rank FROM public.items",
		Order: tabula.QueryOrderSpec{
			Default:          "status",
			DefaultDirection: tabula.SortAsc,
			Sorts: []tabula.QuerySort{
				{Key: "status", Expr: "status", Column: "status"},
				{Key: "rank", Expr: "rank", Column: "rank"},
			},
			TieBreaker: []tabula.QuerySortTerm{{Expr: "id", Column: "id"}},
		},
		MaxLimit: 20,
		Keyset:   true,
	})

	shapes := list.Definition().StatementShapes
	// Result fields keep strict narrowing bounds.
	statusAfter := shapes["keyset.status.after"]
	require.Equal(t, []string{"smallint"}, statusAfter.Result[1].AllowedTypes)
	require.Equal(t, []string{"smallint", "integer"}, statusAfter.Result[2].AllowedTypes)
	// Keyset after_* cursor inputs propagate Input direction (lossless widening).
	require.Equal(t, "after_status", statusAfter.Inputs[0].Name)
	require.Equal(t, []string{"smallint", "integer", "bigint"}, statusAfter.Inputs[0].AllowedTypes)

	rankAfter := shapes["keyset.rank.after"]
	require.Equal(t, "after_rank", rankAfter.Inputs[0].Name)
	require.Equal(t, []string{"smallint", "integer", "bigint"}, rankAfter.Inputs[0].AllowedTypes)
}
