//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	q "github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

func TestPostgreSQLConstraintNames(t *testing.T) {
	db, pool := databases(t)
	schema := typesSchema(t, db)
	for _, query := range []string{
		"CREATE TABLE " + schema + `.items (id int CONSTRAINT "Item ""Key" PRIMARY KEY, rank int CONSTRAINT "PositiveRank" CHECK (rank > 0))`,
		"CREATE TABLE " + schema + `.notes (id int CONSTRAINT "ItemRef" REFERENCES ` + schema + ".items(id))",
		"CREATE TABLE " + schema + `.entries (id int CONSTRAINT "DeferredRef" REFERENCES ` + schema + ".items(id) DEFERRABLE INITIALLY DEFERRED)",
		"INSERT INTO " + schema + ".items VALUES (1, 1)",
	} {
		_, err := db.ExecContext(t.Context(), query)
		require.NoError(t, err)
	}
	for _, driver := range []string{"pq", "pgx"} {
		t.Run(driver, func(t *testing.T) {
			run := effectSQLRunner(t, driver, db, pool, "")
			var prior error
			for _, tc := range []struct {
				query, state, name string
				class              q.ErrorClass
				cause              error
				commit             bool
			}{
				{"INSERT INTO " + schema + ".items VALUES (1, 2)", "23505", `Item "Key`, q.ClassUnique, q.ErrUnique, false},
				{"INSERT INTO " + schema + ".notes VALUES (2)", "23503", "ItemRef", q.ClassForeignKey, q.ErrForeignKey, false},
				{"INSERT INTO " + schema + ".items VALUES (2, 0)", "23514", "PositiveRank", q.ClassCheck, q.ErrCheck, false},
				{"SELECT 1/0", "22012", "", q.ClassOther, nil, false},
				{"INSERT INTO " + schema + ".entries VALUES (2)", "23503", "DeferredRef", q.ClassForeignKey, q.ErrForeignKey, true},
			} {
				err := run(t.Context(), func(ctx context.Context, s session) error {
					_, err := s.Exec(ctx, tc.query)
					if tc.commit {
						require.NoError(t, err, "deferred check must succeed during the insert")
					}
					return err
				})
				require.Error(t, err)
				require.Equal(t, tc.commit, errors.Is(err, q.ErrCommit))
				require.Equal(t, tc.state, q.SQLState(err))
				require.Equal(t, tc.name, q.ConstraintName(err))
				require.Equal(t, tc.name, q.ConstraintName(fmt.Errorf("operation: %w", err)))
				class, cause := q.ClassifyError(t.Context(), err)
				require.Equal(t, tc.class, class)
				require.Equal(t, tc.cause, cause)
				if prior != nil {
					require.Equal(t, q.ConstraintName(prior), q.ConstraintName(errors.Join(prior, err)))
				}
				prior = err
			}
		})
	}
}
