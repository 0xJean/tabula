// Command tabula verifies committed Tabula SQL snapshots against a real
// PostgreSQL database using PREPARE and EXPLAIN EXECUTE.
//
// Usage:
//
//	tabula check --dsn "$POSTGRES_DSN" --snapshot path/to/defined-statements.sql [--role app_role]
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"

	_ "github.com/lib/pq"

	"github.com/0xJean/tabula/sqlcheck"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "check":
		return runCheck(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "tabula: unknown subcommand %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: tabula check --dsn <postgres-dsn> --snapshot <file> [--role <role>]")
}

func runCheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsn := fs.String("dsn", os.Getenv("TABULA_DSN"), "PostgreSQL DSN (defaults to TABULA_DSN)")
	snapshotPath := fs.String("snapshot", "", "path to a sqlcheck snapshot file")
	role := fs.String("role", "", "optional PostgreSQL role to SET LOCAL ROLE before PREPARE/EXPLAIN")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dsn == "" {
		fmt.Fprintln(stderr, "tabula check: --dsn (or TABULA_DSN) is required")
		return 2
	}
	if *snapshotPath == "" {
		fmt.Fprintln(stderr, "tabula check: --snapshot is required")
		return 2
	}
	raw, err := os.ReadFile(*snapshotPath)
	if err != nil {
		fmt.Fprintf(stderr, "tabula check: read snapshot: %v\n", err)
		return 2
	}
	defs, err := sqlcheck.ParseSnapshot(string(raw))
	if err != nil {
		fmt.Fprintf(stderr, "tabula check: parse snapshot: %v\n", err)
		return 2
	}
	totalStmts := 0
	for _, d := range defs {
		totalStmts += len(d.Statements)
	}
	if totalStmts == 0 {
		fmt.Fprintln(stderr, "tabula check: snapshot contains no statements")
		return 2
	}

	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		fmt.Fprintf(stderr, "tabula check: open database: %v\n", err)
		return 2
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		fmt.Fprintf(stderr, "tabula check: connect database: %v\n", err)
		return 2
	}

	failures, err := sqlcheck.VerifyStatements(ctx, db, defs, *role)
	if err != nil {
		fmt.Fprintf(stderr, "tabula check: verify statements: %v\n", err)
		return 2
	}
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintln(stderr, f)
		}
		fmt.Fprintf(stderr, "tabula check: %d of %d statements failed\n", len(failures), totalStmts)
		return 1
	}
	if *role != "" {
		fmt.Fprintf(stdout, "tabula check: %d statements across %d tables verified as role %s\n", totalStmts, len(defs), *role)
	} else {
		fmt.Fprintf(stdout, "tabula check: %d statements across %d tables verified\n", totalStmts, len(defs))
	}
	return 0
}
