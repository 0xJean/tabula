// Package plan provides Tabula's typed PostgreSQL query, expression, join,
// CTE, window, locking, routine and write plan builders.
//
// Use package tabula's Table CRUD methods for primary-key operations and
// tabula.DefineSQL / DefineSQLExec / DefineSQLReturning / DefineSQLList for
// everyday application SQL. Import github.com/0xJean/tabula/plan when you need
// programmatic query composition, finite filter/order/pagination families,
// keyset continuation proofs, or column-level privilege verification generated
// from a typed AST.
//
// Compiled plans register their SQL and StatementShape metadata on the owning
// tabula.Table or tabula.View and execute exclusively through tabula's
// AuthorizedSession channel with the same SQLAuthority and StatementGuard
// guarantees.
package plan
