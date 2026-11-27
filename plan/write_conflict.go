package plan

import (
	"github.com/0xJean/tabula"

	"fmt"
	"slices"
	"strings"
)

// ConflictTarget identifies inferred columns or a named catalog constraint.
// Partial inference predicates use the separate closed IndexPredicate grammar.
type ConflictTarget struct {
	columns      []RowColumn
	constraint   string
	predicate    IndexPredicate
	predicateSet bool
	declared     bool
}

// ConflictColumns selects an immediate unique index inferred by PostgreSQL.
// Readiness checks its actual EXPLAIN arbiter and catalog properties.
func ConflictColumns(first RowColumn, rest ...RowColumn) ConflictTarget {
	return ConflictTarget{columns: slices.Concat([]RowColumn{first}, rest), declared: true}
}

// ConflictConstraint selects a named constraint on the insertion target.
// The name is an identifier, never SQL text.
func ConflictConstraint(name string) ConflictTarget {
	return ConflictTarget{constraint: name, declared: true}
}

// WhereIndex adds a fixed schema predicate for partial-index inference. It
// accepts no request inputs; ordinary runtime predicates remain fully bound.
func (t ConflictTarget) WhereIndex(predicate IndexPredicate) ConflictTarget {
	t.predicate = predicate
	t.predicateSet = true
	return t
}

type conflictAction struct {
	target ConflictTarget
	mode   string
	set    []RowAssignment
	where  []Pred
}

// OnConflict selects an inference target. Follow it with DoNothing or DoUpdate.
func (q InsertStatement) OnConflict(target ConflictTarget) InsertStatement {
	target.declared = true
	q.node.conflict.target = target
	return q
}

// DoNothing ignores conflicts. With no OnConflict target it retains the
// INSERT-only path, requiring no SELECT privilege on the target.
func (q InsertStatement) DoNothing() InsertStatement {
	q.node.conflict.mode = "NOTHING"
	q.node.conflict.set = nil
	return q
}

// DoUpdate updates conflicts using one fixed ordered assignment list. It
// requires an explicit immediate unique arbiter and preserves protected fields.
func (q InsertStatement) DoUpdate(first RowAssignment, rest ...RowAssignment) InsertStatement {
	q.node.conflict.mode = "UPDATE"
	q.node.conflict.set = slices.Concat([]RowAssignment{first}, rest)
	return q
}

// UpdateWhere restricts the conflict update. A false/NULL condition leaves the
// existing row untouched and produces no RETURNING row for that input.
func (q InsertStatement) UpdateWhere(first Pred, rest ...Pred) InsertStatement {
	q.node.conflict.where = slices.Concat(q.node.conflict.where, []Pred{first}, rest)
	return q
}

type excludedReference struct{ field *fieldRef }

// Excluded reads the proposed value of a target column after BEFORE INSERT
// triggers. It is legal only inside that exact target's ON CONFLICT update arm.
func Excluded[V any](field Field[V]) Expr[V] {
	return Expr[V]{part: excludedReference{field.ref}, kind: field.kind,
		nullable: field.ref == nil || field.ref.nullable}
}

func (e excludedReference) expressionTraits() expressionTraits {
	return expressionTraits{identity: "excluded", children: []expressionPart{e.field}}
}

func (e excludedReference) nullableAfterJoin(map[*occurrence]bool) bool {
	return e.field == nil || e.field.nullable
}

func (e excludedReference) render(c *typedCompiler) (string, error) {
	if e.field == nil || c.conflictTarget == nil || e.field.source != c.conflictTarget {
		return "", fmt.Errorf("tabula: EXCLUDED requires the exact ON CONFLICT target")
	}
	if c.block != nil {
		if err := c.block.resolvePhysical(e.field.source, 0); err != nil {
			return "", err
		}
	}
	if err := c.read(e.field); err != nil {
		return "", err
	}
	return "excluded." + typedQuote(e.field.name), nil
}

func (n writeDeclaration) renderConflict(c *typedCompiler, meta *tabula.StatementShape) (string, error) {
	a := n.conflict
	if a.mode == "" {
		if a.target.declared || len(a.where) != 0 {
			return "", fmt.Errorf("tabula: ON CONFLICT requires DoNothing or DoUpdate")
		}
		return "", nil
	}
	if len(a.where) != 0 && a.mode != "UPDATE" {
		return "", fmt.Errorf("tabula: UpdateWhere requires DoUpdate")
	}
	target, requirement, err := n.conflictTarget(c)
	if err != nil {
		return "", err
	}
	meta.Conflicts = append(meta.Conflicts, requirement)
	text := " ON CONFLICT" + target + " DO " + a.mode
	if a.mode == "NOTHING" {
		return text, nil
	}
	if !a.target.declared {
		return "", fmt.Errorf("tabula: DoUpdate requires a target")
	}
	if err := n.target.rel.checkCommand("UPDATE"); err != nil {
		return "", err
	}
	prior := c.conflictTarget
	c.conflictTarget = n.target.rel
	defer func() { c.conflictTarget = prior }()
	set, err := n.renderAssignments(c, a.set, meta)
	if err != nil {
		return "", err
	}
	// Existing scope/liveness/version guards apply equally to conflict updates.
	terms, err := n.mutationWhere(c, a.where)
	if err != nil {
		return "", err
	}
	meta.Access = append(meta.Access, tabula.ShapeAccess{Relation: n.target.rel.table.SQL(), Command: "UPDATE"})
	return text + " SET " + set + whereSQL(terms), nil
}

func (n writeDeclaration) conflictTarget(c *typedCompiler) (string, tabula.ConflictRequirement, error) {
	t := n.conflict.target
	req := tabula.ConflictRequirement{Relation: n.target.rel.table.SQL(), DoUpdate: n.conflict.mode == "UPDATE"}
	if !t.declared {
		return "", req, nil
	}
	if t.constraint != "" {
		if !typedIdent(t.constraint) || len(t.columns) != 0 || t.predicateSet {
			return "", req, fmt.Errorf("tabula: invalid named conflict constraint")
		}
		req.Constraint = t.constraint
		return " ON CONSTRAINT " + typedQuote(t.constraint), req, nil
	}
	if len(t.columns) == 0 {
		return "", req, fmt.Errorf("tabula: conflict inference requires target columns")
	}
	if t.predicateSet && t.predicate.node == nil {
		return "", req, fmt.Errorf("tabula: undeclared index predicate")
	}
	var columns []string
	for _, col := range t.columns {
		f := col.ref
		if f == nil || f.source != n.target.rel || slices.Contains(req.Columns, f.name) {
			return "", req, fmt.Errorf("tabula: conflict inference requires distinct target columns")
		}
		if err := c.read(f); err != nil {
			return "", req, err
		}
		req.Columns = append(req.Columns, f.name)
		columns = append(columns, typedQuote(f.name))
	}
	text := " (" + strings.Join(columns, ", ") + ")"
	if t.predicate.node != nil {
		predicate, err := t.predicate.node.renderIndex(c, n.target.rel)
		if err != nil {
			return "", req, err
		}
		text += " WHERE " + predicate
	}
	return text, req, nil
}
