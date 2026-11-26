package tabula

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const defaultShapeBudget = 256

type shapeChoice struct {
	name   string
	values []string
}

type shapeAlternative struct{ choice, value string }

// enumerateShapes preserves option and alternative order. Reachability can
// exclude a whole prefix, for example when a selected operation has no order.
// It returns no partial list if the plan exceeds its registration budget.
func enumerateShapes(plan string, budget int, choices []shapeChoice, reachable func([]shapeAlternative) bool) ([][]shapeAlternative, error) {
	if budget == 0 {
		budget = defaultShapeBudget
	}
	if budget < 1 || plan == "" {
		return nil, fmt.Errorf("tabula: a plan name and positive shape budget are required")
	}
	names := make(map[string]bool)
	counts := make([]string, 0, len(choices))
	for _, c := range choices {
		if c.name == "" || names[c.name] || len(c.values) == 0 {
			return nil, fmt.Errorf("tabula: plan %q has an empty or duplicate choice %q", plan, c.name)
		}
		names[c.name] = true
		seen := make(map[string]bool)
		for _, v := range c.values {
			if v == "" || seen[v] {
				return nil, fmt.Errorf("tabula: plan %q choice %q has an empty or duplicate alternative %q", plan, c.name, v)
			}
			seen[v] = true
		}
		counts = append(counts, c.name+"="+strconv.Itoa(len(c.values)))
	}
	var rows [][]shapeAlternative
	path := make([]shapeAlternative, len(choices))
	var visit func(int) error
	visit = func(i int) error {
		if reachable != nil && !reachable(path[:i]) {
			return nil
		}
		if i == len(choices) {
			if len(rows) == budget {
				selected := make([]string, len(path))
				for j, alt := range path {
					selected[j] = alt.choice + "=" + alt.value
				}
				return fmt.Errorf("tabula: plan %q exceeds %d shapes at [%s], alternatives [%s]",
					plan, budget, strings.Join(selected, ", "), strings.Join(counts, ", "))
			}
			rows = append(rows, slices.Clone(path))
			return nil
		}
		for _, v := range choices[i].values {
			path[i] = shapeAlternative{choices[i].name, v}
			if err := visit(i + 1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(0); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("tabula: plan %q has no reachable shapes", plan)
	}
	return rows, nil
}
