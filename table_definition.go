package tabula

import "strings"

func (d *Table[T, K, S]) record(spec TableSpec[T, K]) {
	cols := d.definedColumns()
	stmts := make(map[string]string)
	if d.mode.allows(selectOp) {
		stmts["get"], stmts["count"], stmts["list"] = d.stmt.get, d.stmt.count, d.stmt.list
		if d.stmt.unscopedGet != d.stmt.get {
			stmts["unscoped_get"] = d.stmt.unscopedGet
		}
	}
	if !d.readOnly && d.mode.allows(updateOp) {
		stmts["update"], stmts["lock"] = d.stmt.update, d.stmt.lock
		if d.stmt.unscopedLock != d.stmt.lock {
			stmts["unscoped_lock"] = d.stmt.unscopedLock
		}
	}
	if !d.readOnly && d.mode.allows(insertOp) {
		stmts["insert"] = d.stmt.insert
	}
	if d.allowDelete {
		stmts["delete"] = d.stmt.deleteStmt
		if d.stmt.unscopedDelete != d.stmt.deleteStmt {
			stmts["unscoped_delete"] = d.stmt.unscopedDelete
		}
	}
	for _, col := range d.groupByOrder {
		stmts["count_by_"+col] = "SELECT " + col + ", count(*) FROM " + d.name + " WHERE true GROUP BY " + col
	}
	var owners map[string]string
	for fn, col := range spec.Aggregates.columns {
		if owners == nil {
			owners = make(map[string]string)
		}
		op := strings.ToLower(fn)
		key := op + "_" + col
		base := scalarSQL(fn, col, d.name)
		owner := op + ":" + col
		stmts[key] = base + d.liveFilter(d.scope >= 0, nil).Where()
		owners[key] = owner
		if d.scope >= 0 {
			stmts["unscoped_"+key] = base + d.liveFilter(false, nil).Where()
			owners["unscoped_"+key] = owner
		}
	}
	owners = d.registerFixedStatements(stmts, owners)
	d.entry = d.reg.record(Definition{
		Name: d.name, Columns: cols, Statements: stmts, StatementQueries: owners,
		Privileges: d.derivePrivileges(),
	}, false)
}
