package sqlcheck

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/0xJean/tabula"
)

// ReadinessNeeds describes minimum capabilities for one role. Privileges accepts
// the result of CollectPrivilegeNeeds. Relations and function signatures must be
// schema-qualified. Schema and column names are their exact catalog names.
type ReadinessNeeds struct {
	Privileges      PrivilegeNeeds
	SchemaUsage     []string
	AnyColumnSelect []string                       // SELECT on at least one column of each relation
	AnyColumnUpdate []string                       // UPDATE on at least one column of each locked relation
	Routines        []tabula.RoutineRequirement    // exact function catalog contracts
	ForceRLS        []string                       // both ENABLE and FORCE ROW LEVEL SECURITY
	SequenceUsage   []string                       // named sequences in addition to column-owned sequences
	Keys            []tabula.KeyRequirement        // immediate primary-key premises
	Comparisons     []tabula.ComparisonRequirement // native ordering and deterministic collations
}

// MissingRequirement identifies one unmet capability. Column is set for a named
// column grant or its owned sequence. Kind is ROLE, SCHEMA, TABLE, COLUMN,
// SEQUENCE, FUNCTION, KEY or COMPARISON. A missing role produces only the
// ROLE/EXISTS finding.
type MissingRequirement struct {
	Kind, Object, Column, Privilege string
}

// CheckReadiness checks the union of all needs for role in one catalog query.
// It returns sorted missing capabilities, including absent objects. Use
// CheckRolePrivileges for an audit that also reports extra grants.
func CheckReadiness(ctx context.Context, db *sql.DB, role string, needs ...ReadinessNeeds) ([]MissingRequirement, error) {
	if db == nil {
		return nil, errors.New("sqlcheck: readiness requires a database")
	}
	return checkReadiness(ctx, db, role, needs)
}

//go:embed readiness.sql
var readinessSQL string

type readinessItem struct {
	MissingRequirement
	Schema, Name                     string
	Args                             []readinessType
	Routine                          *readinessRoutine
	KeyColumns                       []string
	WithSchema, WithName, WithColumn string
}

type readinessRoutine struct {
	ResultSchema, ResultName              string
	ReturnsSet, ReadOnly, SecurityDefiner bool
}

func checkReadiness(ctx context.Context, db queryer, role string, groups []ReadinessNeeds) ([]MissingRequirement, error) {
	if !catalogName(role) {
		return nil, fmt.Errorf("sqlcheck: invalid readiness role %q", role)
	}
	items := make([]readinessItem, 0)
	for _, group := range groups {
		groupItems, err := readinessItems(group)
		if err != nil {
			return nil, err
		}
		items = append(items, groupItems...)
	}
	input, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return readRows(ctx, db, readinessSQL, func(rows *sql.Rows) (MissingRequirement, error) {
		var m MissingRequirement
		err := rows.Scan(&m.Kind, &m.Object, &m.Column, &m.Privilege)
		return m, err
	}, role, string(input))
}

func catalogName(s string) bool {
	return s != "" && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

func readinessItems(n ReadinessNeeds) ([]readinessItem, error) {
	var out []readinessItem
	add := func(kind, object, column, privilege string) error {
		req := MissingRequirement{Kind: kind, Object: object, Column: column, Privilege: privilege}
		item, err := resolveReadinessItem(req)
		if err != nil {
			return err
		}
		out = append(out, item)
		return nil
	}
	for name, privileges := range n.Privileges.Columns {
		for privilege, columns := range privileges {
			switch privilege {
			case "SELECT", "INSERT", "UPDATE":
			default:
				return nil, fmt.Errorf("sqlcheck: invalid column privilege %q", privilege)
			}
			for column := range columns {
				if !catalogName(column) {
					return nil, fmt.Errorf("sqlcheck: invalid column name %q", column)
				}
				if err := add("COLUMN", name, column, privilege); err != nil {
					return nil, err
				}
			}
		}
	}
	for name := range n.Privileges.TableDelete {
		if err := add("TABLE", name, "", "DELETE"); err != nil {
			return nil, err
		}
	}
	for _, pair := range n.Privileges.Sequences {
		if !catalogName(pair[1]) {
			return nil, fmt.Errorf("sqlcheck: invalid sequence owner column %q", pair[1])
		}
		if err := add("SEQUENCE", pair[0], pair[1], "USAGE"); err != nil {
			return nil, err
		}
	}
	for _, group := range []struct {
		names           []string
		kind, privilege string
	}{
		{n.Privileges.Functions, "FUNCTION", "EXECUTE"},
		{n.SchemaUsage, "SCHEMA", "USAGE"},
		{n.AnyColumnSelect, "TABLE", "SELECT ANY COLUMN"},
		{n.AnyColumnUpdate, "TABLE", "UPDATE ANY COLUMN"},
		{n.ForceRLS, "TABLE", "ENABLE RLS"},
		{n.ForceRLS, "TABLE", "FORCE RLS"},
		{n.SequenceUsage, "SEQUENCE", "USAGE"},
	} {
		for _, name := range group.names {
			if err := add(group.kind, name, "", group.privilege); err != nil {
				return nil, err
			}
		}
	}
	for _, routine := range n.Routines {
		item, err := routineItem(routine)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	proofs, err := proofReadinessItems(n.Keys, n.Comparisons)
	if err != nil {
		return nil, err
	}
	return append(out, proofs...), nil
}

func routineItem(r tabula.RoutineRequirement) (readinessItem, error) {
	result, err := resolveReadinessItem(MissingRequirement{Kind: "TYPE", Object: r.ResultType})
	if err != nil {
		return readinessItem{}, err
	}
	returns := "RETURNS "
	if r.ReturnsSet {
		returns += "SETOF "
	}
	contract := returns + r.ResultType
	if r.ReadOnly {
		contract += " STABLE OR IMMUTABLE"
	}
	if r.SecurityDefiner {
		contract += " SECURITY DEFINER"
	} else {
		contract += " SECURITY INVOKER"
	}
	item, err := resolveReadinessItem(MissingRequirement{Kind: "FUNCTION", Object: r.Signature, Privilege: contract})
	if err != nil {
		return readinessItem{}, err
	}
	item.Routine = &readinessRoutine{result.Schema, result.Name, r.ReturnsSet, r.ReadOnly, r.SecurityDefiner}
	return item, nil
}
