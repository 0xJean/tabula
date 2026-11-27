//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/0xJean/tabula/pgxdriver"
	"github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

func TestSignedPublicWriteGraphs(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint PRIMARY KEY, rank integer NOT NULL);
CREATE TABLE `+schema+`.notes(id bigint PRIMARY KEY, rank integer NOT NULL);
INSERT INTO `+schema+`.items VALUES(1,1)`)
			require.NoError(t, err)
			type item struct {
				ID   int64 `db:"id,key"`
				Rank int32 `db:"rank"`
			}
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			items := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".items", RelationScope: q.Global()})
			notes := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".notes", RelationScope: q.Global(), WriteMode: q.WriteInsertOnly})
			i, n := plan.MapTable(items).As("i"), plan.MapTable(notes).As("n")
			id, rank := plan.FieldOf(i.Rel(), "id", plan.Int8()), plan.FieldOf(i.Rel(), "rank", plan.Int4())
			noteID, noteRank := plan.FieldOf(n.Rel(), "id", plan.Int8()), plan.FieldOf(n.Rel(), "rank", plan.Int4())
			key, value := plan.Input("key", plan.Int8()), plan.Input("rank", plan.Int4())
			out := q.DefineView(reg, q.ViewSpec[lockResult]{Name: "entries"})
			effect := plan.EffectCTE("added", plan.InsertInto(n).Values(plan.Assign(noteID, key.Expr()), plan.Assign(noteRank, value.Expr())))
			readAfterWrite := plan.DefineReturning(out, "append_and_read", plan.Select(plan.Project(plan.Output(out, "id", plan.Int8()), id.Expr())).From(i).With(effect))
			upsert := plan.DefineWrite(i, "upsert", plan.InsertInto(i).Values(plan.Assign(id, key.Expr()), plan.Assign(rank, value.Expr())).
				OnConflict(plan.ConflictColumns(id.Column())).DoUpdate(plan.Assign(rank, value.Expr())))
			bindings := []plan.Binding{plan.BindValue(key, int64(1)), plan.BindValue(value, int32(7))}
			policy := q.StatementPolicy{Read: []string{"select"}, Write: []string{"with", "insert"}, RequireReturning: true}
			invoke := func(policy q.StatementPolicy, readOnly bool, body func(context.Context, session) error) error {
				t.Helper()
				return authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), policy, nil, readOnly)(ctx, body)
			}
			require.NoError(t, invoke(policy, false, func(ctx context.Context, s session) error {
				_, err := readAfterWrite.Bind(s).All(ctx, bindings...)
				require.ErrorIs(t, err, q.ErrForbiddenStatement)
				return nil
			}))
			policy.AllowAuthorizedWriteCTE = true
			require.NoError(t, invoke(policy, false, func(ctx context.Context, s session) error {
				rows, err := readAfterWrite.Bind(s).All(ctx, bindings...)
				require.NoError(t, err)
				require.Equal(t, []lockResult{{1}}, rows)
				// INSERT permission in the command policy cannot authorize the
				// separately signed UPDATE effect of ON CONFLICT DO UPDATE.
				_, err = upsert.Bind(s).Exec(ctx, bindings...)
				require.ErrorIs(t, err, q.ErrForbiddenStatement)
				var got int64
				raw := out.Definition().Statements["append_and_read.global.write"]
				require.ErrorIs(t, s.Returning(ctx, raw, []any{int64(2), int32(8)}, &got), q.ErrLegacySQL)
				return nil
			}))
			policy.Write = []string{"with", "insert", "update"}
			require.NoError(t, invoke(policy, false, func(ctx context.Context, s session) error {
				n, err := upsert.Bind(s).Exec(ctx, bindings...)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				return nil
			}))
			require.NoError(t, invoke(policy, true, func(ctx context.Context, s session) error {
				_, err := readAfterWrite.Bind(s).All(ctx, bindings...)
				require.ErrorIs(t, err, q.ErrReadOnly)
				return nil
			}))
			_, err = readAfterWrite.Bind(effectDirect(t, driver, reg.SQLAuthority())).All(ctx, bindings...)
			require.ErrorIs(t, err, q.ErrReadOnly)
			blocked := errors.New("guard rejected statement")
			guarded := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), policy, func(string) error { return blocked }, false)
			err = guarded(ctx, func(ctx context.Context, s session) error {
				_, err := readAfterWrite.Bind(s).All(ctx, bindings...)
				return err
			})
			require.ErrorIs(t, err, blocked)
			defs := append(reg.Defined(), sqlcheck.ViewsToDefinitions(reg.DefinedViews())...)
			findings, err := sqlcheck.VerifyStatementsUsing(ctx, pgxdriver.SQLVerifier(pool), defs, "")
			require.NoError(t, err)
			require.Empty(t, findings)
			var count, current int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+schema+".notes").Scan(&count))
			require.Equal(t, 1, count)
			require.NoError(t, db.QueryRowContext(ctx, "SELECT rank FROM "+schema+".items WHERE id=1").Scan(&current))
			require.Equal(t, 7, current)
		})
	}
}
