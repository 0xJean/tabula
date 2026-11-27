package plan

import (
	"context"
	"time"

	"github.com/0xJean/tabula"
)

type customTenant string

type fakeSession struct {
	tenant   customTenant
	unscoped bool

	lastQuery string
	lastArgs  []any
	execRows  int64
	scanFn    func(dest ...any) error
	eachRows  [][]any
}

func (f *fakeSession) Scope() customTenant { return f.tenant }
func (f *fakeSession) Unscoped() bool      { return f.unscoped }
func (f *fakeSession) Exec(_ context.Context, query string, args ...any) (int64, error) {
	f.lastQuery, f.lastArgs = query, args
	return f.execRows, nil
}
func (f *fakeSession) Scan(_ context.Context, query string, args []any, dest ...any) error {
	f.lastQuery, f.lastArgs = query, args
	if f.scanFn != nil {
		return f.scanFn(dest...)
	}
	return nil
}
func (f *fakeSession) Returning(_ context.Context, query string, args []any, dest ...any) error {
	f.lastQuery, f.lastArgs = query, args
	if f.scanFn != nil {
		return f.scanFn(dest...)
	}
	return nil
}
func (f *fakeSession) Each(_ context.Context, query string, args []any, decode func(func(...any) error) error) error {
	f.lastQuery, f.lastArgs = query, args
	for _, r := range f.eachRows {
		if err := decode(func(dest ...any) error {
			for i := range dest {
				switch d := dest[i].(type) {
				case *int64:
					*d = r[i].(int64)
				case *customTenant:
					*d = r[i].(customTenant)
				case *string:
					*d = r[i].(string)
				case *int:
					*d = r[i].(int)
				case **time.Time:
					if r[i] != nil {
						*d = r[i].(*time.Time)
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

type fakeTracer struct {
	spans []struct {
		info tabula.TraceInfo
		rows int64
		err  error
	}
}

type traceCtxKey struct{}

func (f *fakeTracer) Start(ctx context.Context, info tabula.TraceInfo) (context.Context, func(int64, error)) {
	ctx = context.WithValue(ctx, traceCtxKey{}, info.Key)
	return ctx, func(rows int64, err error) {
		f.spans = append(f.spans, struct {
			info tabula.TraceInfo
			rows int64
			err  error
		}{info: info, rows: rows, err: err})
	}
}

type pgStateErr struct {
	code string
	msg  string
}

func (e pgStateErr) Error() string    { return e.msg }
func (e pgStateErr) SQLState() string { return e.code }

type namedDiagnostic struct {
	pgStateErr
	Constraint string
}

func tableDef[S any](reg *tabula.Registry[S], name string) tabula.Definition {
	for _, d := range reg.Defined() {
		if d.Name == name {
			return d
		}
	}
	return tabula.Definition{}
}
