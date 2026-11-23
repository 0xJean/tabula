package sqlcheck

import (
	"fmt"
	"maps"
	"slices"
)

// SQLBinding assigns one inventoried site to its execution roles. Dynamic
// builders require reviewed named variants. Skip requires a registered exception.
type SQLBinding struct {
	Roles    []string
	Access   []PolicyAccess
	Variants []Statement
	Skip     bool
}

// BindRoles produces a workload from the collected SQL. Missing and stale
// assignments, unreviewed dynamic sites and empty workloads fail validation.
func (inv SQLInventory) BindRoles(bindings map[string]SQLBinding) (RoleStatements, error) {
	if len(inv.Findings) > 0 {
		return nil, fmt.Errorf("inventory has unresolved findings: %v", inv.Findings)
	}
	out := RoleStatements{}
	seen := map[string]bool{}
	for _, site := range inv.Sites {
		b, ok := bindings[site.ID]
		if !ok {
			return nil, fmt.Errorf("missing role assignment for %s", site.ID)
		}
		seen[site.ID] = true
		if b.Skip {
			if site.Exception == "" || len(b.Roles)+len(b.Access)+len(b.Variants) != 0 {
				return nil, fmt.Errorf("%s: skipped site requires a reviewed exception and no workload", site.ID)
			}
			continue
		}
		if _, err := scopeArray(b.Roles); err != nil {
			return nil, fmt.Errorf("%s roles: %w", site.ID, err)
		}
		stmts := []Statement{{Name: site.ID, SQL: site.SQL, Access: b.Access}}
		if site.Dynamic {
			if site.Exception == "" || len(b.Variants) == 0 || len(b.Access) > 0 {
				return nil, fmt.Errorf("%s: dynamic site needs reviewed variants with their own access requirements", site.ID)
			}
			stmts = slices.Clone(b.Variants)
			for i := range stmts {
				if stmts[i].Name == "" {
					return nil, fmt.Errorf("%s: unnamed variant", site.ID)
				}
				stmts[i].Name = site.ID + "/" + stmts[i].Name
			}
		} else if len(b.Variants) > 0 {
			return nil, fmt.Errorf("%s: static site uses its collected SQL", site.ID)
		}
		for _, r := range b.Roles {
			out[r] = append(out[r], stmts...)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(bindings)) {
		if !seen[id] {
			return nil, fmt.Errorf("stale role assignment for %s", id)
		}
	}
	if err := out.validate(); err != nil {
		return nil, err
	}
	return out, nil
}
