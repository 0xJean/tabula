package sqlcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/pgstmt"
)

const allowedTypeOIDSQL = `WITH RECURSIVE actual_chain AS (
 SELECT oid, typbasetype, typelem FROM pg_catalog.pg_type WHERE oid = $1::oid
 UNION ALL
 SELECT t.oid, t.typbasetype, t.typelem FROM pg_catalog.pg_type t JOIN actual_chain p ON t.oid = p.typbasetype
), actual_base AS (
 SELECT oid, typelem FROM actual_chain WHERE typbasetype = 0
), actual_elem_chain AS (
 SELECT t.oid, t.typbasetype, t.typarray FROM pg_catalog.pg_type t JOIN actual_base b ON b.typelem <> 0 AND t.oid = b.typelem AND t.typarray = b.oid
 UNION ALL
 SELECT t.oid, t.typbasetype, t.typarray FROM pg_catalog.pg_type t JOIN actual_elem_chain p ON t.oid = p.typbasetype
), actual_oids AS (
 SELECT oid FROM actual_chain
 UNION
 SELECT typarray FROM actual_elem_chain WHERE typbasetype = 0 AND typarray <> 0
)
SELECT EXISTS (
 SELECT FROM pg_catalog.jsonb_array_elements_text($2::jsonb) AS allowed(name)
 WHERE pg_catalog.to_regtype(allowed.name)::oid IN (SELECT oid FROM actual_oids)
), pg_catalog.format_type($1::oid, NULL)`

func checkAllowedTypeOID(ctx context.Context, tx VerificationTx, actualOID uint32, allowed []string) (bool, string, error) {
	if actualOID == 0 {
		return false, "unknown", nil
	}
	raw, err := json.Marshal(allowed)
	if err != nil {
		return false, "", err
	}
	var matches bool
	var actualName string
	if err := tx.Scan(ctx, allowedTypeOIDSQL, []any{actualOID, string(raw)}, &matches, &actualName); err != nil {
		return false, "", err
	}
	return matches, actualName, nil
}

func checkPreparedParamAllowed(ctx context.Context, tx VerificationTx, stmtName string, pos int, in tabula.ShapeInput) error {
	var paramOID uint32
	err := tx.Scan(ctx, `SELECT coalesce(parameter_types[$2]::oid, 0)
FROM pg_catalog.pg_prepared_statements WHERE name = $1`, []any{stmtName, pos}, &paramOID)
	if err != nil {
		return err
	}
	matches, actualName, err := checkAllowedTypeOID(ctx, tx, paramOID, in.AllowedTypes)
	if err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("prepared input %q at $%d has PostgreSQL type %s, not compatible with %v", in.Name, pos, actualName, in.AllowedTypes)
	}
	return nil
}

// stripSQLRowLock removes trailing PostgreSQL row-lock clauses
// (FOR UPDATE / FOR NO KEY UPDATE / FOR SHARE / FOR KEY SHARE, optional OF
// target list, optional NOWAIT / SKIP LOCKED) for database/sql read-result
// probes on DefineSQL shapes. Nested row locks require protocol Describe.
func stripSQLRowLock(query string) (string, error) {
	spans, err := pgstmt.ScanFragment(query)
	if err != nil {
		return "", err
	}
	var tokens []pgstmt.Span
	for _, token := range spans {
		if token.Kind != pgstmt.Comment {
			tokens = append(tokens, token)
		}
	}
	depth, start := 0, -1
	for i, token := range tokens {
		if token.Kind == pgstmt.Symbol {
			if token.Text == "(" {
				depth++
			}
			if token.Text == ")" {
				depth--
			}
			continue
		}
		if token.Kind != pgstmt.Word || token.Text != "for" {
			continue
		}
		if _, ok := lockStrengthEnd(tokens, i); !ok {
			continue
		}
		if depth != 0 {
			return "", fmt.Errorf("nested row locks: %w", ErrDescribeRequired)
		}
		start = i
		break
	}
	if start < 0 {
		return "", errors.New("expected a trailing row-lock clause")
	}
	for i := start; i < len(tokens); {
		end, ok := lockStrengthEnd(tokens, i)
		if !ok {
			return "", fmt.Errorf("trailing tokens after row-lock clause: %w", ErrDescribeRequired)
		}
		i = end
		if wordSpan(tokens, i, "of") {
			i++
			for {
				if i >= len(tokens) || (tokens[i].Kind != pgstmt.Word && tokens[i].Kind != pgstmt.Quoted) {
					return "", errors.New("invalid row-lock OF target")
				}
				i++
				if i >= len(tokens) || tokens[i].Text != "," {
					break
				}
				i++
			}
		}
		if wordSpan(tokens, i, "nowait") {
			i++
		} else if wordSpan(tokens, i, "skip") && wordSpan(tokens, i+1, "locked") {
			i += 2
		}
	}
	return strings.TrimSpace(query[:tokens[start].Start]), nil
}
