package sqlcheck

import (
	"fmt"
	"regexp"
	"slices"
)

// PolicyRule matches the conjunction of its nonempty fields. Roles is an exact
// set; Using and Check are Go regular expressions over server-deparsed SQL.
// Expression and NotExpression match (or exclude) the combined USING and WITH
// CHECK predicates (Using + "\n" + Check). Entries, when present, restricts the
// rule to registered policy identities.
type PolicyRule struct {
	Name, Command, Using, Check string
	Expression, NotExpression   string `json:",omitempty"`
	Permissive                  *bool
	Roles                       []string
	Entries                     []PolicyID
	// Patterns match individual identifiers. UnregisteredOnly excludes policy
	// identities explicitly registered in any rule's Entries.
	SchemaPattern, TablePattern, NamePattern string
	UnregisteredOnly                         bool
}

// PolicyClass records the unique matching rule for a policy.
type PolicyClass struct {
	PolicyID
	Rule string
}

// ClassifyPolicies requires exactly one rule per policy and reports every
// registered identity that no longer satisfies its rule. It has no built-in roles.
func ClassifyPolicies(policies []Policy, rules []PolicyRule) ([]PolicyClass, []string, error) {
	type rule struct {
		PolicyRule
		using, check, expr, notExpr *regexp.Regexp
		schema, table, name         *regexp.Regexp
		entries                     map[PolicyID]bool
	}
	var compiled []rule
	names := map[string]bool{}
	registered := map[PolicyID]bool{}
	for _, r := range rules {
		if r.Name == "" || names[r.Name] {
			return nil, nil, fmt.Errorf("empty or duplicate policy rule %q", r.Name)
		}
		names[r.Name] = true
		if r.Command != "" && !slices.Contains([]string{"ALL", "SELECT", "INSERT", "UPDATE", "DELETE"}, r.Command) {
			return nil, nil, fmt.Errorf("unknown policy command %q", r.Command)
		}
		u, err := regexp.Compile(r.Using)
		if err != nil {
			return nil, nil, err
		}
		c, err := regexp.Compile(r.Check)
		if err != nil {
			return nil, nil, err
		}
		exprRE, err := regexp.Compile(r.Expression)
		if err != nil {
			return nil, nil, err
		}
		var notExprRE *regexp.Regexp
		if r.NotExpression != "" {
			notExprRE, err = regexp.Compile(r.NotExpression)
			if err != nil {
				return nil, nil, err
			}
		}
		var selectors [3]*regexp.Regexp
		for i, pattern := range []string{r.SchemaPattern, r.TablePattern, r.NamePattern} {
			selectors[i], err = regexp.Compile(pattern)
			if err != nil {
				return nil, nil, err
			}
		}
		if r.Roles != nil && len(r.Roles) == 0 {
			return nil, nil, fmt.Errorf("empty role set for %s", r.Name)
		}
		if r.UnregisteredOnly && r.Entries != nil {
			return nil, nil, fmt.Errorf("%s cannot match both registered and unregistered policies", r.Name)
		}
		r.Roles = slices.Sorted(slices.Values(r.Roles))
		for i, v := range r.Roles {
			if v == "" || i > 0 && v == r.Roles[i-1] {
				return nil, nil, fmt.Errorf("invalid role %q in %s", v, r.Name)
			}
		}
		entries := map[PolicyID]bool{}
		for _, id := range r.Entries {
			if _, ok := entries[id]; ok || id.Schema == "" || id.Table == "" || id.Name == "" {
				return nil, nil, fmt.Errorf("invalid or duplicate policy entry %+v", id)
			}
			entries[id] = false
			registered[id] = true
		}
		compiled = append(compiled, rule{r, u, c, exprRE, notExprRE, selectors[0], selectors[1], selectors[2], entries})
	}
	var classes []PolicyClass
	var findings []string
	for _, p := range policies {
		var matches []string
		roles := slices.Sorted(slices.Values(p.Roles))
		combined := p.Using + "\n" + p.Check
		for _, r := range compiled {
			if r.Command != "" && r.Command != p.Command || r.Permissive != nil && *r.Permissive != p.Permissive || len(r.Roles) > 0 && !slices.Equal(r.Roles, roles) {
				continue
			}
			if !r.using.MatchString(p.Using) || !r.check.MatchString(p.Check) || !r.expr.MatchString(combined) || (r.notExpr != nil && r.notExpr.MatchString(combined)) || !r.schema.MatchString(p.Schema) || !r.table.MatchString(p.Table) || !r.name.MatchString(p.Name) {
				continue
			}
			if r.UnregisteredOnly && registered[p.PolicyID] {
				continue
			}
			if r.Entries != nil {
				if _, ok := r.entries[p.PolicyID]; !ok {
					continue
				}
				r.entries[p.PolicyID] = true
			}
			matches = append(matches, r.Name)
		}
		if len(matches) == 1 {
			classes = append(classes, PolicyClass{p.PolicyID, matches[0]})
		} else {
			slices.Sort(matches)
			findings = append(findings, fmt.Sprintf("%s.%s.%s: matched %d policy rules %v", p.Schema, p.Table, p.Name, len(matches), matches))
		}
	}
	for _, r := range compiled {
		for id, seen := range r.entries {
			if !seen {
				findings = append(findings, fmt.Sprintf("%s: stale policy entry %s.%s.%s", r.Name, id.Schema, id.Table, id.Name))
			}
		}
	}
	slices.Sort(findings)
	return classes, findings, nil
}
