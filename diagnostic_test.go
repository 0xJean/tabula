package tabula

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

type namedDiagnostic struct {
	pgStateErr
	Constraint string
}

type accessorDiagnostic struct {
	namedDiagnostic
	name string
}

func (e accessorDiagnostic) ConstraintName() string { return e.name }

type privateDiagnostic struct {
	pgStateErr
	cause error
}

func (e privateDiagnostic) Unwrap() error { return e.cause }

type diagnosticProvider struct{ diagnostic namedDiagnostic }

func (diagnosticProvider) Error() string { return "operation failed" }

func (e diagnosticProvider) As(target any) bool {
	// Match the public structural SQLState interface used by existing errors.
	slot, ok := target.(*interface {
		error
		SQLState() string
	})
	if ok {
		*slot = e.diagnostic
	}
	return ok
}

func TestConstraintDiagnosticSelection(t *testing.T) {
	first := &namedDiagnostic{pgStateErr{"23505", "details"}, `Mixed "Name"`}
	second := namedDiagnostic{pgStateErr{"23505", "details"}, "later"}
	foreign := namedDiagnostic{pgStateErr{"23503", "details"}, "reference"}
	absent := pgStateErr{"23505", `violates constraint "from message"`}
	var typedNil *namedDiagnostic
	for _, tc := range []struct {
		err         error
		state, name string
	}{
		{nil, "", ""},
		{typedNil, "", ""},
		{fmt.Errorf("operation: %w", typedNil), "", ""},
		{errors.Join(typedNil, second), "", ""},
		{first, "23505", first.Constraint},
		{diagnosticProvider{*first}, "23505", first.Constraint},
		{fmt.Errorf("%w: %w", ErrCommit, first), "23505", first.Constraint},
		{errors.Join(errors.New("operation"), first, second), "23505", first.Constraint},
		{errors.Join(absent, second), "23505", ""},
		{errors.Join(absent, foreign), "23505", ""},
		{errors.Join(second, foreign, first), "23505", second.Constraint},
		{errors.Join(foreign, first), "23503", foreign.Constraint},
		{privateDiagnostic{absent, first}, "23505", ""},
		{accessorDiagnostic{*first, "explicit"}, "23505", "explicit"},
		{accessorDiagnostic{*first, ""}, "23505", ""},
		{namedDiagnostic{pgStateErr{"invalid", "hidden"}, "hidden"}, "", ""},
	} {
		require.Equal(t, tc.state, SQLState(tc.err))
		require.Equal(t, tc.name, ConstraintName(tc.err))
	}
	redacted := (TraceInfo{}).traceError(t.Context(), first)
	require.Equal(t, "23505", SQLState(redacted))
	require.Empty(t, ConstraintName(redacted))
	require.Equal(t, first.Constraint, ConstraintName((TraceInfo{RawErrorDiagnostics: true}).traceError(t.Context(), first)))
}
