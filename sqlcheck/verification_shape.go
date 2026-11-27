package sqlcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/0xJean/tabula"
)

func checkVerificationShape(ctx context.Context, tx VerificationTx, name, query string, params int, s *tabula.StatementShape) error {
	if len(s.Inputs) != params {
		return fmt.Errorf("prepared statement has %d inputs, expected %d", params, len(s.Inputs))
	}
	for i, in := range s.Inputs {
		var matches bool
		err := tx.Scan(ctx, `SELECT parameter_types[$2] IS NOT DISTINCT FROM pg_catalog.to_regtype($3)
FROM pg_catalog.pg_prepared_statements WHERE name = $1`, []any{name, i + 1, in.Type}, &matches)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("prepared input %q at $%d differs from %s", in.Name, i+1, in.Type)
		}
	}
	for _, col := range s.PhysicalColumns {
		var matches bool
		err := tx.Scan(ctx, `SELECT EXISTS (SELECT FROM pg_catalog.pg_attribute AS a
WHERE attrelid = pg_catalog.to_regclass($1) AND attname = $2 AND attnum > 0 AND NOT attisdropped
 AND a.atttypid = pg_catalog.to_regtype($3) AND ($4::integer IS NULL OR a.atttypmod = $4))`, []any{col.Relation, col.Name, col.Type, col.TypeMod}, &matches)
		if err != nil {
			return err
		}
		if !matches {
			if col.TypeMod != nil {
				return fmt.Errorf("physical column %s.%s differs from %s with type modifier %d", col.Relation, col.Name, col.Type, *col.TypeMod)
			}
			return fmt.Errorf("physical column %s.%s differs from %s", col.Relation, col.Name, col.Type)
		}
	}
	if err := checkVerificationProof(ctx, tx, s); err != nil {
		return err
	}
	if len(s.Result) == 0 {
		return nil
	}
	description, err := tx.Describe(ctx, name+"_d", query)
	if errors.Is(err, ErrDescribeRequired) && !descriptionNeedsProtocol(query, s) {
		if old, ok := tx.(sqlVerificationTx); ok {
			if s.Effects&tabula.EffectRowLock != 0 {
				query, err = lockProjectionQuery(query)
				if err != nil {
					return err
				}
			}
			return checkReadResult(ctx, old.tx, query, params, s.Result)
		}
	}
	if err != nil {
		return err
	}
	return checkDescription(ctx, tx, query, params, s, description)
}

func checkVerificationProof(ctx context.Context, tx VerificationTx, s *tabula.StatementShape) error {
	if len(s.Keys)+len(s.Comparisons)+len(s.UniqueMatches)+len(s.Defaults)+len(s.Conflicts) == 0 {
		return nil
	}
	var role string
	if err := tx.Scan(ctx, "SELECT CURRENT_USER", nil, &role); err != nil {
		return err
	}
	needs := ReadinessNeeds{Keys: s.Keys, Comparisons: s.Comparisons, UniqueMatches: s.UniqueMatches, Defaults: s.Defaults, Conflicts: s.Conflicts}
	items, err := readinessItems(needs)
	if err != nil {
		return err
	}
	input, err := json.Marshal(items)
	if err != nil {
		return err
	}
	var missing []MissingRequirement
	err = tx.Each(ctx, readinessSQL, []any{role, string(input)}, func(scan func(...any) error) error {
		var need MissingRequirement
		if err := scan(&need.Kind, &need.Object, &need.Column, &need.Privilege); err != nil {
			return err
		}
		missing = append(missing, need)
		return nil
	})
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("sqlcheck: shape requirements: %v", missing)
	}
	return nil
}
