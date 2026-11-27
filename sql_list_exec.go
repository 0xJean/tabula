package tabula

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/0xJean/tabula/internal/planrt"
)

// Name returns the registered declaration name.
func (l *SQLList[Row, Args, S]) Name() string { return l.name }

// Definition returns a deep copy of the registered Definition for sqlcheck.
func (l *SQLList[Row, Args, S]) Definition() Definition {
	l.reg.mu.RLock()
	defer l.reg.mu.RUnlock()
	return cloneDefinition(l.entry.def)
}

// Bind attaches this list query to an authorized session from the owning Registry.
func (l *SQLList[Row, Args, S]) Bind(s Session[S]) *BoundSQLList[Row, Args, S] {
	if l == nil || l.reg == nil {
		return &BoundSQLList[Row, Args, S]{binding: sqlSessionBinding[S]{bindErr: ErrStatementAuthority}}
	}
	return &BoundSQLList[Row, Args, S]{l: l, binding: bindSQLSession(l.reg, s)}
}

// BoundSQLList executes a registered SQLList on one authorized session.
type BoundSQLList[Row, Args, S any] struct {
	l       *SQLList[Row, Args, S]
	binding sqlSessionBinding[S]
}

// WithLogger returns a handle with a per-binding logger override.
func (b *BoundSQLList[Row, Args, S]) WithLogger(logger *slog.Logger) *BoundSQLList[Row, Args, S] {
	clone := *b
	clone.binding.logger = logger
	return &clone
}

func (b *BoundSQLList[Row, Args, S]) enabledFilters(args *Args) []int {
	var enabled []int
	for i, f := range b.l.filters {
		if f.when(args) {
			enabled = append(enabled, i)
		}
	}
	return enabled
}

func (b *BoundSQLList[Row, Args, S]) prepareVariant(action string, method CallMethod, built builtSQLListVariant, args *Args, extraVals []any) (AuthorizedStatement, error) {
	if b.binding.bindErr != nil {
		return AuthorizedStatement{}, b.binding.bindErr
	}
	values := make([]any, 0, len(built.slots)+len(extraVals))
	secret := make([]bool, 0, len(built.slots)+len(extraVals))
	for _, slot := range built.slots {
		secret = append(secret, slot.secret)
		if slot.isScope {
			if b.binding.s.Unscoped() {
				return AuthorizedStatement{}, ErrStatementAuthority
			}
			values = append(values, b.l.reg.scopeValue(b.binding.s.Scope()))
			continue
		}
		v, err := b.l.args.fields[slot.fieldIdx].bind(args)
		if err != nil {
			return AuthorizedStatement{}, fmt.Errorf("%s.SQLList %s: encode parameter @%s: %w", b.l.reg.prefix, b.l.name, slot.name, err)
		}
		values = append(values, v)
	}
	for _, ev := range extraVals {
		values = append(values, ev)
		secret = append(secret, b.l.meta.secret)
	}
	shape := &planrt.CompiledShape{
		ObservationKey: b.l.name + "." + action,
		TableName:      b.l.name,
		Action:         action,
		Authority:      b.l.reg.statementIssuer,
		Key:            action,
		SQL:            built.sql,
		Effects:        uint8(built.shape.Effects),
		HasResult:      len(built.shape.Result) != 0,
	}
	return b.binding.authorize(shape, method, values, secret, b.l.meta, b.l.scope.mode == scopeParam)
}

func (b *BoundSQLList[Row, Args, S]) runCount(ctx context.Context, enabled []int, args *Args) (int64, error) {
	built, err := b.l.buildCountVariant(enabled)
	if err != nil {
		return 0, err
	}
	call, err := b.prepareVariant("count", CallScan, built, args, nil)
	if err != nil {
		return 0, err
	}
	var total int64
	_, err = b.binding.runSQL(ctx, b.l.name, "count", b.l.meta, call, func(ctx context.Context) (int64, error) {
		return -1, b.binding.driver.ScanAuthorized(ctx, call, &total)
	})
	return total, err
}

// List reads one offset-paginated page plus optional exact total count.
// CountMode must be CountExact (default) or CountNone (total = -1).
func (b *BoundSQLList[Row, Args, S]) List(ctx context.Context, args Args, page QueryPage) ([]Row, int64, error) {
	if b == nil || b.l == nil {
		return nil, 0, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return nil, 0, b.binding.bindErr
	}
	if page.Size < 1 || page.Size > b.l.maxLimit || page.Offset < 0 {
		return nil, 0, fmt.Errorf("%s.SQLList %s: invalid page size=%d offset=%d (max=%d)", b.l.reg.prefix, b.l.name, page.Size, page.Offset, b.l.maxLimit)
	}
	if b.l.maxOffset > 0 && page.Offset > b.l.maxOffset {
		return nil, 0, fmt.Errorf("%s.SQLList %s: offset %d exceeds MaxOffset %d", b.l.reg.prefix, b.l.name, page.Offset, b.l.maxOffset)
	}
	if page.Count != CountExact && page.Count != CountNone {
		return nil, 0, fmt.Errorf("%s.SQLList %s: unsupported CountMode %d (use CountExact or CountNone)", b.l.reg.prefix, b.l.name, page.Count)
	}
	enabled := b.enabledFilters(&args)
	pageBuilt, err := b.l.buildSelectVariant(enabled, page.Sort, page.Direction, true, false, false)
	if err != nil {
		return nil, 0, err
	}
	pageCall, err := b.prepareVariant("list", CallEach, pageBuilt, &args, []any{page.Size, page.Offset})
	if err != nil {
		return nil, 0, err
	}
	total := int64(-1)
	if page.Count == CountExact {
		total, err = b.runCount(ctx, enabled, &args)
		if err != nil {
			return nil, 0, err
		}
	}
	rows, err := execSQLAll(ctx, &b.binding, b.l.row, b.l.name, "list", b.l.meta, pageCall)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// Count returns the exact number of matching rows before pagination.
func (b *BoundSQLList[Row, Args, S]) Count(ctx context.Context, args Args) (int64, error) {
	if b == nil || b.l == nil {
		return 0, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return 0, b.binding.bindErr
	}
	enabled := b.enabledFilters(&args)
	return b.runCount(ctx, enabled, &args)
}

// All reads all matching rows in the selected order without pagination or COUNT(*).
func (b *BoundSQLList[Row, Args, S]) All(ctx context.Context, args Args, order QueryOrder) ([]Row, error) {
	if b == nil || b.l == nil {
		return nil, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return nil, b.binding.bindErr
	}
	enabled := b.enabledFilters(&args)
	built, err := b.l.buildSelectVariant(enabled, order.Sort, order.Direction, false, false, false)
	if err != nil {
		return nil, err
	}
	call, err := b.prepareVariant("all", CallEach, built, &args, nil)
	if err != nil {
		return nil, err
	}
	return execSQLAll(ctx, &b.binding, b.l.row, b.l.name, "all", b.l.meta, call)
}

// Each streams all matching rows in the selected order to fn.
func (b *BoundSQLList[Row, Args, S]) Each(ctx context.Context, args Args, order QueryOrder, fn func(Row) error) error {
	if b == nil || b.l == nil {
		return ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return b.binding.bindErr
	}
	enabled := b.enabledFilters(&args)
	built, err := b.l.buildSelectVariant(enabled, order.Sort, order.Direction, false, false, false)
	if err != nil {
		return err
	}
	call, err := b.prepareVariant("each", CallEach, built, &args, nil)
	if err != nil {
		return err
	}
	return execSQLEach(ctx, &b.binding, b.l.row, b.l.name, "each", b.l.meta, call, fn)
}

// encodeKeysetCursorValue applies registry type codecs in the same precedence
// order as compileArgBinder (RegisterType before RegistryConfig.DefaultTypes).
func (r *Registry[S]) encodeKeysetCursorValue(colType reflect.Type, val any) (any, error) {
	r.mu.RLock()
	customWrap := r.types[colType]
	def, hasDef := r.defaultTypes[colType]
	r.mu.RUnlock()
	if customWrap != nil {
		ptr := reflect.New(colType)
		ptr.Elem().Set(reflect.ValueOf(val))
		return customWrap(ptr.Interface()), nil
	}
	if hasDef && def.kind.Encoder != nil {
		return def.kind.Encoder.Encode(val)
	}
	return val, nil
}

// ListKeyset reads one cursor-paginated page without OFFSET, fetching Limit+1
// rows to set HasMore and populating Next from the last returned row.
func (b *BoundSQLList[Row, Args, S]) ListKeyset(ctx context.Context, args Args, page QueryKeyset) (KeysetPage[Row], error) {
	if b == nil || b.l == nil {
		return KeysetPage[Row]{}, ErrStatementAuthority
	}
	if b.binding.bindErr != nil {
		return KeysetPage[Row]{}, b.binding.bindErr
	}
	if !b.l.keyset {
		return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: keyset pagination is not enabled on this query", b.l.reg.prefix, b.l.name)
	}
	if page.Limit < 1 || page.Limit > b.l.maxLimit {
		return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: invalid keyset limit=%d (max=%d)", b.l.reg.prefix, b.l.name, page.Limit, b.l.maxLimit)
	}
	cs, _, _, err := b.l.resolveOrder(page.Sort, page.Direction)
	if err != nil {
		return KeysetPage[Row]{}, err
	}
	var extra []any
	if len(page.After) > 0 {
		if len(page.After) != len(cs.terms) {
			return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: After cursor has %d values, expected %d", b.l.reg.prefix, b.l.name, len(page.After), len(cs.terms))
		}
		extra = make([]any, 0, len(page.After)+1)
		for i, val := range page.After {
			if val == nil {
				return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: After[%d] must not be nil", b.l.reg.prefix, b.l.name, i)
			}
			col := b.l.row.cols[cs.terms[i].colIdx]
			gotType := reflect.TypeOf(val)
			if gotType != col.goType {
				return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: After[%d] has type %s, expected %s", b.l.reg.prefix, b.l.name, i, gotType, col.goType)
			}
			encoded, err := b.l.reg.encodeKeysetCursorValue(col.goType, val)
			if err != nil {
				return KeysetPage[Row]{}, fmt.Errorf("%s.SQLList %s: encode After[%d]: %w", b.l.reg.prefix, b.l.name, i, err)
			}
			extra = append(extra, encoded)
		}
	}
	extra = append(extra, page.Limit+1)
	enabled := b.enabledFilters(&args)
	built, err := b.l.buildSelectVariant(enabled, page.Sort, page.Direction, false, true, len(page.After) > 0)
	if err != nil {
		return KeysetPage[Row]{}, err
	}
	call, err := b.prepareVariant("list_keyset", CallEach, built, &args, extra)
	if err != nil {
		return KeysetPage[Row]{}, err
	}
	rows, err := execSQLAll(ctx, &b.binding, b.l.row, b.l.name, "list_keyset", b.l.meta, call)
	if err != nil {
		return KeysetPage[Row]{}, err
	}
	hasMore := len(rows) > page.Limit
	var next []any
	if hasMore {
		rows = rows[:page.Limit]
		last := rows[len(rows)-1]
		next = make([]any, len(cs.terms))
		for i, term := range cs.terms {
			next[i] = reflect.ValueOf(b.l.row.cols[term.colIdx].field(&last)).Elem().Interface()
		}
	}
	return KeysetPage[Row]{Items: rows, HasMore: hasMore, Next: next}, nil
}
