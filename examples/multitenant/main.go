// Command multitenant demonstrates descriptor checks and tenant-isolated reads.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
	"github.com/0xJean/tabula/stdsql"
	_ "github.com/lib/pq"
)

type tenant string
type note struct {
	ID     int64  `db:"id,key,serial"`
	Tenant tenant `db:"tenant_id,scope"`
	Body   string `db:"body"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	port := os.Getenv("TABULA_DEMO_PORT")
	if port == "" {
		port = "55432"
	}
	db, err := sql.Open("postgres", "postgres://tabula_reader_writer:local_demo_app@127.0.0.1:"+port+"/tabula_demo?sslmode=disable")
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	reg := tabula.NewRegistry(tabula.RegistryConfig[tenant]{})
	notes := tabula.Define(reg, tabula.TableSpec[note, int64]{Name: "public.notes", Order: "id"})
	type visibleNote struct {
		Tenant tenant `db:"tenant_id"`
		Body   string `db:"body"`
	}
	v := tabula.DefineView(reg, tabula.ViewSpec[visibleNote]{Name: "visible_notes"})
	query := v.Statement("list", "SELECT "+v.Cols()+" FROM public.notes ORDER BY id")
	seed := notes.DefineUpsert("seed", tabula.UpsertSpec{Conflict: []string{"tenant_id", "body"}})
	families := sqlcheck.DefaultTypeFamilies()
	families["main.tenant"] = families["string"]
	if err := check(sqlcheck.VerifySchema(ctx, db, reg.Defined(), families)); err != nil {
		return err
	}
	if err := check(sqlcheck.VerifyStatements(ctx, db, reg.Defined(), "")); err != nil {
		return err
	}
	if err := check(sqlcheck.VerifyViewStatements(ctx, db, reg.DefinedViews(), "")); err != nil {
		return err
	}
	if err := check(sqlcheck.CheckRLS(ctx, db, sqlcheck.RLSExpectation{Tables: []string{"notes"}, RequireForce: true})); err != nil {
		return err
	}
	fmt.Println("Schema, statements and RLS verified")
	runner := stdsql.NewRunner(db, stdsql.ScopePolicy[tenant]{
		SetupTx: func(ctx context.Context, tx *sql.Tx, s tenant) error {
			return stdsql.SetLocalConfig(ctx, tx, "demo.tenant", string(s))
		},
		GuardQuery: tabula.GuardNoSessionEscape,
	})
	// Seed both tenants before checking what each can read.
	for _, scope := range []tenant{"north", "south"} {
		err := runner.Within(ctx, scope, func(ctx context.Context, s *stdsql.Session[tenant]) error {
			_, err := notes.Bind(s).Upsert(ctx, seed, &note{Tenant: scope, Body: "Hello from " + string(scope)})
			return err
		})
		if err != nil {
			return err
		}
	}
	for _, scope := range []tenant{"north", "south"} {
		err := runner.WithinReadOnly(ctx, scope, func(ctx context.Context, s *stdsql.Session[tenant]) error {
			var count int
			for n, err := range v.Bind(s).Iter(ctx, query) {
				if err != nil {
					return err
				}
				if n.Tenant != scope {
					return fmt.Errorf("RLS returned tenant %q for %q", n.Tenant, scope)
				}
				count++
				fmt.Printf("%s sees %q\n", scope, n.Body)
			}
			if count == 0 {
				return fmt.Errorf("tenant %q has no visible note", scope)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func check(failures []string, err error) error {
	if err != nil {
		return err
	}
	if len(failures) > 0 {
		return fmt.Errorf("sqlcheck: %v", failures)
	}
	return nil
}
