//go:build integration

package integration_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/stretchr/testify/require"
)

type statementCounter struct{ calls atomic.Int64 }

func (c *statementCounter) Start(ctx context.Context, _ q.TraceInfo) (context.Context, func(int64, error)) {
	c.calls.Add(1)
	return ctx, func(int64, error) {}
}

func TestReuseBindingAcrossPlans(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"stdsql", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			schema, role := prepareJoinedTables(t, db)
			_, err := db.ExecContext(t.Context(), "GRANT UPDATE(name), DELETE ON "+schema+".groups TO "+role)
			require.NoError(t, err)
			_, err = db.ExecContext(t.Context(), "CREATE FUNCTION "+schema+".first_id(bigint[]) RETURNS bigint LANGUAGE sql IMMUTABLE STRICT AS 'SELECT ($1)[1]'")
			require.NoError(t, err)

			var encodings, adaptations atomic.Int64
			var counter statementCounter
			reg := q.NewRegistry(q.RegistryConfig[itemScope]{
				Deny: q.DenyLegacySQL, Tracer: &counter,
				DefaultTypes: []q.TypeDefault{q.TypeMapping(q.CatalogType[itemScope]("pg_catalog", "int8", nil))},
				ArrayArg: func(value any) any {
					// A driver adapter is allowed to modify its own argument.
					members := value.([]int64)
					adaptations.Add(1)
					if len(members) > 0 {
						members[0]++
					}
					if driver == "stdsql" {
						return pqdriver.Array(members)
					}
					return members
				},
			})
			items := q.DefinePlanSource(reg, q.TableSpec[joinGroup, int64]{Name: schema + ".groups", AllowDelete: true})
			layout := plan.MapTable(items)
			src, peer := layout.As("g"), layout.As("peer")
			id := plan.MappedColumn[int64](layout, "id")
			field := id.On(src.Rel())
			members := q.ArrayOf(plan.Int8())
			ids := plan.Input("ids", members)
			filter := plan.RowFilter{Where: plan.And(plan.Any(field.Expr(), ids.Expr()), plan.Any(field.Expr(), ids.Expr()))}
			invalid := errors.New("invalid label")
			label := plan.Input("label", plan.Text().WithEncoder(func(s string) (any, error) {
				encodings.Add(1)
				if s == "" {
					return nil, invalid
				}
				return s + "!", nil
			}))
			out := q.DefineView(reg, q.ViewSpec[planResult]{Name: "entries"})
			projection := []plan.Projection{plan.Project(plan.Output(out, "id", plan.Int8()), field.Expr()), plan.Project(plan.Output(out, "label", plan.Text()), label.Expr())}
			spec := plan.RowsSpec{RowFilter: filter, WithExists: true, Orders: []plan.RowOrder{{Name: "id", Terms: []plan.RowTerm{plan.Asc(field)}}}, Pages: plan.RowPages{Keyset: true, Unpaged: true, MaxLimit: 5}}
			page := plan.DefineRowsWithForms(out, "lookup", src, spec, plan.ReadForms{Exact: true, First: plan.OrderedFirst}, projection...)
			probe := plan.DefineExists(src, "visible", filter)
			join := plan.DefineRead(out, "pairs", plan.Select(projection...).From(src).
				InnerJoin(peer, plan.Eq(field.Expr(), id.On(peer.Rel()).Expr())).Where(filter.Where).WithForms(plan.ReadForms{Exact: true}))
			edit := plan.MutationSpec{RowFilter: filter}
			name := plan.MappedColumn[string](layout, "name").On(src.Rel())
			newName := plan.Input("name", plan.Text())
			update := plan.DefineUpdateMany(src, "change_name", edit, plan.RowSet{Name: "label", Assignments: []plan.RowAssignment{plan.Assign(name, newName.Expr())}})
			remove := plan.DefineDeleteMany(src, "remove", edit)
			result := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "notes"})
			fn := plan.DeclareRoutine(plan.RoutineSpec[int64]{Schema: schema, Name: "first_id", Result: plan.Int8(),
				Cardinality: plan.RoutineOne, ReadOnly: true, Nullable: true}, plan.Parameter("members", members))
			// The tested input has a first member, but an empty input can
			// return NULL, so declare that result as nullable.
			type nullableResult struct {
				ID *int64 `db:"id"`
			}
			nullable := q.DefineView(reg, q.ViewSpec[nullableResult]{Name: "regions"})
			readMember := plan.DefineRoutine(nullable, "first_member", fn, plan.Arg(plan.FormalOf(fn, "members", members), ids.Expr()))
			advisoryKey := id.Input("lock_id")
			abs := plan.DeclareRoutine(plan.RoutineSpec[int64]{Schema: "pg_catalog", Name: "abs", Result: plan.Int8(),
				Cardinality: plan.RoutineOne, ReadOnly: true}, plan.Parameter("value", plan.Int8()))
			magnitude := plan.DefineRoutine(result, "magnitude", abs, plan.Arg(plan.FormalOf(abs, "value", plan.Int8()), advisoryKey.Expr()))
			flags := q.DefineView(reg, q.ViewSpec[advisoryResult]{Name: "jobs"})
			lock := plan.DefineAdvisoryLock(flags, "hold_key", plan.AdvisoryXactLock(plan.Exclusive, plan.Try, advisoryKey.Expr()))

			original := []int64{0, 4, 5}
			binding := plan.BindValue(ids, original)
			original[0] = 8
			desc := plan.BindValue(label, "visible")
			want := planResult{ID: 1, Label: new("visible!")}
			run := func(body rowCallback) error {
				return runRowPlan(t, db, pool, driver, reg.SQLAuthority(), role, 1, q.LogConfig{}, body)
			}
			require.NoError(t, run(func(ctx context.Context, s q.Session[itemScope]) error {
				b := page.Bind(s)
				choice := plan.RowChoice{Order: "id"}
				base := adaptations.Load()
				rows, total, err := b.List(ctx, choice, desc, binding)
				require.NoError(t, err)
				require.EqualValues(t, 1, total)
				require.Equal(t, []planResult{want}, rows)
				require.EqualValues(t, 2, adaptations.Load()-base)
				require.EqualValues(t, 1, encodings.Load())
				got, err := b.One(ctx, choice, binding, desc)
				require.NoError(t, err)
				require.Equal(t, want, got)
				got, ok, err := b.MaybeFirst(ctx, choice, desc, binding)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, want, got)
				got, err = join.Bind(s).One(ctx, binding, desc)
				require.NoError(t, err)
				require.Equal(t, want, got)
				member, err := readMember.Bind(s).One(ctx, binding)
				require.NoError(t, err)
				require.Equal(t, new(int64(1)), member.ID)
				lockID := plan.BindValue(advisoryKey, -19834)
				number, err := magnitude.Bind(s).One(ctx, lockID)
				require.NoError(t, err)
				require.EqualValues(t, 19834, number.ID)
				held, err := lock.Bind(s).Acquire(ctx, lockID)
				require.NoError(t, err)
				require.True(t, held)
				var competing bool
				require.NoError(t, db.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1::bigint)", -19834).Scan(&competing))
				require.False(t, competing)
				after, err := page.Cursor("id", plan.At(field, int64(1)))
				require.NoError(t, err)
				rows, total, err = b.List(ctx, plan.RowChoice{Order: "id", Page: plan.AfterPage, Limit: 1, After: after}, desc, binding)
				require.NoError(t, err)
				require.Empty(t, rows)
				require.EqualValues(t, 1, total)
				found, err := b.Exists(ctx, nil, binding)
				require.NoError(t, err)
				require.True(t, found)
				found, err = probe.Bind(s).Exists(ctx, nil, binding)
				require.NoError(t, err)
				require.True(t, found)
				before := counter.calls.Load()
				for _, values := range [][]plan.Binding{{binding}, {desc, binding, binding}, {desc, binding, plan.BindValue(id.Input("extra"), 1)}} {
					_, _, err = b.List(ctx, choice, values...)
					require.Error(t, err)
				}
				_, _, err = b.List(ctx, choice, binding, plan.BindValue(label, ""))
				require.ErrorIs(t, err, invalid)
				_, err = b.Exists(ctx, nil, desc, binding)
				require.Error(t, err)
				require.Equal(t, before, counter.calls.Load())
				n, err := update.Bind(s).UpdateMany(ctx, "label", nil, binding, plan.BindValue(newName, "changed"))
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				return nil
			}))
			base := adaptations.Load()
			var wg sync.WaitGroup
			type outcome struct {
				found bool
				err   error
			}
			results := make(chan outcome, 2)
			for range 2 {
				wg.Go(func() {
					var found bool
					err := run(func(ctx context.Context, s q.Session[itemScope]) error {
						var err error
						found, err = probe.Bind(s).Exists(ctx, nil, binding)
						return err
					})
					results <- outcome{found, err}
				})
			}
			wg.Wait()
			close(results)
			for result := range results {
				require.NoError(t, result.err)
				require.True(t, result.found)
			}
			require.EqualValues(t, 2, adaptations.Load()-base)
			require.NoError(t, run(func(ctx context.Context, s q.Session[itemScope]) error {
				n, err := remove.Bind(s).DeleteMany(ctx, nil, binding)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				found, err := probe.Bind(s).Exists(ctx, nil, binding)
				require.NoError(t, err)
				require.False(t, found)
				return nil
			}))
			require.Equal(t, []int64{8, 4, 5}, original)
			require.EqualValues(t, 2, encodings.Load())
		})
	}
}
