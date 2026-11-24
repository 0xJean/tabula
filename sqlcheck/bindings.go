package sqlcheck

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
)

// SQLBinding assigns one inventoried site to its execution roles. Dynamic
// builders require reviewed named variants. Skip requires a registered exception.
// Composed-query sites (Kind == "composed_query") expand automatically from
// registered ViewDefinitions or snapshot definitions passed to
// BindRolesWithDefinitions without manual variant listing.
type SQLBinding struct {
	Roles    []string
	Access   []PolicyAccess
	Variants []Statement
	Prefix   string `json:",omitempty"`
	Skip     bool
}

// BindRoles produces a workload from the collected SQL. Missing and stale
// assignments, unreviewed dynamic sites and empty workloads fail validation.
func (inv SQLInventory) BindRoles(bindings map[string]SQLBinding) (RoleStatements, error) {
	return inv.BindRolesWithDefinitions(bindings, nil, nil)
}

// BindRolesWithDefinitions is BindRoles with table Definitions and
// ViewDefinitions (including C3 composed-query variants and C1 aggregates) so
// composed-query and aggregate sites expand automatically without manual listing.
func (inv SQLInventory) BindRolesWithDefinitions(
	bindings map[string]SQLBinding,
	defs []tabula.Definition,
	views []tabula.ViewDefinition,
) (RoleStatements, error) {
	if len(inv.Findings) > 0 {
		return nil, fmt.Errorf("inventory has unresolved findings: %v", inv.Findings)
	}
	allDefs := make([]tabula.Definition, 0, len(defs)+len(views))
	allDefs = append(allDefs, defs...)
	allDefs = append(allDefs, ViewsToDefinitions(views)...)

	out := RoleStatements{}
	seen := map[string]bool{}
	for _, site := range inv.Sites {
		b, ok := bindings[site.ID]
		if !ok {
			return nil, fmt.Errorf("missing role assignment for %s", site.ID)
		}
		seen[site.ID] = true
		if b.Skip {
			if site.Exception == "" && site.Kind != "aggregate" {
				return nil, fmt.Errorf("%s: skipped site requires a reviewed exception and no workload", site.ID)
			}
			if len(b.Roles)+len(b.Access)+len(b.Variants) != 0 {
				return nil, fmt.Errorf("%s: skipped site requires a reviewed exception and no workload", site.ID)
			}
			continue
		}
		if _, err := scopeArray(b.Roles); err != nil {
			return nil, fmt.Errorf("%s roles: %w", site.ID, err)
		}
		var stmts []Statement
		switch {
		case site.Dynamic:
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
		case site.Kind == "composed_query":
			if len(b.Variants) > 0 {
				stmts = slices.Clone(b.Variants)
				for i := range stmts {
					stmts[i].Name = site.ID + "/" + stmts[i].Name
				}
			} else {
				prefix := b.Prefix
				if prefix == "" {
					prefix = site.QueryKey + "."
				}
				stmts = expandPrefixStatements(site.ID, prefix, allDefs, b.Access)
				if len(stmts) == 0 {
					return nil, fmt.Errorf("%s: no registered composed-query variants match prefix %q", site.ID, prefix)
				}
			}
		case site.Kind == "aggregate":
			if len(b.Variants) > 0 {
				stmts = slices.Clone(b.Variants)
				for i := range stmts {
					stmts[i].Name = site.ID + "/" + stmts[i].Name
				}
			} else if len(allDefs) > 0 {
				stmts = expandAggregateStatements(site.ID, site.QueryKey, allDefs, b.Access)
			}
			if len(stmts) == 0 {
				// Aggregate queries over declared columns have no raw SQL text unless
				// matched against table Definitions; allow explicit role binding without
				// blocking static verification when no extra SQL is required.
				continue
			}
		default:
			if len(b.Variants) > 0 {
				return nil, fmt.Errorf("%s: static site uses its collected SQL", site.ID)
			}
			stmts = []Statement{{Name: site.ID, SQL: site.SQL, Access: b.Access}}
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

func expandPrefixStatements(siteID, prefix string, defs []tabula.Definition, access []PolicyAccess) []Statement {
	var out []Statement
	for _, d := range defs {
		keys := slices.Sorted(maps.Keys(d.Statements))
		for _, k := range keys {
			if strings.HasPrefix(k, prefix) || k == strings.TrimSuffix(prefix, ".") {
				out = append(out, Statement{
					Name:   siteID + "/" + d.Name + "/" + k,
					SQL:    d.Statements[k],
					Access: access,
				})
			}
		}
	}
	return out
}

func expandAggregateStatements(siteID, queryKey string, defs []tabula.Definition, access []PolicyAccess) []Statement {
	var targetSuffix string
	switch {
	case queryKey == "exists":
		targetSuffix = "exists"
	case strings.HasPrefix(queryKey, "countby:"):
		targetSuffix = "count_by_" + strings.TrimPrefix(queryKey, "countby:")
	default:
		return nil
	}
	var out []Statement
	for _, d := range defs {
		if query, ok := d.Statements[targetSuffix]; ok {
			out = append(out, Statement{
				Name:   siteID + "/" + d.Name + "/" + targetSuffix,
				SQL:    query,
				Access: access,
			})
		}
	}
	return out
}
