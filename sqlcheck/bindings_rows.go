package sqlcheck

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/0xJean/tabula"
)

func definitionStatement(d tabula.Definition, key, name string) Statement {
	stmt := Statement{Name: name, SQL: d.Statements[key]}
	if shape, ok := d.StatementShapes[key]; ok {
		stmt.Shape = &shape
		stmt.NoAccess = stmt.builtInAdvisoryLock()
		for _, a := range shape.Access {
			stmt.Access = append(stmt.Access, PolicyAccess{Table: a.Relation, Command: a.Command})
		}
		if shape.Effects&tabula.EffectRowLock != 0 {
			for _, table := range shape.AnyColumnUpdate {
				update := PolicyAccess{Table: table, Command: "UPDATE"}
				if !slices.Contains(stmt.Access, update) {
					stmt.Access = append(stmt.Access, update)
				}
			}
		}
	}
	return stmt
}

func (site SQLSite) bindRowPlan(b SQLBinding, defs map[string][]tabula.Definition) ([]Statement, error) {
	if len(b.Variants) != 0 {
		return nil, fmt.Errorf("typed plans use registered SQL")
	}
	if site.Definition != "" && b.Definition != "" && b.Definition != site.Definition ||
		site.QueryKey != "" && b.Plan != "" && site.QueryKey != b.Plan {
		return nil, fmt.Errorf("binding conflicts with source definition or plan")
	}
	name, plan := cmp.Or(site.Definition, b.Definition), cmp.Or(site.QueryKey, b.Plan)
	if name == "" || plan == "" {
		return nil, fmt.Errorf("indirect typed plan: specify Definition and Plan")
	}
	if len(defs[name]) != 1 {
		return nil, fmt.Errorf("missing or ambiguous definition %q", name)
	}
	d := defs[name][0]
	selected := make(map[string]bool)
	prefix := strings.TrimSuffix(b.Prefix, ".")
	for _, key := range slices.Sorted(maps.Keys(d.StatementQueries)) {
		if d.StatementQueries[key] != plan {
			continue
		}
		shape, ok := d.StatementShapes[key]
		if !ok || d.Statements[key] == "" || shape.Effects == 0 {
			return nil, fmt.Errorf("missing typed statement metadata for %s/%s", name, key)
		}
		if prefix != "" && key != prefix && !strings.HasPrefix(key, prefix+".") {
			continue
		}
		if !site.matchesTypedShape(key, plan, shape) {
			continue
		}
		selected[key] = true
	}
	if site.Method == "List" {
		foundData := false
		for key := range selected {
			if !matchesRowMethod(key, plan, "Find") {
				continue
			}
			foundData = true
			count := pairedRowCount(key, plan)
			if _, ok := d.StatementShapes[count]; !ok || d.StatementQueries[count] != plan || d.Statements[count] == "" {
				return nil, fmt.Errorf("missing count shape %s/%s for %s", name, count, key)
			}
			selected[count] = true
		}
		if !foundData {
			return nil, fmt.Errorf("list execution requires a matching data shape")
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no matching typed shapes for %s/%s", name, plan)
	}
	out := make([]Statement, 0, len(selected))
	for _, key := range slices.Sorted(maps.Keys(selected)) {
		stmt := definitionStatement(d, key, site.ID+"/"+name+"/"+key)
		if err := stmt.bindPolicyAccess(b); err != nil {
			return nil, err
		}
		out = append(out, stmt)
	}
	return out, nil
}

// A routine body can read tables even when its calling SQL has no FROM clause.
func (s Statement) needsPolicyAccess() bool {
	return s.Shape != nil && len(s.Shape.Access) == 0 && len(s.Access) == 0 && !s.NoAccess && !s.builtInAdvisoryLock()
}

// The dedicated transaction advisory operation has no table or routine body.
func (s Statement) builtInAdvisoryLock() bool {
	return s.Shape != nil && s.Shape.Effects == tabula.EffectRead|tabula.EffectAdvisoryLock &&
		len(s.Shape.Routines) == 0 && len(s.Shape.Access) == 0
}

func (s *Statement) bindPolicyAccess(b SQLBinding) error {
	if !s.needsPolicyAccess() {
		if b.NoAccess && !s.NoAccess || len(b.Access) > 0 {
			return fmt.Errorf("%s already has registered policy access", s.Name)
		}
		return nil
	}
	if !b.NoAccess && len(b.Access) == 0 {
		return fmt.Errorf("%s requires reviewed Access or NoAccess", s.Name)
	}
	s.Access, s.NoAccess = slices.Clone(b.Access), b.NoAccess
	return nil
}

// Drop the data order and page. An order can itself be named "where".
func pairedRowCount(key, plan string) string {
	scope, rest, _ := strings.Cut(strings.TrimPrefix(key, plan+"."), ".")
	tail := strings.TrimPrefix(rest, "data")
	if order, ok := strings.CutPrefix(tail, ".sort."); ok {
		_, tail, _ = strings.Cut(order, ".")
	} else {
		tail = strings.TrimPrefix(tail, ".")
	}
	count := plan + "." + scope + ".count"
	if filters, ok := strings.CutPrefix(tail, "where."); ok {
		return count + ".where." + filters
	}
	if _, filters, ok := strings.Cut(tail, ".where."); ok {
		return count + ".where." + filters
	}
	return count
}

func matchesRowMethod(key, plan, method string) bool {
	// Keys are plan.scope.form, optionally followed by registered alternatives.
	rest, ok := strings.CutPrefix(key, plan+".")
	if !ok {
		return false
	}
	_, rest, _ = strings.Cut(rest, ".")
	form, _, _ := strings.Cut(rest, ".")
	switch method {
	case "Count":
		return form == "count"
	case "Find":
		return form == "data"
	case "Exists":
		return form == "exists"
	case "DefineRows", "TryDefineRows":
		return form == "count" || form == "data" || form == "exists"
	case "List":
		return form == "count" || form == "data"
	case "UpdateMany", "DefineUpdateMany", "TryDefineUpdateMany":
		return form == "set"
	case "DeleteMany", "DefineDeleteMany", "TryDefineDeleteMany":
		return form == "delete"
	default:
		return false
	}
}

// Readiness collects minimum privileges and scope requirements from the
// registered shapes bound to role. Pass the result and any application
// requirements to CheckReadiness. Legacy SQL retains its application-owned needs.
func (r RoleStatements) Readiness(role string) (ReadinessNeeds, error) {
	work, ok := r[role]
	if !ok || len(work) == 0 {
		return ReadinessNeeds{}, fmt.Errorf("no workload for role %q", role)
	}
	var n ReadinessNeeds
	var privileges []tabula.DefinedPrivilege
	seenRoutine := make(map[tabula.RoutineRequirement]bool)
	for _, stmt := range work {
		s := stmt.Shape
		if s == nil {
			continue
		}
		for _, routine := range s.Routines {
			if !seenRoutine[routine] {
				n.Routines = append(n.Routines, routine)
				seenRoutine[routine] = true
			}
		}
		privileges = append(privileges, s.Privileges...)
		n.SchemaUsage = append(n.SchemaUsage, s.SchemaUsage...)
		n.ForceRLS = append(n.ForceRLS, s.ForceRLS...)
		n.AnyColumnSelect = append(n.AnyColumnSelect, s.AnyColumnSelect...)
		n.AnyColumnUpdate = append(n.AnyColumnUpdate, s.AnyColumnUpdate...)
	}
	n.Privileges, _ = CollectPrivilegeNeeds([]tabula.Definition{{Name: role, Privileges: privileges}})
	slices.SortStableFunc(n.Routines, func(a, b tabula.RoutineRequirement) int { return strings.Compare(a.Signature, b.Signature) })
	for _, names := range []*[]string{&n.SchemaUsage, &n.ForceRLS, &n.AnyColumnSelect, &n.AnyColumnUpdate} {
		slices.Sort(*names)
		*names = slices.Compact(*names)
	}
	return n, nil
}

// Typed grants use quoted physical names. Retain the table spelling callers
// already use to look up grants on the same descriptor.
func grantTableNames(defs []tabula.Definition) map[string]string {
	if !slices.ContainsFunc(defs, func(d tabula.Definition) bool { return len(d.StatementShapes) != 0 }) {
		return nil
	}
	names := make(map[string]string)
	for _, d := range defs {
		for _, grant := range d.Privileges {
			obj, err := resolveReadinessItem(MissingRequirement{Kind: "TABLE", Object: grant.Table})
			if err == nil {
				names[quoteIdent(obj.Schema)+"."+quoteIdent(obj.Name)] = grant.Table
			}
		}
	}
	return names
}
