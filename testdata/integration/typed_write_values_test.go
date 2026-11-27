//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/pqdriver"
	"github.com/stretchr/testify/require"
)

func TestWriteBatchCanonicalValues(t *testing.T) {
	type item struct {
		ID      int64            `db:"id,key"`
		Members []int32          `db:"members,array"`
		Doc     *json.RawMessage `db:"doc,json"`
		UID     *string          `db:"uid"`
		Label   *string          `db:"label"`
	}
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+".items(id bigint PRIMARY KEY, members int[], doc jsonb, uid uuid, label text)")
			require.NoError(t, err)
			cfg := q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL, ArrayColumn: pgxdriver.ArrayColumn}
			if driver == "pq" {
				cfg.ArrayColumn, cfg.ArrayArg = pqdriver.ArrayColumn, pqdriver.Array
			}
			reg := q.NewRegistry(cfg)
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".items", RelationScope: q.Global()})
			layout := tplan.MapTable(table, tplan.ColumnType("members", pg.ArrayOf(pg.Int4())), tplan.ColumnType("doc", pg.JSONB[json.RawMessage]()), tplan.ColumnType("uid", pg.UUID[string]()))
			src := layout.As("i")
			id := tplan.MappedColumn[int64](layout, "id").On(src.Rel())
			members := tplan.MappedColumn[[]int32](layout, "members").On(src.Rel())
			doc := tplan.MappedColumn[json.RawMessage](layout, "doc").On(src.Rel())
			uid, label := tplan.MappedColumn[string](layout, "uid").On(src.Rel()), tplan.MappedColumn[string](layout, "label").On(src.Rel())
			encoded := 0
			arrayType := pg.ArrayOf(pg.Int4()).WithEncoder(func(value []int32) (any, error) { encoded++; return value, nil })
			key, values, document := tplan.Input("id", pg.Int8()), tplan.Input("members", arrayType), tplan.Input("doc", pg.JSONB[json.RawMessage]())
			nullableText := func(value string) (any, error) {
				if value == "" {
					return nil, nil
				}
				return value, nil
			}
			identifier, text := tplan.Input("uid", pg.UUID[string]().WithEncoder(nullableText)), tplan.Input("label", pg.Text().WithEncoder(nullableText))
			plan := tplan.DefineTableReturning(src, "batch", tplan.InsertInto(src).Values(tplan.Assign(id, key.Expr()), tplan.Assign(members, values.Expr()),
				tplan.Assign(doc, document.Expr()), tplan.Assign(uid, identifier.Expr()), tplan.Assign(label, text.Expr())).Batch(1, 2),
				tplan.SelectColumns(id.Column(), members.Column(), doc.Column(), uid.Column(), label.Column()))
			raw, rawJSON := []int32{2, 3}, json.RawMessage(`{"x":1}`)
			arrayBinding, jsonBinding := tplan.BindValue(values, raw), tplan.BindValue(document, rawJSON)
			raw[0], rawJSON[5] = 99, '9'
			uuid := "00000000-0000-4000-8000-000000000001"
			var inputs [][]tplan.Binding
			for i := int64(1); i <= 3; i++ {
				inputs = append(inputs, []tplan.Binding{tplan.BindValue(key, i), arrayBinding, jsonBinding, tplan.BindValue(identifier, uuid), tplan.BindValue(text, "")})
			}
			inputs = append(inputs, []tplan.Binding{tplan.BindValue(key, int64(4)), tplan.BindValue(values, []int32{}), tplan.BindValue(document, nil), tplan.BindValue(identifier, ""), tplan.BindValue(text, "note")})
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				// Reuse the exact canonical bindings in a two-row form and a
				// one-row remainder, then in a later operation.
				rows, err := plan.Bind(s).AllBatch(ctx, inputs[:3])
				require.NoError(t, err)
				require.Len(t, rows, 3)
				for _, row := range rows {
					require.Equal(t, []int32{2, 3}, row.Members)
					require.NotNil(t, row.Doc)
					require.JSONEq(t, `{"x":1}`, string(*row.Doc))
					require.Equal(t, new(uuid), row.UID)
					require.Nil(t, row.Label)
				}
				rows, err = plan.Bind(s).AllBatch(ctx, inputs[3:])
				require.NoError(t, err)
				require.Len(t, rows, 1)
				require.Empty(t, rows[0].Members)
				require.NotNil(t, rows[0].Members)
				require.Nil(t, rows[0].Doc)
				require.Nil(t, rows[0].UID)
				require.Equal(t, new("note"), rows[0].Label)
				return nil
			}))
			require.Equal(t, 2, encoded)
		})
	}
}

func TestWriteGeneratedColumns(t *testing.T) {
	type item struct {
		ID    int64  `db:"id,key"`
		Rank  int32  `db:"rank"`
		Label string `db:"label"`
	}
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			_, err := db.ExecContext(ctx, "CREATE TABLE "+schema+`.items(id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, rank int NOT NULL DEFAULT 3, label text GENERATED ALWAYS AS (rank::text) STORED)`)
			require.NoError(t, err)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[item, int64]{Name: schema + ".items", RelationScope: q.Global()})
			src := tplan.MapTable(table).As("i")
			id, rank, label := tplan.FieldOf(src.Rel(), "id", tplan.Int8()), tplan.FieldOf(src.Rel(), "rank", tplan.Int4()), tplan.FieldOf(src.Rel(), "label", tplan.Text())
			key, value, text := tplan.Input("id", tplan.Int8()), tplan.Input("rank", tplan.Int4()), tplan.Input("label", tplan.Text())
			selection := tplan.SelectColumns(id.Column(), label.Column())
			defaults := tplan.DefineTableReturning(src, "defaults", tplan.InsertInto(src).DefaultValues(), selection)
			explicit := tplan.DefineTableReturning(src, "explicit_default", tplan.InsertInto(src).Values(tplan.Assign(rank, value.Expr()), tplan.DefaultOf(label)), selection)
			badIdentity := tplan.DefineWrite(src, "explicit_identity", tplan.InsertInto(src).Values(tplan.Assign(id, key.Expr())))
			badGenerated := tplan.DefineWrite(src, "explicit_generated", tplan.InsertInto(src).Values(tplan.Assign(label, text.Expr())))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := defaults.Bind(s).All(ctx)
				require.NoError(t, err)
				require.Equal(t, []item{{ID: 1, Label: "3"}}, rows)
				rows, err = explicit.Bind(s).All(ctx, tplan.BindValue(value, int32(8)))
				require.NoError(t, err)
				require.Equal(t, []item{{ID: 2, Label: "8"}}, rows)
				return nil
			}))
			err = run(ctx, func(ctx context.Context, s session) error {
				_, err := badIdentity.Bind(s).Exec(ctx, tplan.BindValue(key, int64(9)))
				return err
			})
			require.Equal(t, "428C9", q.SQLState(err))
			err = run(ctx, func(ctx context.Context, s session) error {
				_, err := badGenerated.Bind(s).Exec(ctx, tplan.BindValue(text, "note"))
				return err
			})
			require.Equal(t, "428C9", q.SQLState(err))
		})
	}
}
