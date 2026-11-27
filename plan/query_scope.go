package plan

import "fmt"

// queryOccurrence is a result occurrence, never a physical table declaration.
// Its node supplies physical leaves; alias and field names cannot create grants.
type queryOccurrence struct {
	alias  string
	node   statementNode
	fields []queryResultField
}

// queryBlock is a lexical environment. A child sees an ancestor only through
// an explicit depth, so matching spellings cannot capture another occurrence.
// Join renderers construct visibility incrementally in declaration order.
type queryBlock struct {
	parent          *queryBlock
	issuer          *statementIssuer
	physical        map[*occurrence]bool
	results         map[*queryOccurrence]bool
	aliases         map[string]bool
	physicalAliases map[*occurrence]string
	resultAliases   map[*queryOccurrence]string
	resultFields    map[*queryOccurrence][]queryResultField
	resultKeys      map[*queryOccurrence][]queryResultKey
	outerPhysical   map[*occurrence]bool
	outerResults    map[*queryOccurrence]bool
	common          map[*commonTableNode]statementResult
	recursive       *commonTableNode
	selfUses        int
}

func newQueryBlock(parent *queryBlock, issuer *statementIssuer) (*queryBlock, error) {
	if issuer == nil || parent != nil && parent.issuer != issuer {
		return nil, fmt.Errorf("tabula: query block requires one registry")
	}
	return &queryBlock{
		parent: parent, issuer: issuer, physical: make(map[*occurrence]bool),
		results: make(map[*queryOccurrence]bool), aliases: make(map[string]bool),
		physicalAliases: make(map[*occurrence]string), resultAliases: make(map[*queryOccurrence]string),
		resultFields: make(map[*queryOccurrence][]queryResultField), resultKeys: make(map[*queryOccurrence][]queryResultKey),
		outerPhysical: make(map[*occurrence]bool), outerResults: make(map[*queryOccurrence]bool),
		common: make(map[*commonTableNode]statementResult),
	}, nil
}

func (b *queryBlock) addPhysical(src *occurrence) error {
	if b == nil || src == nil || src.issuer != b.issuer || src.entry == nil {
		return fmt.Errorf("tabula: query refers to an undeclared or foreign physical occurrence")
	}
	if err := b.addAlias(src.alias); err != nil {
		return err
	}
	b.physical[src] = true
	return nil
}

func (b *queryBlock) addResult(src *queryOccurrence) error {
	if b == nil || src == nil || isNilBound(src.node) || len(src.fields) == 0 {
		return fmt.Errorf("tabula: query refers to an undeclared result occurrence")
	}
	names := make(map[string]bool)
	identities := make(map[*queryFieldIdentity]bool)
	for _, field := range src.fields {
		if field.id == nil || !typedIdent(field.name) || field.value.kind.GoType == nil || names[field.name] || identities[field.id] {
			return fmt.Errorf("tabula: invalid or duplicate exported result field %q", field.name)
		}
		names[field.name], identities[field.id] = true, true
	}
	if err := b.addAlias(src.alias); err != nil {
		return err
	}
	b.results[src] = true
	return nil
}

func (b *queryBlock) addAlias(alias string) error {
	if !typedIdent(alias) || b.aliases[alias] {
		return fmt.Errorf("tabula: invalid or duplicate query alias %q", alias)
	}
	b.aliases[alias] = true
	return nil
}

func (b *queryBlock) ancestor(depth int) (*queryBlock, error) {
	if depth < 0 {
		return nil, fmt.Errorf("tabula: invalid outer query depth")
	}
	for range depth {
		if b == nil {
			break
		}
		b = b.parent
	}
	if b == nil {
		return nil, fmt.Errorf("tabula: outer reference escapes its query")
	}
	return b, nil
}

func (b *queryBlock) resolvePhysical(src *occurrence, depth int) error {
	outer, err := b.ancestor(depth)
	if err != nil {
		return err
	}
	if src == nil || !outer.physical[src] {
		return fmt.Errorf("tabula: physical occurrence is not visible at query depth %d", depth)
	}
	return nil
}

func (b *queryBlock) resolveResult(src *queryOccurrence, field *queryFieldIdentity, depth int) (queryResultField, error) {
	outer, err := b.ancestor(depth)
	if err != nil {
		return queryResultField{}, err
	}
	if src == nil || field == nil || !outer.results[src] {
		return queryResultField{}, fmt.Errorf("tabula: result occurrence is not visible at query depth %d", depth)
	}
	fields := src.fields
	if compiled, ok := outer.resultFields[src]; ok {
		fields = compiled
	}
	for _, found := range fields {
		if found.id == field {
			return cloneQueryFields([]queryResultField{found})[0], nil
		}
	}
	return queryResultField{}, fmt.Errorf("tabula: exported field does not belong to the result occurrence")
}
