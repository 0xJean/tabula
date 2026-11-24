// Command tabula verifies SQL and PostgreSQL authorization. Run tabula --help
// for the command list and tabula <command> -h for its flags. Database commands
// accept TABULA_DSN. See docs/AUTHORIZATION.md for the configuration formats.
package main

import (
	"context"
	"database/sql"
	"errors"
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

type command struct {
	name, summary string
	run           func(context.Context, string, []string, io.Writer, io.Writer) int
}

func commands() []command {
	return []command{
		{"check", "Prepare and explain statements from a SQL snapshot.", runCheck},
		{"check-rls", "Check row-level security settings on tables.", runCheckRLS},
		{"check-login-role", "Verify login role hygiene (NOINHERIT, memberships, grants, ownership, CREATE).", runCheckLoginRole},
		{"inventory", "Collect SQL call sites and their role assignments from Go source.", runAudit},
		{"verify-roles", "Prepare and explain each role's workload.", runAudit},
		{"grants", "Compare direct grants with a manifest and report reconciliation SQL.", runAudit},
		{"minimize", "Trial grant removals in a rolled-back transaction.", runAudit},
		{"catalog", "Snapshot the PostgreSQL authorization catalog or compare saved snapshots.", runAudit},
		{"check-policies", "Classify installed policies using application rules.", runAudit},
		{"check-coverage", "Check RLS command coverage for a role workload.", runAudit},
		{"test-rls", "Run a role and scope matrix against expected rows or denials.", runAudit},
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	}
	for _, c := range commands() {
		if args[0] == c.name {
			return c.run(ctx, c.name, args[1:], stdout, stderr)
		}
	}
	_, _ = fmt.Fprintf(stderr, "tabula: unknown subcommand %q\n", args[0])
	printUsage(stderr)
	return 2
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: tabula <command> [flags]")
	for _, c := range commands() {
		_, _ = fmt.Fprintf(w, "  tabula %-15s %s\n", c.name, c.summary)
	}
	_, _ = fmt.Fprintln(w, "Use tabula <command> -h for flags.")
}

func flagExitCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

func runCheck(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsn := fs.String("dsn", "", "PostgreSQL DSN (defaults to TABULA_DSN)")
	*dsn = os.Getenv("TABULA_DSN")
	snapshotPath := fs.String("snapshot", "", "path to a sqlcheck snapshot file")
	role := fs.String("role", "", "optional PostgreSQL role to SET LOCAL ROLE before PREPARE/EXPLAIN")
	if err := fs.Parse(args); err != nil {
		return flagExitCode(err)
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

func runCheckRLS(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsn := fs.String("dsn", "", "PostgreSQL DSN (defaults to TABULA_DSN)")
	*dsn = os.Getenv("TABULA_DSN")
	schema := fs.String("schema", "public", "PostgreSQL schema to audit")
	tablesCSV := fs.String("tables", "", "comma-separated table names that must have RLS enabled (default: all RLS tables in schema)")
	force := fs.Bool("force", true, "require FORCE ROW LEVEL SECURITY")
	restrictive := fs.Bool("restrictive", false, "require at least one RESTRICTIVE policy per table")
	forbidCSV := fs.String("forbid-roles", "", "comma-separated role names forbidden from appearing directly in policies")
	if err := fs.Parse(args); err != nil {
		return flagExitCode(err)
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

func runCheckLoginRole(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dsn := fs.String("dsn", "", "PostgreSQL DSN (defaults to TABULA_DSN)")
	*dsn = os.Getenv("TABULA_DSN")
	role := fs.String("role", "", "PostgreSQL login role name to audit (required)")
	memberOfCSV := fs.String("member-of", "", "comma-separated exact role memberships allowed for the login role")
	schemasCSV := fs.String("schemas", "public", "comma-separated schemas where CREATE must be denied")
	if err := fs.Parse(args); err != nil {
		return flagExitCode(err)
	}
	if *dsn == "" {
		_, _ = fmt.Fprintln(stderr, "tabula check-login-role: --dsn (or TABULA_DSN) is required")
		return 2
	}
	if strings.TrimSpace(*role) == "" {
		_, _ = fmt.Fprintln(stderr, "tabula check-login-role: --role is required")
		return 2
	}
	db, err := openDB(ctx, *dsn, "tabula check-login-role", stderr)
	if err != nil {
		return 2
	}
	defer func() { _ = db.Close() }()

	failures, err := sqlcheck.CheckLoginRole(ctx, db, sqlcheck.LoginRoleExpectation{
		Role:     *role,
		MemberOf: splitCSV(*memberOfCSV),
		Schemas:  splitCSV(*schemasCSV),
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "tabula check-login-role: %v\n", err)
		return 2
	}
	if len(failures) > 0 {
		for _, f := range failures {
			_, _ = fmt.Fprintln(stderr, f)
		}
		_, _ = fmt.Fprintf(stderr, "tabula check-login-role: %d login role violations found\n", len(failures))
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "tabula check-login-role: role %s passed hygiene checks\n", *role)
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
