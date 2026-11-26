package sqlcheck

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestCollectSQL_V031ComposedQueriesAggregatesAndSecurityCallSites(t *testing.T) {
	// Point temp module at this checkout of tabula via GOWORK or replace in test dir.
	repoRoot, err := filepath.Abs("..")
	require.NoError(t, err)

	dir := t.TempDir()
	write := func(name, data string) {
		path := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, []byte(data), 0600))
	}
	t.Setenv("GOWORK", "off")
	write("go.mod", "module example.test/audit\n\ngo 1.26.0\n\nrequire github.com/0xJean/tabula v0.3.0\n\nreplace github.com/0xJean/tabula => "+filepath.ToSlash(repoRoot)+"\n")
	write("descriptors.go", `package audit
import tb "github.com/0xJean/tabula"
var registry = tb.NewRegistry(tb.RegistryConfig[string]{})
var shared = tb.DefineView(registry, tb.ViewSpec[item]{Name: "shared_view"})
var mutable = shared
var Exported = shared
`)
	write("app.go", `package audit

import (
	"context"
	"database/sql"

	tb "github.com/0xJean/tabula"
	ss "github.com/0xJean/tabula/stdsql"
)

type item struct {
	ID     int64  `+"`db:\"id,key\"`"+`
	Tenant string `+"`db:\"tenant_id,scope\"`"+`
	Rank   int64  `+"`db:\"rank\"`"+`
	Kind   string `+"`db:\"kind\"`"+`
}

func setup(ctx context.Context, db *sql.DB, dynKey, dynCol string) {
	key := tb.NewSessionKey()
	escapedKeyFn := tb.NewSessionKey
	_ = escapedKeyFn

	reg := tb.NewRegistry(tb.RegistryConfig[string]{RequireSealed: true})
	_ = reg.Seal(key)
	signFn := key.Sign
	_ = signFn

	tbl := tb.Define(reg, tb.TableSpec[item, int64]{Name: "items", GroupBy: []string{"kind"}})
	view := tb.DefineView(reg, tb.ViewSpec[item]{Name: "items_view"})
	mutable = view
	_, _ = shared.TryDefineQuery("private", tb.QuerySpec{})
	_, _ = mutable.TryDefineQuery("mutable", tb.QuerySpec{})
	_, _ = Exported.TryDefineQuery("exported", tb.QuerySpec{})
	_ = view.DefineQuery("items.search", tb.QuerySpec{
		Select:   "SELECT id, tenant_id, rank, kind FROM items",
		Scope:    tb.ScopeColumns("tenant_id"),
		MaxLimit: 50,
	})
	_ = view.DefineQuery(dynKey, tb.QuerySpec{
		Select:   "SELECT id, tenant_id, rank, kind FROM items",
		Scope:    tb.ScopeColumns("tenant_id"),
		MaxLimit: 50,
	})

	runner, _ := ss.NewRunner(db, ss.ScopePolicy[string]{}, ss.RunnerOptions{
		SessionKey: key,
		ExposeTx:   true,
	})
	_ = runner.WithinRaw(ctx, "t1", func(txCtx context.Context, rs *ss.RawSession[string]) error {
		_ = rs.Tx()
		detached := tb.WithoutTransaction(txCtx)
		_ = tb.InTransaction(detached)
		_ = tb.MarkTransaction(detached)
		rows := tbl.Bind(rs)
		_, _, _ = tb.Max[int64](txCtx, rows, "rank", (nil))
		_, _, _ = tb.Avg[float64](txCtx,rows,"rank",func(f *tb.Filter){f.And("rank > ?", 0)})
		_, _ = tb.CountBy[string](txCtx, rows, "kind", nil)
		_, _, _ = tb.Min[int64](txCtx, rows, dynCol, nil)
		_, _ = rows.Exists(txCtx, nil)
		return nil
	})
	_, _ = ss.Direct("postgres", "postgres://localhost/db", ss.ScopePolicy[string]{}, ss.DirectSessionKey(key))
	_ = ss.NewSession[string](nil, "t1", ss.ScopePolicy[string]{})
}
`)

	inv, err := CollectSQL(t.Context(), InventoryConfig{
		Dir:                dir,
		AuditSecurityCalls: true,
	})
	require.NoError(t, err)

	// Verify composed query and aggregate SQL sites.
	byMethod := map[string][]SQLSite{}
	for _, s := range inv.Sites {
		byMethod[s.Method] = append(byMethod[s.Method], s)
	}
	require.Len(t, byMethod["DefineQuery"], 2)
	var staticDQ, dynamicDQ int
	for _, s := range byMethod["DefineQuery"] {
		require.Equal(t, "composed_query", s.Kind)
		if s.Dynamic {
			dynamicDQ++
		} else {
			staticDQ++
			require.Equal(t, "items.search", s.QueryKey)
			require.Equal(t, "items_view", s.Definition)
		}
	}
	require.Equal(t, 1, staticDQ)
	require.Equal(t, 1, dynamicDQ)
	owners := map[string]string{}
	for _, site := range byMethod["TryDefineQuery"] {
		owners[site.QueryKey] = site.Definition
	}
	require.Equal(t, map[string]string{"private": "shared_view", "mutable": "", "exported": ""}, owners)

	require.Len(t, byMethod["Max"], 1)
	require.False(t, byMethod["Max"][0].Dynamic)
	require.Equal(t, "max:rank", byMethod["Max"][0].QueryKey)
	require.True(t, byMethod["Max"][0].NoFilter)
	require.Len(t, byMethod["Avg"], 1)
	require.False(t, byMethod["Avg"][0].NoFilter)

	require.Len(t, byMethod["CountBy"], 1)
	require.False(t, byMethod["CountBy"][0].Dynamic)
	require.Equal(t, "countby:kind", byMethod["CountBy"][0].QueryKey)
	require.Equal(t, "items", byMethod["CountBy"][0].Definition)

	require.Len(t, byMethod["Min"], 1)
	require.True(t, byMethod["Min"][0].Dynamic)

	require.Len(t, byMethod["Exists"], 1)
	require.False(t, byMethod["Exists"][0].Dynamic)
	require.Equal(t, "items", byMethod["Exists"][0].Definition)

	// Verify dynamic sites generated findings.
	require.Len(t, inv.Findings, 2)

	// Verify typed security call sites (including aliases & escaped function/method values).
	hasCall := func(sym string, escaped bool) bool {
		for _, cs := range inv.CallSites {
			if cs.Symbol == sym && cs.Escaped == escaped {
				return true
			}
		}
		return false
	}
	require.True(t, hasCall("github.com/0xJean/tabula.NewSessionKey", false))
	require.True(t, hasCall("github.com/0xJean/tabula.NewSessionKey", true), "escaped function value must be flagged")
	require.True(t, hasCall("github.com/0xJean/tabula.Registry.Seal", false))
	require.True(t, hasCall("github.com/0xJean/tabula.SessionKey.Sign", true), "escaped method value must be flagged")
	require.True(t, hasCall("github.com/0xJean/tabula/stdsql.NewRunner", false))
	require.True(t, hasCall("github.com/0xJean/tabula/stdsql.RunnerOptions.ExposeTx", false))
	require.True(t, hasCall("github.com/0xJean/tabula/stdsql.Runner.WithinRaw", false))
	require.True(t, hasCall("github.com/0xJean/tabula/stdsql.RawSession.Tx", false))
	require.True(t, hasCall("github.com/0xJean/tabula.WithoutTransaction", false))
	require.True(t, hasCall("github.com/0xJean/tabula.InTransaction", false))
	require.True(t, hasCall("github.com/0xJean/tabula.MarkTransaction", false))
	require.True(t, hasCall("github.com/0xJean/tabula/stdsql.DirectSessionKey", false))
	require.True(t, hasCall("github.com/0xJean/tabula/stdsql.NewSession", false))

	// Verify BindRolesWithDefinitions expands composed-query C3 variants without manual listing.
	type viewRow struct {
		ID   int64  `db:"id"`
		Kind string `db:"kind"`
	}
	reg := tabula.NewRegistry(tabula.RegistryConfig[string]{})
	v := tabula.DefineView(reg, tabula.ViewSpec[viewRow]{Name: "items_view"})
	v.DefineQuery("items.search", tabula.QuerySpec{
		Select:  "SELECT id, kind FROM items",
		Scope:   tabula.ScopeColumns("tenant_id"),
		Filters: []tabula.QueryFilter{{Name: "kind", SQL: "kind = $1"}},
		Order: tabula.QueryOrderSpec{
			Default:          "id",
			DefaultDirection: tabula.SortAsc,
			Sorts:            []tabula.QuerySort{{Key: "id", Expr: "id", Column: "id"}},
		},
		MaxLimit: 50,
	})
	work := RoleStatements{}
	work.AddViews("scope_reader", reg.DefinedViews()...)
	require.GreaterOrEqual(t, len(work["scope_reader"]), 4)
	require.NoError(t, work.validate())

	cleanInv := SQLInventory{
		Sites: []SQLSite{{
			ID:       "app.go#dq-1",
			Kind:     "composed_query",
			QueryKey: "items.search",
		}},
	}
	bound, err := cleanInv.BindRolesWithDefinitions(
		map[string]SQLBinding{"app.go#dq-1": {Roles: []string{"scope_reader"}, Definition: "items_view"}},
		nil,
		reg.DefinedViews(),
	)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(bound["scope_reader"]), 4)
}
