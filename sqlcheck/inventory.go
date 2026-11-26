package sqlcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	tabulaPkgPath    = "github.com/0xJean/tabula"
	stdsqlPkgPath    = "github.com/0xJean/tabula/stdsql"
	pgxdriverPkgPath = "github.com/0xJean/tabula/pgxdriver"
)

// SQLMethod matches the package-qualified receiver type (without pointer or
// generic arguments). Argument is the zero-based SQL position in a method call.
type SQLMethod struct {
	Receiver, Method string
	Argument         int
}

// SQLRoutineMethod matches a method call that invokes a PostgreSQL routine by
// name and positional argument slice rather than raw SQL text. NameArgument and
// ArgsArgument are zero-based positions of fixed parameters after the receiver.
// A nil or dense slice literal supplies the arity. Trailing variadic result
// destinations do not affect it.
// Schema defaults to "public". When SetReturning is true, static calls emit
// SELECT * FROM <schema>.<name>($1, ...); otherwise SELECT <schema>.<name>($1, ...).
type SQLRoutineMethod struct {
	Receiver     string
	Method       string
	NameArgument int
	ArgsArgument int
	Schema       string `json:",omitempty"`
	SetReturning bool   `json:",omitempty"`
}

// CallTarget matches a Go package-level function (Receiver == ""), method
// (Receiver != ""), or struct field (Field == true) by canonical package path
// and symbol name. Field targets also match selections promoted by embedding.
type CallTarget struct {
	Package  string
	Receiver string `json:",omitempty"`
	Name     string
	Field    bool   `json:",omitempty"`
	Category string `json:",omitempty"`
}

// CallSite records one resolved reference or invocation of a CallTarget.
// Escaped is true when a function or method is referenced as a value rather
// than called directly.
type CallSite struct {
	Package  string
	File     string
	Line     int
	Symbol   string
	Category string `json:",omitempty"`
	Escaped  bool   `json:",omitempty"`
}

// DefaultSecurityCallTargets returns the built-in security-review call targets:
// session keys, registry sealing, proof signing, keyed runner/Direct factories,
// nested-transaction marker helpers, and both drivers' raw transaction access.
func DefaultSecurityCallTargets() []CallTarget {
	return []CallTarget{
		{Package: tabulaPkgPath, Name: "NewSessionKey", Category: "session_key"},
		{Package: tabulaPkgPath, Receiver: tabulaPkgPath + ".Registry", Name: "Seal", Category: "registry_seal"},
		{Package: tabulaPkgPath, Receiver: tabulaPkgPath + ".SessionKey", Name: "Sign", Category: "session_sign"},
		{Package: tabulaPkgPath, Name: "WithoutTransaction", Category: "tx_escape"},
		{Package: tabulaPkgPath, Name: "MarkTransaction", Category: "tx_marker"},
		{Package: tabulaPkgPath, Name: "InTransaction", Category: "tx_marker"},
		{Package: stdsqlPkgPath, Name: "NewRunner", Category: "runner_factory"},
		{Package: stdsqlPkgPath, Name: "DirectSessionKey", Category: "direct_factory"},
		{Package: stdsqlPkgPath, Name: "NewSession", Category: "unsealed_session"},
		{Package: stdsqlPkgPath, Receiver: stdsqlPkgPath + ".Runner", Name: "WithinRaw", Category: "raw_tx"},
		{Package: stdsqlPkgPath, Receiver: stdsqlPkgPath + ".Runner", Name: "WithinRawOptions", Category: "raw_tx"},
		{Package: stdsqlPkgPath, Receiver: stdsqlPkgPath + ".RawSession", Name: "Tx", Category: "raw_tx"},
		{Package: stdsqlPkgPath, Receiver: stdsqlPkgPath + ".RunnerOptions", Name: "ExposeTx", Field: true, Category: "raw_tx"},
		{Package: pgxdriverPkgPath, Name: "NewRunner", Category: "runner_factory"},
		{Package: pgxdriverPkgPath, Name: "DirectWithConfig", Category: "direct_factory"},
		{Package: pgxdriverPkgPath, Name: "NewSession", Category: "unsealed_session"},
		{Package: pgxdriverPkgPath, Receiver: pgxdriverPkgPath + ".Runner", Name: "WithinRaw", Category: "raw_tx"},
		{Package: pgxdriverPkgPath, Receiver: pgxdriverPkgPath + ".Runner", Name: "WithinRawTxOptions", Category: "raw_tx"},
		{Package: pgxdriverPkgPath, Receiver: pgxdriverPkgPath + ".RawSession", Name: "Tx", Category: "raw_tx"},
		{Package: pgxdriverPkgPath, Receiver: pgxdriverPkgPath + ".RunnerOptions", Name: "ExposeTx", Field: true, Category: "raw_tx"},
	}
}

// InventoryConfig selects buildable Go packages, SQL methods, call targets,
// and reviewed site exceptions. Patterns default to ./... in Dir. Tags use the
// Go build tag syntax.
type InventoryConfig struct {
	Dir      string
	Patterns []string
	Tags     string
	// Tests includes internal and external _test.go files in matching packages.
	Tests          bool
	Methods        []SQLMethod
	RoutineMethods []SQLRoutineMethod `json:",omitempty"`
	Exceptions     map[string]string
	// CallTargets collects typed references to package-level functions, methods,
	// and struct fields for review (reported in SQLInventory.CallSites).
	CallTargets []CallTarget
	// AuditSecurityCalls adds DefaultSecurityCallTargets() to CallTargets.
	AuditSecurityCalls bool
}

// SQLSite describes a static or dynamic SQL call, composed query definition,
// or single-table aggregate call. ID is independent of line numbers.
type SQLSite struct {
	ID, File         string
	Line             int
	Receiver, Method string
	SQL              string `json:",omitempty"`
	QueryKey         string `json:",omitempty"`
	// Definition is the owning descriptor when its name is resolved from source.
	Definition string `json:",omitempty"`
	// NoFilter records a scalar aggregate call with an explicit nil where argument.
	// Only these calls can use the aggregate SQL from a descriptor snapshot.
	NoFilter  bool   `json:",omitempty"`
	Kind      string `json:",omitempty"`
	Dynamic   bool   `json:",omitempty"`
	Exception string `json:",omitempty"`
}

// SQLInventory retains reviewed sites, typed security/factory call sites, and
// reports unreviewed dynamic SQL and stale exceptions.
type SQLInventory struct {
	Sites     []SQLSite
	CallSites []CallSite `json:",omitempty"`
	Findings  []string
}

type buildPackage struct {
	ImportPath, Dir, Export string
	CompiledGoFiles         []string
	ForTest                 string
	DepOnly                 bool
	ImportMap               map[string]string
}

// CollectSQL resolves Go types and constants using the installed Go toolchain,
// including aliases and imported constants. It recognizes View.DefineQuery,
// View.TryDefineQuery, and single-table aggregate calls (Max, Min, Sum, Avg,
// CountBy, Exists) as static sites when their key or column is compile-time
// constant, and collects typed call sites for CallTargets / AuditSecurityCalls.
// Typed row declarations and executions are built-in sites. Indirect plan
// handles can be resolved through SQLBinding.Definition and SQLBinding.Plan.
// Raw SQL methods and additional call targets are optional.
func CollectSQL(ctx context.Context, cfg InventoryConfig) (SQLInventory, error) {
	var out SQLInventory
	methods, err := compileSQLMethods(cfg.Methods, cfg.RoutineMethods)
	if err != nil {
		return out, err
	}
	allTargets := slices.Clone(cfg.CallTargets)
	if cfg.AuditSecurityCalls {
		allTargets = append(allTargets, DefaultSecurityCallTargets()...)
	}
	targets, err := compileCallTargets(allTargets)
	if err != nil {
		return out, err
	}
	root, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return out, err
	}
	args := []string{"list", "-deps", "-export", "-compiled", "-json=ImportPath,Dir,Export,CompiledGoFiles,ForTest,DepOnly,ImportMap"}
	if cfg.Tests {
		args = append(args, "-test")
	}
	if cfg.Tags != "" {
		args = append(args, "-tags", cfg.Tags)
	}
	patterns := cfg.Patterns
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	for _, p := range patterns {
		if strings.HasPrefix(p, "-") {
			return out, fmt.Errorf("invalid package pattern %q", p)
		}
	}
	cmd := exec.CommandContext(ctx, "go", append(args, patterns...)...)
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("go list: %w: %s", err, stderr.String())
	}
	exports := map[string]string{}
	var selected []buildPackage
	dec := json.NewDecoder(bytes.NewReader(output))
	for dec.More() {
		var p buildPackage
		if err := dec.Decode(&p); err != nil {
			return out, err
		}
		exports[p.ImportPath] = p.Export
		if !p.DepOnly && len(p.CompiledGoFiles) > 0 {
			selected = append(selected, p)
		}
	}
	if len(selected) == 0 {
		return out, fmt.Errorf("no packages match %v", patterns)
	}
	seen := map[string]bool{}
	for _, p := range selected {
		sites, callSites, err := p.inventory(ctx, root, exports, methods, targets)
		if err != nil {
			return out, err
		}
		for _, site := range sites {
			reason, ok := cfg.Exceptions[site.ID]
			if ok && strings.TrimSpace(reason) == "" {
				return out, fmt.Errorf("exception %s needs a reason", site.ID)
			}
			site.Exception = reason
			seen[site.ID] = true
			if site.Dynamic && !ok {
				out.Findings = append(out.Findings, "unreviewed dynamic SQL: "+site.ID)
			}
			out.Sites = append(out.Sites, site)
		}
		out.CallSites = append(out.CallSites, callSites...)
	}
	for id := range cfg.Exceptions {
		if !seen[id] {
			out.Findings = append(out.Findings, "stale SQL exception: "+id)
		}
	}
	slices.SortFunc(out.Sites, func(a, b SQLSite) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(out.CallSites, func(a, b CallSite) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return strings.Compare(a.Symbol, b.Symbol)
	})
	slices.Sort(out.Findings)
	return out, nil
}

type compiledCallTargets struct {
	funcs   map[[2]string]CallTarget
	methods map[[2]string]CallTarget
	fields  map[[2]string]CallTarget
}

func compileCallTargets(list []CallTarget) (compiledCallTargets, error) {
	ct := compiledCallTargets{
		funcs:   map[[2]string]CallTarget{},
		methods: map[[2]string]CallTarget{},
		fields:  map[[2]string]CallTarget{},
	}
	for _, t := range list {
		if t.Name == "" {
			return ct, fmt.Errorf("invalid call target %+v: Name is required", t)
		}
		switch {
		case t.Field:
			if t.Receiver == "" {
				return ct, fmt.Errorf("invalid field call target %+v: Receiver is required", t)
			}
			ct.fields[[2]string{t.Receiver, t.Name}] = t
		case t.Receiver != "":
			ct.methods[[2]string{t.Receiver, t.Name}] = t
		default:
			if t.Package == "" {
				return ct, fmt.Errorf("invalid function call target %+v: Package is required", t)
			}
			ct.funcs[[2]string{t.Package, t.Name}] = t
		}
	}
	return ct, nil
}
