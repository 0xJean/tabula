package v04

import (
	"context"
	"errors"
	"iter"
	"strconv"
	"testing"
	"time"

	"github.com/0xJean/tabula"
	"github.com/stretchr/testify/require"
)

// Model the output of compilation for one list. Neither the input SQL grammar
// nor the public filter, page or cursor API is needed to run these statements.
type note struct {
	ID      int64  `db:"id"`
	Rank    int64  `db:"rank"`
	Due     bool   `db:"due"`
	Caption string `db:"caption"`
}

func (n note) cursor() []any { return []any{n.Rank, n.ID} }

type variant struct{ scoped, search bool }
type statements struct{ find, count, page, first, after string }

func slot(n int) string { return "$" + strconv.Itoa(n) }

func (v variant) predicates(n int) (string, int) {
	where := "\nWHERE (n.visible OR n.pinned)"
	if v.scoped {
		n++
		where += "\n  AND (n.scope_id = " + slot(n) + ")"
	}
	if v.search {
		n++
		where += "\n  AND (n.title ILIKE " + slot(n) + " OR n.code ILIKE " + slot(n) + ")"
	}
	return where, n
}

func (v variant) emittedSQL() statements {
	const projection = `SELECT n.id, n.rank, n.due_at <= $1::date AS due, coalesce(l.caption, '') AS caption
FROM pg_temp.notes n LEFT JOIN pg_temp.labels l ON l.id = n.label_id AND $2::boolean`
	const order = "\nORDER BY n.rank DESC, n.id DESC"
	where, n := v.predicates(2)
	countWhere, _ := v.predicates(0) // neither display parameter occurs in CountBase
	body := projection + where
	return statements{
		find:  body + order,
		count: "SELECT count(*) FROM pg_temp.notes n" + countWhere,
		page:  body + order + " LIMIT " + slot(n+1) + " OFFSET " + slot(n+2),
		first: body + order + " LIMIT " + slot(n+1),
		after: body + "\n  AND ((n.rank, n.id) < (" + slot(n+1) + ", " + slot(n+2) + "))" + order + " LIMIT " + slot(n+3),
	}
}

type noteView struct {
	*tabula.View[note, int64]
	reg *registry[int64]
}
type compiledQuery map[variant]statements

type query struct {
	view noteView
	sql  compiledQuery
}

func (v noteView) DefineQuery(key string, shapes []variant, opts ...tabula.StatementOption) query {
	compiled := make(compiledQuery, len(shapes))
	for _, shape := range shapes {
		stmts := shape.emittedSQL()
		compiled[shape] = stmts
		name := key + ".all_scopes"
		if shape.scoped {
			name = key + ".scoped"
		}
		if shape.search {
			name += ".search"
		}
		v.Statement(name+".find", stmts.find, opts...)
		v.Statement(name+".count", stmts.count, opts...)
		v.Statement(name+".page", stmts.page, opts...)
		v.Statement(name+".first", stmts.first, opts...)
		v.Statement(name+".after", stmts.after, opts...)
	}
	return query{v, compiled}
}

func noteQuery(reg *registry[int64]) query {
	descriptors := tabula.NewRegistry(tabula.RegistryConfig[int64]{})
	view := noteView{tabula.DefineView(descriptors, tabula.ViewSpec[note]{Name: "notes_list"}), reg}
	shapes := []variant{{}, {search: true}, {scoped: true}, {scoped: true, search: true}}
	return view.DefineQuery("list", shapes, tabula.Quiet())
}

type queryRows struct {
	view    *tabula.ViewRows[note, int64]
	session tabula.Session[int64]
	sql     compiledQuery
	err     error
}

func (q query) Bind(s tabula.Session[int64]) queryRows {
	if err := q.view.reg.check(s); err != nil {
		return queryRows{err: err}
	}
	return queryRows{view: q.view.Bind(s), session: s, sql: q.sql}
}

// Sample inputs after request validation. Presence and a bound NULL are distinct.
type inputs struct {
	asOf       time.Time
	showLabels bool
	search     bool
	pattern    any
}

func (r queryRows) bindArgs(in inputs) (sql statements, page, count []any) {
	v := variant{!r.session.Unscoped(), in.search}
	if v.scoped {
		count = append(count, r.session.Scope())
	}
	if v.search {
		count = append(count, in.pattern)
	}
	page = append([]any{in.asOf, in.showLabels}, count...)
	return r.sql[v], page, count
}

const maxLimit = 100

func (r queryRows) List(ctx context.Context, in inputs, size, offset int, mode tabula.CountMode) ([]note, int64, error) {
	if r.err != nil {
		return nil, 0, r.err
	}
	if size < 1 || size > maxLimit || offset < 0 || (mode != tabula.CountExact && mode != tabula.CountNone) {
		return nil, 0, errors.New("invalid offset page")
	}
	sql, args, countArgs := r.bindArgs(in)
	total := int64(-1)
	if mode == tabula.CountExact {
		if err := r.view.Scan(ctx, sql.count, countArgs, &total); err != nil {
			return nil, 0, err
		}
	}
	items, err := r.view.Select(ctx, sql.page, append(args, size, offset)...)
	return items, total, err
}

func (r queryRows) Count(ctx context.Context, in inputs) (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	sql, _, args := r.bindArgs(in)
	var total int64
	err := r.view.Scan(ctx, sql.count, args, &total)
	return total, err
}

func (r queryRows) Find(ctx context.Context, in inputs) ([]note, error) {
	if r.err != nil {
		return nil, r.err
	}
	sql, args, _ := r.bindArgs(in)
	return r.view.Select(ctx, sql.find, args...)
}

func (r queryRows) Iter(ctx context.Context, in inputs) iter.Seq2[note, error] {
	if r.err != nil {
		return failedIter[note](r.err)
	}
	sql, args, _ := r.bindArgs(in)
	return r.view.Iter(ctx, sql.find, args...)
}

func (r queryRows) ListKeyset(ctx context.Context, in inputs, limit int, after []any) (tabula.KeysetPage[note], error) {
	var page tabula.KeysetPage[note]
	if r.err != nil {
		return page, r.err
	}
	if limit < 1 || limit > maxLimit || (len(after) != 0 && len(after) != 2) {
		return page, errors.New("invalid keyset page")
	}
	for _, value := range after {
		if _, ok := value.(int64); !ok {
			return page, errors.New("invalid cursor value")
		}
	}
	sql, args, _ := r.bindArgs(in)
	statement := sql.first
	if len(after) > 0 {
		statement = sql.after
		args = append(args, after...)
	}
	rows, err := r.view.Select(ctx, statement, append(args, limit+1)...)
	if err != nil {
		return page, err
	}
	page.HasMore = len(rows) > limit
	page.Items = rows[:min(len(rows), limit)]
	return page, nil
}

func TestComposedQuery(t *testing.T) {
	q := noteQuery(newRegistry[int64](false, SessionKey{}))
	registered := q.view.Definition().Statements
	require.Len(t, registered, 20)
	require.Contains(t, registered["list.scoped.search.page"], "(n.visible OR n.pinned)\n  AND (n.scope_id = $3)\n  AND (n.title ILIKE $4 OR n.code ILIKE $4)")
	require.Contains(t, registered["list.scoped.search.count"], "(n.scope_id = $1)\n  AND (n.title ILIKE $2 OR n.code ILIKE $2)")
	require.NotContains(t, registered["list.scoped.search.count"], "labels")
	// A nil test session makes any attempted I/O or scope lookup a failure.
	rows := q.Bind(nil)
	_, _, err := rows.List(t.Context(), inputs{}, 1, 0, tabula.CountEstimate)
	require.Error(t, err)
	_, err = rows.ListKeyset(t.Context(), inputs{}, maxLimit+1, nil)
	require.Error(t, err)
	_, err = rows.ListKeyset(t.Context(), inputs{}, 2, []any{nil, int64(1)})
	require.Error(t, err)
}

func TestComposedQueryRequiresSealedSession(t *testing.T) {
	r := noteQuery(newRegistry[int64](true, NewSessionKey())).Bind(&sealedReader{})
	_, _, err := r.List(t.Context(), inputs{}, 0, 0, tabula.CountEstimate)
	require.ErrorIs(t, err, ErrForeignSession)
	_, err = r.ListKeyset(t.Context(), inputs{}, 0, nil)
	require.ErrorIs(t, err, ErrForeignSession)
	_, err = r.Count(t.Context(), inputs{})
	require.ErrorIs(t, err, ErrForeignSession)
	_, err = r.Find(t.Context(), inputs{})
	require.ErrorIs(t, err, ErrForeignSession)
	var emitted int
	for _, err := range r.Iter(t.Context(), inputs{}) {
		require.ErrorIs(t, err, ErrForeignSession)
		emitted++
	}
	require.Equal(t, 1, emitted)
}
