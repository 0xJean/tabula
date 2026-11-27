package sqlcheck

import (
	"fmt"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
)

func writeReadinessItems(n ReadinessNeeds) ([]readinessItem, error) {
	var out []readinessItem
	for _, need := range n.UniqueMatches {
		columns, err := writeRequirementColumns(need.Columns)
		if err != nil {
			return nil, err
		}
		if len(columns) == 0 {
			return nil, fmt.Errorf("sqlcheck: unique match requires distinct columns")
		}
		item, err := resolveReadinessItem(MissingRequirement{Kind: "UNIQUE", Object: need.Relation, Privilege: "IMMEDIATE UNIQUE MATCH (" + strings.Join(columns, ", ") + ")"})
		if err != nil {
			return nil, err
		}
		item.KeyColumns = columns
		out = append(out, item)
	}
	for _, need := range n.Defaults {
		columns, err := writeRequirementColumns(need.Columns)
		if err != nil {
			return nil, err
		}
		if len(columns) == 0 && !need.IncludesOmitted {
			return nil, fmt.Errorf("sqlcheck: defaults require distinct columns")
		}
		if need.IncludesOmitted {
			supplied, err := writeRequirementColumns(need.InsertColumns)
			if err != nil {
				return nil, err
			}
			item, err := resolveReadinessItem(MissingRequirement{Kind: "INSERT_DEFAULTS", Object: need.Relation})
			if err != nil {
				return nil, err
			}
			item.KeyColumns = supplied
			out = append(out, item)
			exists := item
			exists.Kind, exists.Privilege = "TABLE", "EXISTS"
			out = append(out, exists)
			for _, column := range supplied {
				exists.Kind, exists.Column = "COLUMN", column
				out = append(out, exists)
			}
		} else if len(need.InsertColumns) > 0 {
			return nil, fmt.Errorf("sqlcheck: insert columns require omitted-default discovery")
		}
		for _, column := range columns {
			item, err := resolveReadinessItem(MissingRequirement{Kind: "DEFAULT", Object: need.Relation, Column: column, Privilege: "DEFAULT VALUE"})
			if err != nil {
				return nil, err
			}
			out = append(out, item)
		}
	}
	for _, need := range n.Conflicts {
		columns, err := writeRequirementColumns(need.Columns)
		if err != nil {
			return nil, err
		}
		if need.Constraint != "" && (!catalogName(need.Constraint) || len(columns) > 0) {
			return nil, fmt.Errorf("sqlcheck: conflict requires one constraint or inference columns")
		}
		if need.DoUpdate && need.Constraint == "" && len(columns) == 0 {
			return nil, fmt.Errorf("sqlcheck: conflict update requires a target")
		}
		item, err := resolveReadinessItem(MissingRequirement{Kind: "CONFLICT", Object: need.Relation, Privilege: "IMMEDIATE CONFLICT ARBITER"})
		if err != nil {
			return nil, err
		}
		item.KeyColumns, item.Constraint, item.ConflictUpdate = columns, need.Constraint, need.DoUpdate
		out = append(out, item)
	}
	return out, nil
}

func writeRequirementColumns(names []string) ([]string, error) {
	out := append([]string{}, names...)
	slices.Sort(out)
	for i, name := range out {
		if !catalogName(name) || i > 0 && out[i-1] == name {
			return nil, fmt.Errorf("sqlcheck: invalid or duplicate requirement column")
		}
	}
	return out, nil
}

func cloneUniqueMatches(in []tabula.UniqueMatchRequirement) []tabula.UniqueMatchRequirement {
	out := slices.Clone(in)
	for i := range out {
		out[i].Columns = slices.Clone(out[i].Columns)
	}
	return out
}
func cloneDefaults(in []tabula.DefaultRequirement) []tabula.DefaultRequirement {
	out := slices.Clone(in)
	for i := range out {
		out[i].Columns = slices.Clone(out[i].Columns)
		out[i].InsertColumns = slices.Clone(out[i].InsertColumns)
	}
	return out
}
func cloneConflicts(in []tabula.ConflictRequirement) []tabula.ConflictRequirement {
	out := slices.Clone(in)
	for i := range out {
		out[i].Columns = slices.Clone(out[i].Columns)
	}
	return out
}
