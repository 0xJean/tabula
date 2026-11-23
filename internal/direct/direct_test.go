package direct

import (
	"errors"
	"testing"

	"github.com/0xJean/tabula"
)

func TestReadPolicy(t *testing.T) {
	denied := errors.New("policy")
	guard := Guard(func(q string) error {
		if q == "SELECT private_value" {
			return denied
		}
		return nil
	})
	for _, q := range []string{"/* app */ SELECT 1;", "SELECT 'a;b'", "SELECT $body$x;y$body$", `SELECT E'a\'b;c'`, "WITH v AS (SELECT 1) SELECT * FROM v"} {
		if err := guard(q); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	for _, q := range []string{"", "SELECT 1; SELECT 2", `SELECT E'\''; SET default_transaction_read_only = off`, "UPDATE job SET id = 1"} {
		if err := guard(q); !errors.Is(err, tabula.ErrReadOnly) {
			t.Errorf("%q: got %v", q, err)
		}
	}
	if err := guard("SELECT private_value"); !errors.Is(err, denied) {
		t.Fatal(err)
	}
}
