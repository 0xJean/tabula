package tabula_test

import (
	"context"
	"fmt"
	"time"

	"github.com/0xJean/tabula"
)

func ExampleDefine() {
	type item struct {
		ID        int64      `db:"id,key,serial"`
		Code      string     `db:"code"`
		CreatedBy string     `db:"created_by,once"`
		Version   int        `db:"version,version"`
		DeletedAt *time.Time `db:"deleted_at,deleted"`
	}

	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	tbl := tabula.Define(reg, tabula.TableSpec[item, int64]{
		Name:  "public.items",
		Order: "id ASC",
	})

	fmt.Println(tbl.Cols())
	fmt.Println(tbl.Definition().Statements["get"])
	// Output:
	// id, code, created_by, version, deleted_at
	// SELECT id, code, created_by, version, deleted_at FROM public.items WHERE id = $1 AND deleted_at IS NULL
}

func ExampleTable_DefineUpsert() {
	type setting struct {
		Key   string `db:"setting_key,key"`
		Value string `db:"setting_value"`
	}

	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	tbl := tabula.Define(reg, tabula.TableSpec[setting, string]{Name: "public.settings"})
	plan := tbl.DefineUpsert("upsert_setting", tabula.UpsertSpec{
		Conflict: []string{"setting_key"},
		Update:   []string{"setting_value"},
	})

	fmt.Println(plan.SQL())
	// Output:
	// INSERT INTO public.settings (setting_key, setting_value) VALUES ($1, $2) ON CONFLICT (setting_key) DO UPDATE SET setting_value = EXCLUDED.setting_value
}

func ExampleDefineView() {
	type orderPart struct {
		ID   int64  `db:"id"`
		Code string `db:"code"`
	}
	type customerPart struct {
		Name string `db:"name,zero"`
	}
	type summary struct {
		Order    orderPart    `db:"o_"`
		Customer customerPart `db:"c_"`
	}

	reg := tabula.NewRegistry(tabula.RegistryConfig[struct{}]{})
	view := tabula.DefineView(reg, tabula.ViewSpec[summary]{Name: "public.order_summary"})

	fmt.Println(view.SelectAs(map[string]string{"o_": "o", "c_": "c"}))
	// Output:
	// o.id AS o_id, o.code AS o_code, c.name AS c_name
}

func ExampleFilter() {
	f := tabula.NewFilter("tenant-1")
	f.AndIf(true, "status = ?", "active")
	f.In("currency", "USD", "EUR")
	f.And("meta ?? ?", "verified")

	fmt.Println(f.Where())
	fmt.Println(f.Args())
	// Output:
	//  AND (status = $2) AND (currency IN ($3, $4)) AND (meta ? $5)
	// [tenant-1 active USD EUR verified]
}

func ExampleMustOrderSpec() {
	spec := tabula.MustOrderSpec(tabula.OrderSpec{
		Columns:          map[string]string{"createdAt": "created_at", "code": "code"},
		DefaultColumn:    "created_at",
		DefaultDirection: "DESC",
		TieBreaker:       "id ASC",
	})

	fmt.Println(spec.Resolve("code", "asc"))
	fmt.Println(spec.Resolve("id; DROP TABLE users--", "ASC; --"))
	// Output:
	// code ASC, id ASC
	// created_at DESC, id ASC
}

func ExampleRetry() {
	attempts := 0
	err := tabula.Retry(context.Background(), tabula.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   time.Millisecond,
		Sleep:       func(context.Context, time.Duration) error { return nil },
	}, func(context.Context) error {
		attempts++
		if attempts == 1 {
			return tabula.ErrSerialization
		}
		return nil
	})
	fmt.Println(attempts, err)
	// Output:
	// 2 <nil>
}
