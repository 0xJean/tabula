package plan

import "fmt"

// Cursor operators have a closed pg_catalog identity. Record the function
// called by each explicit operator; ORDER BY's index support routines do not
// imply caller EXECUTE grants.
func requireReadComparison(c *typedCompiler, kind catalogType, operation string) error {
	if !readComparable(kind) {
		return fmt.Errorf("tabula: unsupported cursor comparison type")
	}
	name, arg := kind.Name, kind.Qualified()
	if kind.Element != nil {
		name, arg = "array", "pg_catalog.anyarray"
	}
	switch name {
	case "varchar":
		name, arg = "text", "pg_catalog.text"
	case "cidr":
		name, arg = "network", "pg_catalog.inet"
	case "inet":
		name = "network"
	}
	suffix := "_" + operation
	switch name {
	case "bool", "int2", "int4", "int8", "oid", "float4", "float8", "bpchar", "bytea":
		suffix = operation
	}
	if name == "text" && (operation == "eq" || operation == "ne") {
		suffix = operation
	}
	c.requireNative("pg_catalog." + name + suffix + "(" + arg + "," + arg + ")")
	return nil
}
