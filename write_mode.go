package tabula

// WriteMode selects the operations a table derives and accepts on bound Rows.
// A PostgreSQL role enforces access for SQL issued through other handles.
type WriteMode uint8

const (
	// WriteDefault uses TableSpec.ReadOnly and TableSpec.AllowDelete.
	WriteDefault WriteMode = iota
	// WriteUpdateOnly permits derived reads, locks and updates.
	WriteUpdateOnly
	// WriteInsertOnly permits inserts, COPY and targetless ON CONFLICT DO NOTHING.
	// Inserts leave a generated serial key in PostgreSQL and never read it back.
	WriteInsertOnly
)

type tableOperation uint8

const (
	selectOp tableOperation = iota
	insertOp
	updateOp
	insertReturningOp
	deleteOp
	rawOp
)

func (mode WriteMode) allows(op tableOperation) bool {
	switch mode {
	case WriteDefault:
		return true
	case WriteUpdateOnly:
		return op == selectOp || op == updateOp
	case WriteInsertOnly:
		return op == insertOp
	default:
		return false
	}
}

func (spec TableSpec[T, K]) validateWriteMode(fail func(string, ...any) error) error {
	switch spec.WriteMode {
	case WriteDefault:
		if spec.ReadOnly && spec.AllowDelete {
			return fail("ReadOnly cannot combine with AllowDelete")
		}
	case WriteUpdateOnly, WriteInsertOnly:
		if spec.ReadOnly || spec.AllowDelete {
			return fail("WriteMode cannot combine with ReadOnly or AllowDelete")
		}
	default:
		return fail("unknown WriteMode %d", spec.WriteMode)
	}
	if spec.WriteMode == WriteInsertOnly {
		if spec.Order != "" || len(spec.GroupBy) > 0 {
			return fail("WriteInsertOnly cannot declare Order or GroupBy")
		}
		for range spec.Aggregates.columns {
			return fail("WriteInsertOnly cannot declare Aggregates")
		}
	}
	if spec.UpdateColumns != nil && (spec.WriteMode != WriteUpdateOnly || len(spec.UpdateColumns) == 0) {
		return fail("UpdateColumns requires WriteUpdateOnly and at least one column")
	}
	return nil
}

func (t *Table[T, K, S]) bindUpdateColumns(names []string, fail func(string, ...any) error) error {
	if names == nil {
		return nil
	}
	t.updateColumns = make(map[string]bool, len(names))
	for _, name := range names {
		i, ok := t.byName[name]
		if !ok {
			return fail("UpdateColumns column %q is not declared", name)
		}
		if !t.cols[i].updatable() || t.updateColumns[name] {
			return fail("UpdateColumns column %q is protected or duplicated", name)
		}
		t.updateColumns[name] = true
	}
	if t.version >= 0 && !t.updateColumns[t.cols[t.version].name] {
		return fail("UpdateColumns must include the version column %q for optimistic updates", t.cols[t.version].name)
	}
	return nil
}

func (c column[T]) updatable() bool { return c.role != keyRole && c.role != scopeRole && !c.once }

func (t *Table[T, K, S]) permitsColumn(name string) bool {
	return t.updateColumns == nil || t.updateColumns[name]
}

// The default mode leaves each single-row method's existing column validation in place.
func (t *Table[T, K, S]) allowsUpdateColumn(name string) bool {
	if t.mode != WriteUpdateOnly {
		return true
	}
	i, found := t.byName[name]
	return found && t.cols[i].updatable() && t.permitsColumn(name)
}

func (r *Rows[T, K, S]) checkOperation(op tableOperation) error {
	if r.bindErr != nil {
		return r.bindErr
	}
	if !r.t.mode.allows(op) {
		return ErrOperationDenied
	}
	return nil
}
