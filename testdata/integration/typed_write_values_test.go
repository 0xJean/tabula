//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"testing"

	q "github.com/0xJean/tabula"
	pg "github.com/0xJean/tabula/pgtype"
	"github.com/0xJean/tabula/pgxdriver"
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
			layout := q.MapTable(table, q.ColumnType("members", pg.ArrayOf(pg.Int4())), q.ColumnType("doc", pg.JSONB[json.RawMessage]()), q.ColumnType("uid", pg.UUID[string]()))
			src := layout.As("i")
			id := q.MappedColumn[int64](layout, "id").On(src.Rel())
			members := q.MappedColumn[[]int32](layout, "members").On(src.Rel())
			doc := q.MappedColumn[json.RawMessage](layout, "doc").On(src.Rel())
			uid, label := q.MappedColumn[string](layout, "uid").On(src.Rel()), q.MappedColumn[string](layout, "label").On(src.Rel())
			encoded := 0
			arrayType := pg.ArrayOf(pg.Int4()).WithEncoder(func(value []int32) (any, error) { encoded++; return value, nil })
			key, values, document := q.Input("id", pg.Int8()), q.Input("members", arrayType), q.Input("doc", pg.JSONB[json.RawMessage]())
			nullableText := func(value string) (any, error) {
				if value == "" {
					return nil, nil
				}
				return value, nil
			}
			identifier, text := q.Input("uid", pg.UUID[string]().WithEncoder(nullableText)), q.Input("label", pg.Text().WithEncoder(nullableText))
			plan := q.DefineTableReturning(src, "batch", q.InsertInto(src).Values(q.Assign(id, key.Expr()), q.Assign(members, values.Expr()),
				q.Assign(doc, document.Expr()), q.Assign(uid, identifier.Expr()), q.Assign(label, text.Expr())).Batch(1, 2),
				q.SelectColumns(id.Column(), members.Column(), doc.Column(), uid.Column(), label.Column()))
			raw, rawJSON := []int32{2, 3}, json.RawMessage(`{"x":1}`)
			arrayBinding, jsonBinding := q.BindValue(values, raw), q.BindValue(document, rawJSON)
			raw[0], rawJSON[5] = 99, '9'
			uuid := "00000000-0000-4000-8000-000000000001"
			var inputs [][]q.Binding
			for i := int64(1); i <= 3; i++ {
				inputs = append(inputs, []q.Binding{q.BindValue(key, i), arrayBinding, jsonBinding, q.BindValue(identifier, uuid), q.BindValue(text, "")})
			}
			inputs = append(inputs, []q.Binding{q.BindValue(key, int64(4)), q.BindValue(values, []int32{}), q.BindValue(document, nil), q.BindValue(identifier, ""), q.BindValue(text, "note")})
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
			src := q.MapTable(table).As("i")
			id, rank, label := q.FieldOf(src.Rel(), "id", q.Int8()), q.FieldOf(src.Rel(), "rank", q.Int4()), q.FieldOf(src.Rel(), "label", q.Text())
			key, value, text := q.Input("id", q.Int8()), q.Input("rank", q.Int4()), q.Input("label", q.Text())
			selection := q.SelectColumns(id.Column(), label.Column())
			defaults := q.DefineTableReturning(src, "defaults", q.InsertInto(src).DefaultValues(), selection)
			explicit := q.DefineTableReturning(src, "explicit_default", q.InsertInto(src).Values(q.Assign(rank, value.Expr()), q.DefaultOf(label)), selection)
			badIdentity := q.DefineWrite(src, "explicit_identity", q.InsertInto(src).Values(q.Assign(id, key.Expr())))
			badGenerated := q.DefineWrite(src, "explicit_generated", q.InsertInto(src).Values(q.Assign(label, text.Expr())))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := defaults.Bind(s).All(ctx)
				require.NoError(t, err)
				require.Equal(t, []item{{ID: 1, Label: "3"}}, rows)
				rows, err = explicit.Bind(s).All(ctx, q.BindValue(value, int32(8)))
				require.NoError(t, err)
				require.Equal(t, []item{{ID: 2, Label: "8"}}, rows)
				return nil
			}))
			err = run(ctx, func(ctx context.Context, s session) error {
				_, err := badIdentity.Bind(s).Exec(ctx, q.BindValue(key, int64(9)))
				return err
			})
			require.Equal(t, "428C9", q.SQLState(err))
			err = run(ctx, func(ctx context.Context, s session) error {
				_, err := badGenerated.Bind(s).Exec(ctx, q.BindValue(text, "note"))
				return err
			})
			require.Equal(t, "428C9", q.SQLState(err))
		})
	}
}
