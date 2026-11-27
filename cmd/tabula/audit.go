package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/sqlcheck"
)

func decodeConfig[T any](raw []byte) (T, error) {
	var c T
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return c, errors.New("expected a single JSON configuration")
	}
	return c, nil
}

type auditOptions struct {
	section, against, bindings, snapshot string
	granularity                          sqlcheck.GrantGranularity
	granularitySet                       bool
}

func runAudit(ctx context.Context, name string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	config := fs.String("config", "", "path to command-specific JSON configuration")
	var dsn string
	var opts auditOptions
	var granularityFlag string
	if name != "inventory" {
		fs.StringVar(&dsn, "dsn", "", "disposable PostgreSQL connection string (or TABULA_DSN)")
	} else {
		fs.StringVar(&opts.bindings, "bindings", "", "emit role workload using site assignments in this JSON file")
		fs.StringVar(&opts.snapshot, "snapshot", "", "sqlcheck snapshot containing the owning definitions for composed queries and aggregates")
	}
	if name == "catalog" {
		fs.StringVar(&opts.section, "section", "", "emit one named snapshot file")
		fs.StringVar(&opts.against, "against", "", "compare with committed snapshot directory")
	}
	if name == "catalog" || name == "grants" || name == "minimize" {
		fs.StringVar(&granularityFlag, "granularity", "all", "grant audit granularity: all or table")
	}
	if err := fs.Parse(args); err != nil {
		return flagExitCode(err)
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "granularity" {
			opts.granularitySet = true
		}
	})
	if opts.granularitySet {
		g, err := sqlcheck.ParseGrantGranularity(granularityFlag)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		opts.granularity = g
	}
	if dsn == "" {
		dsn = os.Getenv("TABULA_DSN")
	}
	if *config == "" || fs.NArg() != 0 || name != "inventory" && dsn == "" {
		fmt.Fprintln(stderr, "expected --config, and a DSN for database commands")
		return 2
	}
	raw, err := os.ReadFile(*config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var db *sql.DB
	if name != "inventory" {
		db, err = openDB(ctx, dsn, "tabula "+name, stderr)
		if err != nil {
			return 2
		}
		defer func() { _ = db.Close() }()
	}
	data, findings, err := audit(ctx, name, db, raw, opts)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if s, ok := data.(string); ok {
		_, err = io.WriteString(stdout, s)
	} else {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(data)
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	for _, f := range findings {
		fmt.Fprintln(stderr, f)
	}
	if len(findings) > 0 {
		return 1
	}
	return 0
}

func audit(ctx context.Context, name string, db *sql.DB, raw []byte, opts auditOptions) (any, []string, error) {
	switch name {
	case "inventory":
		c, err := decodeConfig[sqlcheck.InventoryConfig](raw)
		if err != nil {
			return nil, nil, err
		}
		report, err := sqlcheck.CollectSQL(ctx, c)
		if opts.bindings != "" && err == nil && len(report.Findings) == 0 {
			b, err := os.ReadFile(opts.bindings)
			if err != nil {
				return nil, nil, err
			}
			bindings, err := decodeConfig[map[string]sqlcheck.SQLBinding](b)
			if err != nil {
				return nil, nil, err
			}
			var defs []tabula.Definition
			if opts.snapshot != "" {
				snapRaw, err := os.ReadFile(opts.snapshot)
				if err != nil {
					return nil, nil, err
				}
				defs, err = sqlcheck.ParseSnapshot(string(snapRaw))
				if err != nil {
					return nil, nil, err
				}
			}
			work, err := report.BindRolesWithDefinitions(bindings, defs, nil)
			if err != nil {
				return nil, []string{err.Error()}, nil
			}
			return work, nil, nil
		}
		return report, report.Findings, err
	case "verify-roles", "check-coverage":
		work, err := decodeConfig[sqlcheck.RoleStatements](raw)
		if err != nil {
			return nil, nil, err
		}
		check := sqlcheck.VerifyRoleStatements
		if name == "check-coverage" {
			check = sqlcheck.CheckPolicyCoverage
		}
		f, err := check(ctx, db, work)
		return f, f, err
	case "grants":
		m, err := decodeConfig[sqlcheck.GrantManifest](raw)
		if err != nil {
			return nil, nil, err
		}
		if opts.granularitySet {
			m.Granularity = opts.granularity
		}
		d, err := sqlcheck.CheckGrantManifest(ctx, db, m)
		var f []string
		if len(d.Extra)+len(d.Missing) > 0 {
			f = append(f, fmt.Sprintf("%d extra grants, %d missing grants", len(d.Extra), len(d.Missing)))
		}
		return d, f, err
	case "minimize":
		cfg, err := decodeConfig[sqlcheck.MinimizeConfig](raw)
		if err != nil {
			return nil, nil, err
		}
		if opts.granularitySet {
			cfg.Granularity = opts.granularity
		}
		m, err := sqlcheck.MinimizeGrants(ctx, db, cfg)
		return m, nil, err
	case "check-policies":
		c, err := decodeConfig[struct {
			sqlcheck.CatalogScope
			Rules []sqlcheck.PolicyRule
		}](raw)
		if err != nil {
			return nil, nil, err
		}
		catalog, err := sqlcheck.ReadCatalog(ctx, db, c.CatalogScope)
		if err != nil {
			return nil, nil, err
		}
		classes, f, err := sqlcheck.ClassifyPolicies(catalog.Policies, c.Rules)
		return classes, f, err
	case "test-rls":
		m, err := decodeConfig[sqlcheck.RLSMatrix](raw)
		if err != nil {
			return nil, nil, err
		}
		f, err := sqlcheck.RunRLSMatrix(ctx, db, m)
		return f, f, err
	case "catalog":
		c, err := decodeConfig[sqlcheck.CatalogScope](raw)
		if err != nil {
			return nil, nil, err
		}
		if opts.granularitySet {
			c.Granularity = opts.granularity
		}
		catalog, err := sqlcheck.ReadCatalog(ctx, db, c)
		if err != nil {
			return nil, nil, err
		}
		return catalogReport(catalog, opts.section, opts.against)
	default:
		return nil, nil, fmt.Errorf("unknown audit command %q", name)
	}
}

func catalogReport(c sqlcheck.Catalog, section, against string) (any, []string, error) {
	sections := c.Snapshots()
	if section != "" {
		s, ok := sections[section]
		if !ok {
			return nil, nil, fmt.Errorf("unknown catalog section %q", section)
		}
		sections = map[string]string{section: s}
	}
	if against == "" && section != "" {
		return sections[section], nil, nil
	}
	bundle := map[string]json.RawMessage{}
	var findings []string
	for _, k := range slices.Sorted(maps.Keys(sections)) {
		bundle[k] = json.RawMessage(sections[k])
		if against != "" {
			want, err := os.ReadFile(filepath.Join(against, k))
			if errors.Is(err, os.ErrNotExist) {
				findings = append(findings, k+": snapshot missing")
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			if string(want) != sections[k] {
				diff := sqlcheck.FirstDifference(string(want), sections[k])
				if diff == "" {
					diff = "trailing newline differs"
				}
				findings = append(findings, k+": "+diff)
			}
		}
	}
	return bundle, findings, nil
}
