package sqlcheck

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/0xJean/tabula"
)

func checkDescription(ctx context.Context, tx VerificationTx, query string, params int, s *tabula.StatementShape, d Description) error {
	if len(d.Parameters) != params {
		return fmt.Errorf("protocol parameter count differs from prepared statement")
	}
	for i, oid := range d.Parameters {
		if len(s.Inputs[i].AllowedTypes) > 0 {
			matches, actualName, err := checkAllowedTypeOID(ctx, tx, oid, s.Inputs[i].AllowedTypes)
			if err != nil {
				return err
			}
			if !matches {
				return fmt.Errorf("protocol input %q at $%d has PostgreSQL type %s, not compatible with %v", s.Inputs[i].Name, i+1, actualName, s.Inputs[i].AllowedTypes)
			}
			continue
		}
		var matches bool
		if err := tx.Scan(ctx, `SELECT pg_catalog.to_regtype($1)::oid = $2::oid`, []any{s.Inputs[i].Type, oid}, &matches); err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("protocol input %q differs from %s", s.Inputs[i].Name, s.Inputs[i].Type)
		}
	}
	if len(d.Fields) != len(s.Result) {
		return fmt.Errorf("prepared result has %d fields, expected %d", len(d.Fields), len(s.Result))
	}
	if len(s.ResultLineage) != 0 && len(s.ResultLineage) != len(s.Result) {
		return fmt.Errorf("result lineage must follow the ordered result fields")
	}
	for _, lineage := range s.ResultLineage {
		if (lineage.Relation == "") != (lineage.Column == "") || lineage.CastType != "" && lineage.Relation != "" {
			return fmt.Errorf("result lineage requires either a complete physical origin or an outer cast")
		}
	}
	needsLogicalProbe := false
	for i, want := range s.Result {
		got := d.Fields[i]
		if got.Name != want.Name {
			return fmt.Errorf("prepared result field %d is %q, expected %q", i+1, got.Name, want.Name)
		}
		if len(want.AllowedTypes) > 0 {
			matches, actualName, err := checkAllowedTypeOID(ctx, tx, got.TypeOID, want.AllowedTypes)
			if err != nil {
				return err
			}
			if !matches {
				return fmt.Errorf("prepared result %q at position %d has PostgreSQL type %s, not compatible with %v", want.Name, i+1, actualName, want.AllowedTypes)
			}
			continue
		}
		var expected, base uint32
		err := tx.Scan(ctx, `WITH RECURSIVE types AS (
 SELECT oid, typbasetype FROM pg_catalog.pg_type WHERE oid = pg_catalog.to_regtype($1)
 UNION ALL SELECT t.oid, t.typbasetype FROM pg_catalog.pg_type t JOIN types p ON t.oid = p.typbasetype
) SELECT pg_catalog.to_regtype($1)::oid, oid FROM types WHERE typbasetype = 0`, []any{want.Type}, &expected, &base)
		if err != nil {
			return fmt.Errorf("result %q type %s: %w", want.Name, want.Type, err)
		}
		if got.TypeOID == expected {
			continue
		}
		if expected == base || got.TypeOID != base {
			return fmt.Errorf("prepared result %q differs from %s", want.Name, want.Type)
		}
		var lineage tabula.ResultLineage
		if len(s.ResultLineage) > 0 {
			lineage = s.ResultLineage[i]
		}
		verified, err := verifyLogicalOrigin(ctx, tx, got, expected, lineage)
		if err != nil {
			return err
		}
		if !verified && lineage.CastType != "" {
			verified, err = verifyResultCast(ctx, tx, query, i, expected, lineage.CastType)
			if err != nil {
				return err
			}
		}
		if !verified {
			if descriptionNeedsProtocol(query, s) {
				return fmt.Errorf("result %q: logical domain %s cannot be established from its base wire type", want.Name, want.Type)
			}
			needsLogicalProbe = true
		}
	}
	if !needsLogicalProbe {
		return nil
	}
	var err error
	if s.Effects&tabula.EffectRowLock != 0 {
		query, err = lockProjectionQuery(query)
		if err != nil {
			return err
		}
	}
	return logicalReadProbe(ctx, tx, query, params, s.Result)
}

func verifyLogicalOrigin(ctx context.Context, tx VerificationTx, field DescribedField, expected uint32, lineage tabula.ResultLineage) (bool, error) {
	if lineage.CastType != "" && (lineage.Relation != "" || lineage.Column != "") {
		return false, fmt.Errorf("result lineage cannot combine physical origin and cast")
	}
	if field.TableOID == 0 || field.Attribute <= 0 {
		return false, nil
	}
	var matches bool
	err := tx.Scan(ctx, `SELECT EXISTS (SELECT FROM pg_catalog.pg_attribute a
WHERE a.attrelid = $1::oid AND a.attnum = $2 AND NOT a.attisdropped AND a.atttypid = $3::oid
 AND ($4::text = '' OR a.attrelid = pg_catalog.to_regclass($4)) AND ($5::text = '' OR a.attname::text = $5))`, []any{field.TableOID, field.Attribute, expected, lineage.Relation, lineage.Column}, &matches)
	return matches, err
}

func verifyResultCast(ctx context.Context, tx VerificationTx, query string, index int, expected uint32, declared string) (bool, error) {
	actual, ok := explicitResultCast(query, index)
	if !ok {
		return false, nil
	}
	var matches bool
	err := tx.Scan(ctx, `SELECT pg_catalog.to_regtype($1)::oid = $3::oid AND pg_catalog.to_regtype($2)::oid = $3::oid`, []any{actual, declared, expected}, &matches)
	return matches, err
}

// Only a proven read reaches this legacy logical-type probe. Native names and
// base OIDs have already been checked by Describe on the unchanged statement.
func logicalReadProbe(ctx context.Context, tx VerificationTx, query string, params int, fields []tabula.ShapeField) error {
	args := make([]any, params, params+len(fields))
	checks := make([]string, len(fields))
	for i, field := range fields {
		args = append(args, field.Type)
		checks[i] = "pg_catalog.pg_typeof(r." + quoteIdent(field.Name) + ") = pg_catalog.to_regtype($" + strconv.Itoa(len(args)) + ")"
	}
	query = "SELECT COALESCE(" + strings.Join(checks, " AND ") + ", false) FROM (VALUES (1)) AS seed LEFT JOIN (SELECT * FROM (" + query + ") AS result LIMIT 0) AS r ON true"
	var matches bool
	if err := tx.Scan(ctx, query, args, &matches); err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("prepared result types differ from declared types")
	}
	return nil
}
