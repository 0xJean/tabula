package tabula

import (
	"context"
	"fmt"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// Fixed descriptor operations and typed plans use the same driver credential.
// Only declaration code can attach a shape. Observation metadata looked up by
// SQL text never carries a shape.
func (d *descriptor[T, S]) fixedShape(key, query string, method CallMethod, effects ShapeEffects) *compiledShape {
	return &compiledShape{
		authority: d.reg.statementIssuer, key: key, sql: query,
		tableName: d.name, action: key, observationKey: d.name + "." + key,
		fixedMethod: method, shape: StatementShape{Effects: effects},
	}
}

func (t *Table[T, K, S]) registerFixedStatements(stmts, owners map[string]string) map[string]string {
	if !t.reg.SQLAuthority().DeniesLegacySQL() {
		return owners
	}
	if t.mode.allows(selectOp) {
		order := ""
		if t.order != "" {
			order = " ORDER BY " + t.order
		}
		if owners == nil {
			owners = make(map[string]string)
		}
		addReads := func(prefix string, scoped bool) {
			where := t.liveFilter(scoped, nil).Where()
			stmts[prefix+"count"] = t.stmt.count + where
			stmts[prefix+"list"] = t.stmt.list + where + order
			stmts[prefix+"exists"] = "SELECT EXISTS (SELECT 1 FROM " + t.name + " WHERE true" + where + ")"
			owners[prefix+"exists"] = "exists"
			for _, col := range t.groupByOrder {
				owners[prefix+"count_by_"+col] = "countby:" + col
				stmts[prefix+"count_by_"+col] = "SELECT " + col + ", count(*) FROM " + t.name + " WHERE true" + where + " GROUP BY " + col
			}
		}
		addReads("", t.scope >= 0)
		if t.scope >= 0 {
			addReads("unscoped_", false)
		}
	}
	t.fixed = make(map[string]*compiledShape, len(stmts))
	for key, query := range stmts {
		method, effects := CallScan, EffectRead
		op := strings.TrimPrefix(key, "unscoped_")
		switch {
		case op == "insert":
			method, effects = CallExec, EffectWrite
			if t.serial && t.mode.allows(selectOp) {
				method = CallReturning
			}
		case op == "update" || op == "delete":
			method, effects = CallExec, EffectWrite
		case op == "list" || strings.HasPrefix(op, "count_by_"):
			method = CallEach
		case op == "lock":
			effects |= EffectRowLock
		}
		t.fixed[key] = t.fixedShape(key, query, method, effects)
	}
	return owners
}

func (r *Rows[T, K, S]) fixedMeta(key string, scopedVariant bool) stmtMeta {
	meta := stmtMeta{key: r.t.name + "." + key}
	if r.authorized == nil {
		return meta
	}
	if scopedVariant && r.t.scope >= 0 {
		if r.s.Unscoped() {
			key = "unscoped_" + key
		} else {
			meta.secret = r.t.cols[r.t.scope].secret
		}
	}
	switch strings.TrimPrefix(key, "unscoped_") {
	case "get", "lock", "delete":
		for _, i := range r.t.keys {
			meta.secret = meta.secret || r.t.cols[i].secret
		}
	}
	meta.key, meta.fixed = r.t.name+"."+key, r.t.fixed[key]
	return meta
}

func (b *readBinding[T, S]) checkFixed(meta stmtMeta) error {
	if meta.fixed == nil {
		return b.rejectDynamic()
	}
	return b.bindErr
}

func (b *readBinding[T, S]) rejectDynamic() error {
	if b.bindErr != nil {
		return b.bindErr
	}
	if b.authorized != nil {
		return ErrLegacySQL
	}
	return nil
}

type statementExecutor interface {
	Exec(context.Context, string, ...any) (int64, error)
	Scan(context.Context, string, []any, ...any) error
	Each(context.Context, string, []any, func(func(...any) error) error) error
	Returning(context.Context, string, []any, ...any) error
}

func (b *readBinding[T, S]) executor(meta stmtMeta, secret []bool) statementExecutor {
	if b.authorized == nil {
		return b.s
	}
	return fixedExecutor{b.s, b.authorized, meta, secret}
}

type fixedExecutor struct {
	owner  any
	driver AuthorizedSession
	meta   stmtMeta
	secret []bool
}

func (e fixedExecutor) call(method CallMethod, query string, args []any) (AuthorizedStatement, error) {
	shape := e.meta.fixed
	if shape == nil {
		return AuthorizedStatement{}, ErrLegacySQL
	}
	if shape.sql != query || shape.fixedMethod != method {
		return AuthorizedStatement{}, ErrStatementAuthority
	}
	return AuthorizedStatement{method: method, shape: shape, owner: e.owner, args: args, secret: e.secret, quiet: e.meta.quiet, allSecret: e.meta.secret}, nil
}

func (e fixedExecutor) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	call, err := e.call(CallExec, query, args)
	if err != nil {
		return 0, err
	}
	return e.driver.ExecAuthorized(ctx, call)
}

func (e fixedExecutor) Scan(ctx context.Context, query string, args []any, dest ...any) error {
	call, err := e.call(CallScan, query, args)
	if err != nil {
		return err
	}
	return e.driver.ScanAuthorized(ctx, call, dest...)
}

func (e fixedExecutor) Each(ctx context.Context, query string, args []any, visit func(func(...any) error) error) error {
	call, err := e.call(CallEach, query, args)
	if err != nil {
		return err
	}
	return e.driver.EachAuthorized(ctx, call, visit)
}

func (e fixedExecutor) Returning(ctx context.Context, query string, args []any, dest ...any) error {
	call, err := e.call(CallReturning, query, args)
	if err != nil {
		return err
	}
	return e.driver.ScanAuthorized(ctx, call, dest...)
}

// The original descriptor grammar accepts verbatim table, column and order
// strings. A registry issuing credentials accepts only identifiers and a
// default order over declared columns on these paths.
func (t *Table[T, K, S]) checkFixedIdentifiers() error {
	relation, err := pgstmt.ScanFragment(t.name)
	if err != nil || !fixedRelationName(relation) {
		return fmt.Errorf("%w: expected a table identifier", ErrLegacySQL)
	}
	columns := make(map[string]bool, len(t.cols))
	for _, col := range t.cols {
		tokens, err := pgstmt.ScanFragment(col.name)
		if err != nil || len(tokens) != 1 || !typedIdent(catalogIdentifier(tokens[0])) {
			return fmt.Errorf("%w: expected a column identifier", ErrLegacySQL)
		}
		name := catalogIdentifier(tokens[0])
		if columns[name] {
			return fmt.Errorf("%w: duplicate physical column %s", ErrLegacySQL, name)
		}
		columns[name] = true
	}
	if t.order == "" {
		return nil
	}
	terms, err := pgstmt.ScanFragment(t.order)
	if err != nil || len(terms) == 0 {
		return fmt.Errorf("%w: invalid default order", ErrLegacySQL)
	}
	for len(terms) > 0 {
		col := terms[0]
		if !columns[catalogIdentifier(col)] {
			return fmt.Errorf("%w: default order needs a declared column", ErrLegacySQL)
		}
		terms = terms[1:]
		if len(terms) > 0 && (terms[0].Text == "asc" || terms[0].Text == "desc") {
			terms = terms[1:]
		}
		if len(terms) >= 2 && terms[0].Text == "nulls" && (terms[1].Text == "first" || terms[1].Text == "last") {
			terms = terms[2:]
		}
		if len(terms) == 0 {
			return nil
		}
		if len(terms) < 2 || terms[0].Text != "," {
			return fmt.Errorf("%w: invalid default order", ErrLegacySQL)
		}
		terms = terms[1:]
	}
	return nil
}

func fixedRelationName(t []pgstmt.Span) bool {
	switch len(t) {
	case 1:
		return typedIdent(catalogIdentifier(t[0]))
	case 3:
		return t[1].Text == "." && typedIdent(catalogIdentifier(t[0])) && typedIdent(catalogIdentifier(t[2]))
	default:
		return false
	}
}

// A default page and exact count have a fixed shape. Other options are
// available through finite typed plans on strict registries.
func fixedListing(l Listing) bool {
	return (l.Count == CountExact || l.Count == CountNone) && l.Order == "" && l.Size == 0 && l.Offset == 0 && l.Lock == LockNone
}
