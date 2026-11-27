package sqlcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/0xJean/tabula"
)

type conflictPlan struct {
	Schema     string   `json:"Schema"`
	Relation   string   `json:"Relation Name"`
	Resolution string   `json:"Conflict Resolution"`
	Arbiters   []string `json:"Conflict Arbiter Indexes"`
}

// Check the actual generic plan's arbiters, not an index that only resembles
// the declaration. EXPLAIN without ANALYZE does not execute a modifying CTE.
func checkWritePlan(ctx context.Context, tx VerificationTx, shape *tabula.StatementShape, data json.RawMessage) error {
	if len(shape.Conflicts) == 0 {
		return nil
	}
	plans, err := conflictPlans(data)
	if err != nil {
		return err
	}
	for _, need := range shape.Conflicts {
		target, err := resolveReadinessItem(MissingRequirement{Kind: "TABLE", Object: need.Relation})
		if err != nil {
			return err
		}
		resolution := "NOTHING"
		if need.DoUpdate {
			resolution = "UPDATE"
		}
		found := false
		for _, plan := range plans {
			if plan.Schema != target.Schema || plan.Relation != target.Name {
				continue
			}
			if found || plan.Resolution != resolution {
				return fmt.Errorf("conflict plan for %s does not match its declaration", need.Relation)
			}
			found = true
			if (need.Constraint != "" || len(need.Columns) > 0) && len(plan.Arbiters) == 0 {
				return fmt.Errorf("conflict plan for %s has no inferred arbiter", need.Relation)
			}
			columns, err := json.Marshal(need.Columns)
			if err != nil {
				return err
			}
			for _, arbiter := range plan.Arbiters {
				var matches bool
				err := tx.Scan(ctx, conflictArbiterSQL, []any{need.Relation, arbiter, need.Constraint, need.DoUpdate, string(columns)}, &matches)
				if err != nil {
					return err
				}
				if !matches {
					return fmt.Errorf("conflict arbiter %s for %s is not an eligible immediate index", arbiter, need.Relation)
				}
			}
		}
		if !found {
			return fmt.Errorf("conflict plan for %s is missing", need.Relation)
		}
	}
	return nil
}

func conflictPlans(data json.RawMessage) ([]conflictPlan, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("invalid EXPLAIN JSON: %w", err)
	}
	var out []conflictPlan
	var visit func(any) error
	visit = func(value any) error {
		switch value := value.(type) {
		case map[string]any:
			if _, ok := value["Conflict Resolution"]; ok {
				encoded, err := json.Marshal(value)
				if err != nil {
					return err
				}
				var plan conflictPlan
				if err := json.Unmarshal(encoded, &plan); err != nil {
					return err
				}
				if plan.Schema == "" || plan.Relation == "" || !strings.EqualFold(plan.Resolution, "nothing") && !strings.EqualFold(plan.Resolution, "update") {
					return fmt.Errorf("incomplete conflict plan description")
				}
				plan.Resolution = strings.ToUpper(plan.Resolution)
				out = append(out, plan)
			}
			for _, child := range value {
				if err := visit(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range value {
				if err := visit(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := visit(root)
	return out, err
}

const conflictArbiterSQL = `SELECT EXISTS (
 SELECT FROM pg_catalog.pg_index ix
 JOIN pg_catalog.pg_class target ON target.oid = ix.indrelid
 JOIN pg_catalog.pg_class idx ON idx.oid = ix.indexrelid AND idx.relnamespace = target.relnamespace
 LEFT JOIN pg_catalog.pg_constraint k ON k.conindid = ix.indexrelid AND k.conrelid = target.oid AND k.contype IN ('p','u','x')
 WHERE target.oid = pg_catalog.to_regclass($1) AND idx.relname::text = $2
   AND ix.indisvalid AND ix.indisready AND ix.indislive AND ix.indimmediate
   AND NOT coalesce(k.condeferrable,false) AND (ix.indisunique OR NOT $4::boolean AND ix.indisexclusion)
   AND ($3::text = '' OR k.conname::text = $3)
   AND ($3::text <> '' OR $5::jsonb = 'null'::jsonb OR pg_catalog.jsonb_array_length($5::jsonb) = 0 OR
     ix.indexprs IS NULL AND ARRAY(
       SELECT att.attname::text FROM pg_catalog.unnest(ix.indkey::smallint[]) WITH ORDINALITY key(attnum,pos)
       JOIN pg_catalog.pg_attribute att ON att.attrelid = target.oid AND att.attnum = key.attnum
       WHERE key.pos <= ix.indnkeyatts ORDER BY att.attname::text COLLATE "C"
     ) = ARRAY(SELECT col FROM pg_catalog.jsonb_array_elements_text($5::jsonb) cols(col) ORDER BY col COLLATE "C"))
)`
