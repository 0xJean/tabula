// Command tabula verifies committed Tabula SQL snapshots and PostgreSQL RLS
// configuration against a real PostgreSQL database.
//
// Usage:
//
//	tabula check --dsn "$POSTGRES_DSN" --snapshot path/to/defined-statements.sql [--role app_role]
//	tabula check-rls --dsn "$POSTGRES_DSN" [--schema public] [--tables a,b] [--force] [--restrictive] [--forbid-roles r1,r2]
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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
	case "check-rls":
		return runCheckRLS(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "tabula: unknown subcommand %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage:")
	_, _ = fmt.Fprintln(w, "  tabula check --dsn <postgres-dsn> --snapshot <file> [--role <role>]")
	_, _ = fmt.Fprintln(w, "  tabula check-rls --dsn <postgres-dsn> [--schema public] [--tables t1,t2] [--force] [--restrictive] [--forbid-roles r1,r2]")
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
		_, _ = fmt.Fprintln(stderr, "tabula check: --dsn (or TABULA_DSN) is required")
		return 2
	}
	if *snapshotPath == "" {
		_, _ = fmt.Fprintln(stderr, "tabula check: --snapshot is required")
		return 2
	}
	raw, err := os.ReadFile(*snapshotPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tabula check: read snapshot: %v\n", err)
		return 2
	}
	defs, err := sqlcheck.ParseSnapshot(string(raw))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tabula check: parse snapshot: %v\n", err)
		return 2
	}
	totalStmts := 0
	for _, d := range defs {
		totalStmts += len(d.Statements)
	}
	if totalStmts == 0 {
		_, _ = fmt.Fprintln(stderr, "tabula check: snapshot contains no statements")
		return 2
	}

	db, err := openDB(ctx, *dsn, "tabula check", stderr)
	if err != nil {
		return 2
	}
	defer func() { _ = db.Close() }()

	failures, err := sqlcheck.VerifyStatements(ctx, db, defs, *role)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tabula check: verify statements: %v\n", err)
		return 2
	}
	if len(failures) > 0 {
		for _, f := range failures {
			_, _ = fmt.Fprintln(stderr, f)
		}
		_, _ = fmt.Fprintf(stderr, "tabula check: %d of %d statements failed\n", len(failures), totalStmts)
		return 1
	}
	if *role != "" {
		_, _ = fmt.Fprintf(stdout, "tabula check: %d statements across %d tables verified as role %s\n", totalStmts, len(defs), *role)
	} else {
		_, _ = fmt.Fprintf(stdout, "tabula check: %d statements across %d tables verified\n", totalStmts, len(defs))
	}
	return 0
}

func runCheckRLS(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check-rls", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsn := fs.String("dsn", os.Getenv("TABULA_DSN"), "PostgreSQL DSN (defaults to TABULA_DSN)")
	schema := fs.String("schema", "public", "PostgreSQL schema to audit")
	tablesCSV := fs.String("tables", "", "comma-separated table names that must have RLS enabled (default: all RLS tables in schema)")
	force := fs.Bool("force", true, "require FORCE ROW LEVEL SECURITY")
	restrictive := fs.Bool("restrictive", false, "require at least one RESTRICTIVE policy per table")
	forbidCSV := fs.String("forbid-roles", "", "comma-separated role names forbidden from appearing directly in policies")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *dsn == "" {
		_, _ = fmt.Fprintln(stderr, "tabula check-rls: --dsn (or TABULA_DSN) is required")
		return 2
	}

	db, err := openDB(ctx, *dsn, "tabula check-rls", stderr)
	if err != nil {
		return 2
	}
	defer func() { _ = db.Close() }()

	failures, err := sqlcheck.CheckRLS(ctx, db, sqlcheck.RLSExpectation{
		Schema:               *schema,
		Tables:               splitCSV(*tablesCSV),
		RequireForce:         *force,
		RequireRestrictive:   *restrictive,
		ForbiddenPolicyRoles: splitCSV(*forbidCSV),
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tabula check-rls: %v\n", err)
		return 2
	}
	if len(failures) > 0 {
		for _, f := range failures {
			_, _ = fmt.Fprintln(stderr, f)
		}
		_, _ = fmt.Fprintf(stderr, "tabula check-rls: %d RLS violations found\n", len(failures))
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "tabula check-rls: schema %s RLS check passed\n", *schema)
	return 0
}

func openDB(ctx context.Context, dsn, prefix string, stderr io.Writer) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: open database: %v\n", prefix, err)
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		_, _ = fmt.Fprintf(stderr, "%s: connect database: %v\n", prefix, err)
		return nil, err
	}
	return db, nil
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
