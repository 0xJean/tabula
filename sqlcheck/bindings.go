package sqlcheck

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
)

// SQLBinding assigns one inventoried site to its execution roles. Definition
// names the owning table or view when the collector cannot resolve it. Prefix
// optionally narrows the variants of that one composed query. Manual Variants
// carry their own Access or NoAccess requirements. Skip requires a registered exception.
type SQLBinding struct {
	Roles      []string
	Access     []PolicyAccess
	NoAccess   bool `json:",omitempty"`
	Variants   []Statement
	Definition string `json:",omitempty"`
	Prefix     string `json:",omitempty"`
	Skip       bool
}

// BindRoles produces a workload from the collected SQL. Missing and stale
// assignments, unreviewed dynamic sites and empty workloads fail validation.
func (inv SQLInventory) BindRoles(bindings map[string]SQLBinding) (RoleStatements, error) {
	return inv.BindRolesWithDefinitions(bindings, nil, nil)
}

// BindRolesWithDefinitions expands each static composed query or aggregate
// against its owning definition. A missing owner, ambiguous definition, or
// site with no matching statements returns an error and no partial workload.
// Supply named Variants for SQL that is not registered in a definition.
func (inv SQLInventory) BindRolesWithDefinitions(
	bindings map[string]SQLBinding,
	defs []tabula.Definition,
	views []tabula.ViewDefinition,
) (RoleStatements, error) {
	if len(inv.Findings) > 0 {
		return nil, fmt.Errorf("inventory has unresolved findings: %v", inv.Findings)
	}
	byName := make(map[string][]tabula.Definition, len(defs)+len(views))
	for _, d := range defs {
		byName[d.Name] = append(byName[d.Name], d)
	}
	for _, v := range views {
		byName[v.Name] = append(byName[v.Name], tabula.Definition(v))
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
			if strings.TrimSpace(site.Exception) == "" || len(b.Roles)+len(b.Access)+len(b.Variants) != 0 || b.NoAccess || b.Definition != "" || b.Prefix != "" {
				return nil, fmt.Errorf("%s: skipped site requires a reviewed exception and no workload", site.ID)
			}
			continue
		}
		if _, err := scopeArray(b.Roles); err != nil {
			return nil, fmt.Errorf("%s roles: %w", site.ID, err)
		}
		stmts, err := site.bindStatements(b, byName)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", site.ID, err)
		}
		for _, role := range b.Roles {
			out[role] = append(out[role], stmts...)
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

func (site SQLSite) bindStatements(b SQLBinding, byName map[string][]tabula.Definition) ([]Statement, error) {
	if b.NoAccess && len(b.Access) > 0 {
		return nil, fmt.Errorf("cannot combine NoAccess with Access")
	}
	registered := site.Kind == "composed_query" || site.Kind == "aggregate"
	if site.Dynamic && (strings.TrimSpace(site.Exception) == "" || len(b.Variants) == 0) {
		return nil, fmt.Errorf("dynamic site needs reviewed variants")
	}
	if len(b.Variants) > 0 {
		if !site.Dynamic && !registered {
			return nil, fmt.Errorf("static site uses its collected SQL")
		}
		if len(b.Access) > 0 || b.NoAccess {
			return nil, fmt.Errorf("supply Access or NoAccess on each variant when using Variants")
		}
		if b.Definition != "" || b.Prefix != "" {
			return nil, fmt.Errorf("manual variants cannot also select a definition or prefix")
		}
		stmts := slices.Clone(b.Variants)
		for i := range stmts {
			if strings.TrimSpace(stmts[i].Name) == "" {
				return nil, fmt.Errorf("unnamed variant")
			}
			stmts[i].Name = site.ID + "/" + stmts[i].Name
		}
		return stmts, nil
	}
	if b.Prefix != "" && site.Kind != "composed_query" {
		return nil, fmt.Errorf("binding Prefix requires a composed query")
	}
	if !registered {
		if b.Definition != "" {
			return nil, fmt.Errorf("binding Definition requires a composed query or aggregate")
		}
		return []Statement{{Name: site.ID, SQL: site.SQL, Access: b.Access, NoAccess: b.NoAccess}}, nil
	}
	if site.Method == "TryDefineQuery" {
		return nil, fmt.Errorf("supply named Variants for %q compiled with TryDefineQuery", site.QueryKey)
	}

	if site.Definition != "" && b.Definition != "" && site.Definition != b.Definition {
		return nil, fmt.Errorf("binding definition %q conflicts with source definition %q", b.Definition, site.Definition)
	}
	name := cmp.Or(site.Definition, b.Definition)
	if name == "" {
		return nil, fmt.Errorf("unknown owner of %q: specify Definition or supply named Variants", site.QueryKey)
	}
	matches := byName[name]
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("missing definition %q for %q", name, site.QueryKey)
	case 1:
		return site.expand(matches[0], b)
	default:
		return nil, fmt.Errorf("ambiguous definition %q for %q", name, site.QueryKey)
	}
}

func (site SQLSite) expand(d tabula.Definition, b SQLBinding) ([]Statement, error) {
	var keys []string
	switch {
	case site.Kind == "composed_query":
		exact := strings.TrimSuffix(b.Prefix, ".")
		for key, query := range d.StatementQueries {
			// Composed variants have a dotted suffix. Scalar registrations can
			// share an owner string with a user-chosen composed query key.
			if query != site.QueryKey || query == "" || !strings.HasPrefix(key, query+".") {
				continue
			}
			if strings.HasPrefix(key, b.Prefix) || key == exact {
				keys = append(keys, key)
			}
		}
		if len(keys) == 0 {
			return nil, fmt.Errorf("no registered variants for %q in definition %q matching prefix %q (regenerate the snapshot or supply named Variants)", site.QueryKey, d.Name, b.Prefix)
		}
		slices.Sort(keys)
	case site.QueryKey == "exists":
		keys = []string{"exists"}
	default:
		op, column, ok := strings.Cut(site.QueryKey, ":")
		if !ok || column == "" {
			return nil, fmt.Errorf("aggregate %q in definition %q requires named Variants", site.QueryKey, d.Name)
		}
		key := op + "_" + column
		switch op {
		case "countby":
			key = "count_by_" + column
		case "max", "min", "sum", "avg":
			if !site.NoFilter {
				return nil, fmt.Errorf("aggregate %q: supply named Variants for a filter callback or recollect the inventory for a nil filter", site.QueryKey)
			}
			if d.StatementQueries[key] != site.QueryKey {
				return nil, fmt.Errorf("aggregate %q in definition %q: declare TableSpec.Aggregates and regenerate the snapshot or supply named Variants", site.QueryKey, d.Name)
			}
		default:
			return nil, fmt.Errorf("unknown aggregate %q in definition %q", site.QueryKey, d.Name)
		}
		keys = []string{key}
		if unscoped := "unscoped_" + key; d.StatementQueries[unscoped] == site.QueryKey {
			keys = append(keys, unscoped)
		}
	}
	stmts := make([]Statement, 0, len(keys))
	for _, key := range keys {
		query, ok := d.Statements[key]
		if !ok {
			return nil, fmt.Errorf("missing registered statement %q in definition %q for %q (check the snapshot or supply named Variants)", key, d.Name, site.QueryKey)
		}
		stmts = append(stmts, Statement{Name: site.ID + "/" + d.Name + "/" + key, SQL: query, Access: b.Access, NoAccess: b.NoAccess})
	}
	return stmts, nil
}
