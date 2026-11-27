package sqlcheck

import (
	"fmt"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
)

func proofReadinessItems(keys []tabula.KeyRequirement, comparisons []tabula.ComparisonRequirement) ([]readinessItem, error) {
	var items []readinessItem
	for _, key := range keys {
		columns := slices.Clone(key.Columns)
		slices.Sort(columns)
		if len(columns) == 0 || len(slices.Compact(slices.Clone(columns))) != len(columns) {
			return nil, fmt.Errorf("sqlcheck: key requires distinct columns")
		}
		for _, name := range columns {
			if !catalogName(name) {
				return nil, fmt.Errorf("sqlcheck: invalid key column %q", name)
			}
		}
		item, err := resolveReadinessItem(MissingRequirement{Kind: "KEY", Object: key.Relation, Privilege: "IMMEDIATE PRIMARY KEY (" + strings.Join(columns, ", ") + ")"})
		if err != nil {
			return nil, err
		}
		item.KeyColumns = columns
		items = append(items, item)
	}
	for _, comparison := range comparisons {
		if !catalogName(comparison.Column) {
			return nil, fmt.Errorf("sqlcheck: invalid comparison column")
		}
		item, err := resolveReadinessItem(MissingRequirement{Kind: "COMPARISON", Object: comparison.Relation, Column: comparison.Column, Privilege: "NATIVE DETERMINISTIC ORDER"})
		if err != nil {
			return nil, err
		}
		if comparison.WithRelation != "" || comparison.WithColumn != "" {
			if !catalogName(comparison.WithColumn) {
				return nil, fmt.Errorf("sqlcheck: invalid comparison peer column")
			}
			peer, err := resolveReadinessItem(MissingRequirement{Kind: "COMPARISON", Object: comparison.WithRelation})
			if err != nil {
				return nil, err
			}
			item.WithSchema, item.WithName, item.WithColumn = peer.Schema, peer.Name, comparison.WithColumn
			item.Privilege += " WITH " + comparison.WithRelation + "." + quoteIdent(comparison.WithColumn)
		}
		items = append(items, item)
	}
	return items, nil
}
