package tabula

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSingleFormRegistration(t *testing.T) {
	reg := NewRegistry(RegistryConfig[customTenant]{})
	table := DefinePlanSource(reg, TableSpec[typedItem, int64]{Name: "public.items", RelationScope: ByRLS()})
	rel := FromTable(table, "i", ByRLS())
	src := RowsFrom(table, rel)
	out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	id := FieldOf(rel, "id", Int8())
	project := Project(Output(out, "id", Int8()), id.Expr())
	filter := RowFilter{Optional: []RowPredicate{{Name: "by_id", When: Eq(id.Expr(), Input("id", Int8()).Expr())}}}
	ordered := RowsSpec{RowFilter: filter, Orders: []RowOrder{{Name: "forward", Terms: []RowTerm{Asc(id)}}}}
	ordered.Budget = 6
	both := ReadForms{Exact: true, First: OrderedFirst}
	DefineRowsWithForms(out, "ordered", src, ordered, both, project)
	before := out.Definition()
	require.Len(t, before.StatementShapes, 6)
	single, data := "ordered.rls.single_first.sort.forward.where.by_id", "ordered.rls.data.sort.forward.where.by_id"
	require.Equal(t, before.Statements[data]+` LIMIT CAST($2 AS "pg_catalog"."int8")`, before.Statements[single])
	want := before.StatementShapes[data]
	want.Inputs = append(want.Inputs, ShapeInput{Name: "@take", Type: Int8().id.qualified()})
	require.Equal(t, want, before.StatementShapes[single])

	base := Select(project).From(src)
	for _, tc := range []struct {
		name string
		mode ReadForms
		want string
	}{
		{"exact", ReadForms{Exact: true}, "single"},
		{"first", ReadForms{First: UnorderedFirst}, "firstrow"},
		{"both", ReadForms{Exact: true, First: UnorderedFirst}, "single_first"},
	} {
		DefineRead(out, tc.name, base.WithForms(tc.mode))
		d := out.Definition()
		require.Equal(t, d.Statements[tc.name+".rls.data"]+` LIMIT CAST($1 AS "pg_catalog"."int8")`, d.Statements[tc.name+".rls."+tc.want])
	}
	DefineRead(out, "original", base)
	before = out.Definition()
	require.NotContains(t, before.StatementShapes, "original.rls.single_first")
	ordered.Budget--
	p, err := TryDefineRowsWithForms(out, "budget", src, ordered, both, project)
	require.Nil(t, p)
	require.ErrorContains(t, err, "exceeds 5 shapes")
	for _, invalid := range []ReadForms{{First: UnorderedFirst}, {First: FirstMode(99)}} {
		p, err = TryDefineRowsWithForms(out, "invalid", src, ordered, invalid, project)
		require.Nil(t, p)
		require.Error(t, err)
	}
	j, err := TryDefineRead(out, "invalid", base.WithForms(ReadForms{First: OrderedFirst}))
	require.Nil(t, j)
	require.ErrorContains(t, err, "OrderedFirst")
	require.Equal(t, before, out.Definition())

	// Two existing forms and seven optional filters exactly fill the
	// default budget. The bounded form must participate in the same limit.
	many := RowsSpec{}
	for i := range 7 {
		many.Optional = append(many.Optional, RowPredicate{Name: fmt.Sprintf("filter_%d", i), When: IsNull(id.Expr())})
	}
	p, err = TryDefineRowsWithForms(out, "too_many", src, many, ReadForms{Exact: true}, project)
	require.Nil(t, p)
	require.ErrorContains(t, err, "exceeds 256 shapes")
	require.Equal(t, before, out.Definition())
}

type singleReadSession struct {
	routineSession
	matches, visited, scanned int
	scanErr, finalErr         error
	opened                    []AuthorizedSQL
}

func (s *singleReadSession) EachAuthorized(ctx context.Context, call AuthorizedStatement, visit func(func(...any) error) error) error {
	stmt, err := s.authority.Open(s, CallEach, call)
	if err != nil {
		return err
	}
	s.opened = append(s.opened, stmt)
	s.visited, s.scanned = 0, 0
	n := s.matches
	if strings.Contains(stmt.Query, " LIMIT ") {
		n = min(n, int(stmt.Args[len(stmt.Args)-1].(int64)))
	}
	for i := range n {
		s.visited++
		err := visit(func(dest ...any) error {
			s.scanned++
			*dest[0].(*int64) = int64(i + 1)
			return s.scanErr
		})
		if err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return s.finalErr
}

func TestSingleReadSelectionAndErrors(t *testing.T) {
	ctx := t.Context()
	mapped := errors.New("read failed")
	reg := NewRegistry(RegistryConfig[customTenant]{Errors: ErrorPolicy{Classify: func(_ context.Context, err error) error {
		return errors.Join(mapped, err)
	}}})
	table := DefinePlanSource(reg, TableSpec[typedItem, int64]{Name: "public.items", RelationScope: Global()})
	rel := FromTable(table, "i", Global())
	src := RowsFrom(table, rel)
	out := DefineView(reg, ViewSpec[typedResult]{Name: "entries"})
	id := FieldOf(rel, "id", Int8())
	project := Project(Output(out, "id", Int8()), id.Expr())
	s := &singleReadSession{routineSession: routineSession{authority: reg.SQLAuthority()}}
	plain := DefineRows(out, "plain", src, RowsSpec{}, project).Bind(s)
	firstOnly := DefineRowsWithForms(out, "first_only", src, RowsSpec{}, ReadForms{First: UnorderedFirst}, project).Bind(s)
	spec := RowsSpec{Orders: []RowOrder{{Name: "next", Terms: []RowTerm{Asc(id)}}}, Pages: RowPages{Keyset: true, MaxLimit: 1}}
	paged := DefineRows(out, "paged", src, spec, project)
	both := DefineRowsWithForms(out, "bounded", src, spec, ReadForms{Exact: true, First: OrderedFirst}, project).Bind(s)
	ordered := RowChoice{Order: "next"}

	require.ErrorIs(t, ErrNotFound, sql.ErrNoRows)
	one, err := both.One(ctx, ordered)
	require.Zero(t, one)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorIs(t, err, sql.ErrNoRows)
	maybe, found, err := both.MaybeOne(ctx, ordered)
	require.NoError(t, err)
	require.False(t, found)
	require.Zero(t, maybe)
	s.matches = 4
	_, err = both.One(ctx, ordered)
	require.ErrorIs(t, err, ErrMultipleRows)
	require.Equal(t, 2, s.visited)
	last := s.opened[len(s.opened)-1]
	require.Equal(t, "entries.bounded.global.single_first.sort.next", last.Key)
	require.Equal(t, []any{int64(2)}, last.Args)
	first, err := both.First(ctx, ordered)
	require.NoError(t, err)
	require.Equal(t, typedResult{ID: 1}, first)
	require.Equal(t, 1, s.visited)
	optional, found, err := both.MaybeFirst(ctx, ordered)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, first, optional)
	require.Equal(t, last.Key, s.opened[len(s.opened)-1].Key)
	require.Equal(t, []any{int64(1)}, s.opened[len(s.opened)-1].Args)
	for _, b := range []*BoundRowsPlan[typedResult, customTenant]{plain, firstOnly} {
		maybe, found, err = b.MaybeOne(ctx, RowChoice{})
		require.ErrorIs(t, err, ErrMultipleRows)
		require.False(t, found)
		require.Zero(t, maybe)
		require.Equal(t, 2, s.visited)
		require.Equal(t, 1, s.scanned)
		require.NotContains(t, s.opened[len(s.opened)-1].Query, "LIMIT")
	}
	s.matches = 1
	fail := errors.New("scan failure")
	s.scanErr = fail
	maybe, found, err = both.MaybeFirst(ctx, ordered)
	require.ErrorIs(t, err, fail)
	require.ErrorIs(t, err, mapped)
	require.False(t, found)
	require.Zero(t, maybe)
	s.scanErr, s.finalErr = nil, errors.New("late cursor failure")
	for _, n := range []int{0, 1, 2} {
		s.matches = n
		maybe, found, err = both.MaybeOne(ctx, ordered)
		require.ErrorIs(t, err, s.finalErr)
		require.ErrorIs(t, err, mapped)
		require.False(t, found)
		require.Zero(t, maybe)
	}

	before := len(s.opened)
	cursor, err := paged.Cursor("next", At(id, 1))
	require.NoError(t, err)
	for _, choice := range []RowChoice{
		{Order: "next", Page: FirstPage, Limit: 1}, {Order: "next", Limit: 1},
		{Order: "next", Offset: 1}, {Order: "next", After: cursor}, {Order: "missing"}, {},
	} {
		_, err = both.One(ctx, choice)
		require.Error(t, err)
		_, _, err = both.MaybeFirst(ctx, choice)
		require.Error(t, err)
	}
	_, err = both.First(ctx, ordered, BindValue(Input("@take", Int8()), 10))
	require.Error(t, err)
	_, err = plain.First(ctx, RowChoice{})
	require.ErrorIs(t, err, ErrOperationDenied)
	_, err = paged.Bind(s).One(ctx, ordered)
	require.ErrorIs(t, err, ErrOperationDenied)
	lock := ForRows(ForUpdate, rel, SkipLocked)
	locked := DefineRead(out, "locked", Select(project).From(src).Lock(lock)).Bind(s)
	_, _, err = locked.MaybeOne(ctx)
	require.ErrorIs(t, err, ErrOperationDenied)
	s.readOnly = true
	locked = DefineRead(out, "limited_lock", Select(project).From(src).Lock(lock).WithForms(ReadForms{First: UnorderedFirst})).Bind(s)
	_, err = locked.First(ctx)
	require.ErrorIs(t, err, ErrReadOnly)
	require.Len(t, s.opened, before)
}
