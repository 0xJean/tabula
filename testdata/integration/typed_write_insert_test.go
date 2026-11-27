//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	q "github.com/0xJean/tabula"
	tplan "github.com/0xJean/tabula/plan"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/stretchr/testify/require"
)

type writeItem struct {
	ID    int64  `db:"id,key"`
	Rank  int32  `db:"rank"`
	Label string `db:"label"`
}

func writeRole(t *testing.T, db *sql.DB, schema string) string {
	t.Helper()
	role := fmt.Sprintf("scope_editor_%d", time.Now().UnixNano())
	_, err := db.ExecContext(t.Context(), "CREATE ROLE "+role+"; GRANT USAGE ON SCHEMA "+schema+" TO "+role)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec("DROP OWNED BY " + role + "; DROP ROLE " + role)
		require.NoError(t, err)
	})
	return role
}

func TestWriteInsertionDefaultsAndMinimumGrants(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx := t.Context()
			schema := typesSchema(t, db)
			role := writeRole(t, db, schema)
			exec := func(sql string) { t.Helper(); _, err := db.ExecContext(ctx, sql); require.NoError(t, err) }
			exec("CREATE TABLE " + schema + `.items (id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY, rank int NOT NULL DEFAULT 7, label text NOT NULL DEFAULT 'note');
CREATE TABLE ` + schema + `.jobs (id bigserial PRIMARY KEY, rank int NOT NULL DEFAULT 7, label text NOT NULL DEFAULT 'note');
GRANT INSERT(rank) ON ` + schema + ".items," + schema + ".jobs TO " + role)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			item := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global(), WriteMode: q.WriteInsertOnly})
			job := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".jobs", RelationScope: q.Global(), WriteMode: q.WriteInsertOnly})
			i, j := tplan.MapTable(item).As("i"), tplan.MapTable(job).As("j")
			value := tplan.Input("rank", tplan.Int4())
			insert := tplan.DefineWrite(i, "append", tplan.InsertInto(i).Values(tplan.Assign(tplan.FieldOf(i.Rel(), "rank", tplan.Int4()), value.Expr())).DoNothing())
			serial := tplan.DefineWrite(j, "append", tplan.InsertInto(j).Values(tplan.Assign(tplan.FieldOf(j.Rel(), "rank", tplan.Int4()), value.Expr())))
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			invoke := func(plan *tplan.WritePlan[struct{}], bindings ...tplan.Binding) error {
				return run(ctx, func(ctx context.Context, s session) error {
					n, err := plan.Bind(s).Exec(ctx, bindings...)
					if err == nil {
						require.EqualValues(t, 1, n)
					}
					return err
				})
			}
			require.NoError(t, invoke(insert, tplan.BindValue(value, int32(3))))
			require.Equal(t, "42501", q.SQLState(invoke(serial, tplan.BindValue(value, int32(3)))))
			exec("GRANT USAGE ON SEQUENCE " + schema + ".jobs_id_seq TO " + role)
			require.NoError(t, invoke(serial, tplan.BindValue(value, int32(4))))
			exec("ALTER TABLE " + schema + ".jobs ALTER COLUMN id SET DEFAULT 99; REVOKE USAGE ON SEQUENCE " + schema + ".jobs_id_seq FROM " + role)
			require.NoError(t, invoke(serial, tplan.BindValue(value, int32(5))))
			reset := tplan.DefineWrite(i, "default_rank", tplan.InsertInto(i).Values(tplan.DefaultOf(tplan.FieldOf(i.Rel(), "rank", tplan.Int4()))))
			require.NoError(t, invoke(reset))
			exec("GRANT INSERT ON " + schema + ".items TO " + role)
			defaults := tplan.DefineWrite(i, "defaults", tplan.InsertInto(i).DefaultValues())
			require.NoError(t, invoke(defaults))
			var ranks []int
			rows, err := db.QueryContext(ctx, "SELECT rank FROM "+schema+".items ORDER BY id")
			require.NoError(t, err)
			defer rows.Close()
			for rows.Next() {
				var rank int
				require.NoError(t, rows.Scan(&rank))
				ranks = append(ranks, rank)
			}
			require.NoError(t, rows.Err())
			require.Equal(t, []int{3, 7, 7}, ranks)
			for _, def := range reg.Defined() {
				for _, shape := range def.StatementShapes {
					for _, grant := range shape.Privileges {
						require.NotEqual(t, "SELECT", grant.Privilege)
					}
				}
			}
			exec("REVOKE INSERT ON " + schema + ".items FROM " + role + "; REVOKE INSERT(rank) ON " + schema + ".items FROM " + role)
			require.Equal(t, "42501", q.SQLState(invoke(insert, tplan.BindValue(value, int32(3)))))
		})
	}
}

func TestWriteBatchAtomicityAndReturningTriggers(t *testing.T) {
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			exec := func(sql string) { t.Helper(); _, err := db.ExecContext(ctx, sql); require.NoError(t, err) }
			exec("CREATE TABLE " + schema + `.items (id bigint PRIMARY KEY, rank int NOT NULL DEFAULT 1 CHECK(rank>0), label text NOT NULL DEFAULT 'note');
CREATE FUNCTION ` + schema + `.change_item() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.label='drop' THEN RETURN NULL; END IF; NEW.label=upper(NEW.label); RETURN NEW; END $$;
CREATE TRIGGER change_item BEFORE INSERT ON ` + schema + `.items FOR EACH ROW EXECUTE FUNCTION ` + schema + `.change_item()`)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			table := q.DefinePlanSource(reg, q.TableSpec[writeItem, int64]{Name: schema + ".items", RelationScope: q.Global()})
			src := tplan.MapTable(table).As("i")
			id, rank, label := tplan.FieldOf(src.Rel(), "id", tplan.Int8()), tplan.FieldOf(src.Rel(), "rank", tplan.Int4()), tplan.FieldOf(src.Rel(), "label", tplan.Text())
			key, value, text := tplan.Input("key", tplan.Int8()), tplan.Input("value", tplan.Int4()), tplan.Input("text", tplan.Text())
			write := tplan.InsertInto(src).Values(tplan.Assign(id, key.Expr()), tplan.Assign(rank, value.Expr()), tplan.Assign(label, text.Expr())).Batch(1, 2)
			plan := tplan.DefineWrite(src, "batch", write)
			returned := tplan.DefineTableReturning(src, "returned", write, tplan.SelectColumns(label.Column(), id.Column()))
			run := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			bindings := func(id int64, rank int32, label string) []tplan.Binding {
				return []tplan.Binding{tplan.BindValue(key, id), tplan.BindValue(value, rank), tplan.BindValue(text, label)}
			}
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				bound := plan.Bind(s)
				n, err := bound.ExecBatch(ctx, [][]tplan.Binding{bindings(1, 1, "first"), bindings(2, 1, "second"), bindings(3, -1, "third")})
				require.Equal(t, "23514", q.SQLState(err))
				require.Zero(t, n)
				// The caller may handle the error; the child savepoint already rolled
				// back every earlier chunk before this transaction can commit.
				n, err = bound.ExecBatch(ctx, nil)
				require.NoError(t, err)
				require.Zero(t, n)
				return nil
			}))
			var count int
			require.NoError(t, db.QueryRowContext(ctx, "SELECT count(*) FROM "+schema+".items").Scan(&count))
			require.Zero(t, count)
			require.NoError(t, run(ctx, func(ctx context.Context, s session) error {
				rows, err := returned.Bind(s).AllBatch(ctx, [][]tplan.Binding{bindings(1, 1, "first"), bindings(2, 1, "drop"), bindings(3, 1, "third")})
				require.NoError(t, err)
				require.ElementsMatch(t, []writeItem{{ID: 1, Label: "FIRST"}, {ID: 3, Label: "THIRD"}}, rows)
				return nil
			}))
			_, err := plan.Bind(effectDirect(t, driver, reg.SQLAuthority())).ExecBatch(ctx, [][]tplan.Binding{bindings(9, 1, "note")})
			require.ErrorIs(t, err, q.ErrReadOnly)
			ro := authorizedEffectRunner(t, driver, db, pool, "", reg.SQLAuthority(), q.StatementPolicy{}, nil, true)
			require.ErrorIs(t, ro(ctx, func(ctx context.Context, s session) error {
				_, err := returned.Bind(s).AllBatch(ctx, [][]tplan.Binding{bindings(9, 1, "note")})
				return err
			}), q.ErrReadOnly)
		})
	}
}

func TestWriteNarrowSourceDefaults(t *testing.T) {
	type item struct {
		Rank int32 `db:"rank,key"`
	}
	db, pool := databases(t)
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			ctx, schema := t.Context(), typesSchema(t, db)
			role := writeRole(t, db, schema)
			exec := func(query string) { t.Helper(); _, err := db.ExecContext(ctx, query); require.NoError(t, err) }
			exec("CREATE TABLE " + schema + `.items(id bigserial UNIQUE, rank int PRIMARY KEY);
CREATE TABLE ` + schema + `.jobs(id bigint GENERATED ALWAYS AS IDENTITY UNIQUE, rank int PRIMARY KEY);
GRANT INSERT(rank) ON ` + schema + `.items,` + schema + `.jobs TO ` + role)
			reg := q.NewRegistry(q.RegistryConfig[struct{}]{Deny: q.DenyLegacySQL})
			value := tplan.Input("rank", tplan.Int4())
			plans := make(map[string]*tplan.WritePlan[struct{}])
			defaultsByTable := make(map[string][]q.DefaultRequirement)
			for _, name := range []string{"items", "jobs"} {
				table := q.DefinePlanSource(reg, q.TableSpec[item, int32]{Name: schema + "." + name, RelationScope: q.Global(), WriteMode: q.WriteInsertOnly})
				src := tplan.MapTable(table).As("i")
				plans[name] = tplan.DefineWrite(src, "append", tplan.InsertInto(src).Values(tplan.Assign(tplan.FieldOf(src.Rel(), "rank", tplan.Int4()), value.Expr())))
				defaults := table.Definition().StatementShapes["append.global.write"].Defaults
				defaultsByTable[name] = defaults
				require.Equal(t, []q.DefaultRequirement{{Relation: `"` + schema + `"."` + name + `"`, IncludesOmitted: true, InsertColumns: []string{"rank"}}}, defaults)
			}
			run := authorizedEffectRunner(t, driver, db, pool, role, reg.SQLAuthority(), q.StatementPolicy{}, nil, false)
			var next int32
			invoke := func(name string) error {
				next++
				return run(ctx, func(ctx context.Context, s session) error {
					_, err := plans[name].Bind(s).Exec(ctx, tplan.BindValue(value, next))
					return err
				})
			}
			checkDefaults := func(name string) []sqlcheck.MissingRequirement {
				t.Helper()
				missing, err := sqlcheck.CheckReadiness(ctx, db, role, sqlcheck.ReadinessNeeds{Defaults: defaultsByTable[name]})
				require.NoError(t, err)
				return missing
			}
			require.Equal(t, []sqlcheck.MissingRequirement{{Kind: "SEQUENCE", Object: defaultsByTable["items"][0].Relation, Column: "id", Privilege: "USAGE"}}, checkDefaults("items"))
			require.Equal(t, "42501", q.SQLState(invoke("items")))
			require.Empty(t, checkDefaults("jobs"))
			require.NoError(t, invoke("jobs"))
			exec("GRANT USAGE ON SEQUENCE " + schema + ".items_id_seq TO " + role)
			require.Empty(t, checkDefaults("items"))
			require.NoError(t, invoke("items"))
			exec("ALTER TABLE " + schema + ".items ALTER COLUMN id SET DEFAULT 99; REVOKE USAGE ON SEQUENCE " + schema + ".items_id_seq FROM " + role)
			require.Empty(t, checkDefaults("items"))
			require.NoError(t, invoke("items"))
		})
	}
}
