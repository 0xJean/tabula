package sqlcheck

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/0xJean/tabula"
	"github.com/0xJean/tabula/internal/pgstmt"
)

// MinimizeConfig selects one role and its complete planning workload. Keep
// pins runtime requirements, including requirements from outside the workload.
type MinimizeConfig struct {
	Role       string
	Schemas    []string
	Statements []Statement
	Keep       []Grant
	// Current, if supplied, must match the live direct ACL exactly.
	Current []Grant
	// Runtime probes supplement planning for specified privileges.
	Runtime []RuntimeProbe
}

// RuntimeProbe is a positive execution witness on a disposable fixture. Covers
// names the runtime privileges for which the application considers it evidence.
// Each probe runs as Role with Vars in a separate rolled-back savepoint.
type RuntimeProbe struct {
	BehaviorStatement
	Vars   map[string]string
	Covers []Grant
}

// Minimum records a deterministic, inclusion-minimal set for the tested DML
// privileges, plus pinned and unobservable privileges. Unverified lists
// surviving privileges for which planning alone is insufficient evidence.
type Minimum struct {
	Kept, Removed, Unverified []Grant
	GrantSQL, RevokeSQL       []string
}

func planChecks(g Grant) bool {
	return (g.Kind == "TABLE" || g.Kind == "COLUMN") && slices.Contains([]string{"SELECT", "INSERT", "UPDATE", "DELETE"}, g.Privilege) && !g.GrantOption
}
func (cfg MinimizeConfig) verify(ctx context.Context, tx *sql.Tx) error {
	for _, s := range cfg.Statements {
		if err := PrepareStatement(ctx, tx, s.SQL, cfg.Role); err != nil {
			return fmt.Errorf("%s/%s: %w", cfg.Role, s.Name, err)
		}
	}
	for i, p := range cfg.Runtime {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT tabula_runtime_probe"); err != nil {
			return err
		}
		err := setIdentity(ctx, tx, cfg.Role, p.Vars)
		if err == nil {
			_, err = tx.ExecContext(ctx, p.SQL, p.Args...)
		}
		_, rb := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT tabula_runtime_probe")
		_, rel := tx.ExecContext(ctx, "RELEASE SAVEPOINT tabula_runtime_probe")
		if err = errors.Join(err, rb, rel); err != nil {
			return fmt.Errorf("runtime probe %d: %w", i, err)
		}
	}
	return nil
}

// MinimizeGrants tries cumulative RESTRICT revocations in an always-rolled-back
// transaction. It verifies the baseline, each candidate and the final workload.
// A trial that fails for a non-permission reason aborts the audit.
func MinimizeGrants(ctx context.Context, db *sql.DB, cfg MinimizeConfig) (Minimum, error) {
	var result Minimum
	if err := (RoleStatements{cfg.Role: cfg.Statements}).validate(); err != nil {
		return result, err
	}
	scope := CatalogScope{cfg.Schemas, []string{cfg.Role}}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	schemas, roles, err := scope.bind(ctx, tx)
	if err != nil {
		return result, err
	}
	original, err := readGrants(ctx, tx, schemas, roles)
	if err != nil {
		return result, err
	}
	if cfg.Current != nil {
		diff, err := CompareGrants(GrantManifest{scope, cfg.Current}, original)
		if err != nil {
			return result, err
		}
		if len(diff.Extra)+len(diff.Missing) > 0 {
			return result, errors.New("current grant input differs from database")
		}
	}
	witnessed := map[Grant]bool{}
	for i, p := range cfg.Runtime {
		if _, ok := pgstmt.DML(p.SQL); !ok || len(p.Covers) == 0 {
			return result, fmt.Errorf("runtime probe %d requires a data statement and covered grants", i)
		}
		if err := validateVars(p.Vars); err != nil {
			return result, err
		}
		for _, g := range p.Covers {
			if !slices.Contains(original, g) {
				return result, fmt.Errorf("runtime grant absent: %+v", g)
			}
			witnessed[g] = true
		}
	}
	if err := cfg.verify(ctx, tx); err != nil {
		return result, fmt.Errorf("baseline: %w", err)
	}
	for _, g := range cfg.Keep {
		if !slices.Contains(original, g) {
			return result, fmt.Errorf("pinned grant absent: %+v", g)
		}
	}
	kept := slices.Clone(original)
	// Try removing broad table-level grants before their narrower alternatives.
	trials := slices.Clone(original)
	slices.SortStableFunc(trials, func(a, b Grant) int {
		if a.Kind == b.Kind {
			return 0
		}
		if a.Kind == "TABLE" {
			return -1
		}
		if b.Kind == "TABLE" {
			return 1
		}
		return 0
	})
	for _, g := range trials {
		if slices.Contains(cfg.Keep, g) {
			continue
		}
		candidate := slices.DeleteFunc(slices.Clone(kept), func(v Grant) bool { return v == g })
		diff, err := CompareGrants(GrantManifest{scope, candidate}, kept)
		if err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, "SAVEPOINT tabula_acl_trial"); err != nil {
			return result, err
		}
		for _, q := range diff.SQL {
			if _, err = tx.ExecContext(ctx, q); err != nil {
				break
			}
		}
		if err != nil && tabula.SQLState(err) != "2BP01" {
			return result, fmt.Errorf("could not trial revoke for %+v: %w", g, err)
		}
		if err == nil {
			var installed []Grant
			installed, err = readGrants(ctx, tx, schemas, roles)
			if err != nil {
				return result, fmt.Errorf("read trial ACL: %w", err)
			}
			if !slices.Equal(installed, candidate) {
				return result, fmt.Errorf("trial did not install expected ACL for %+v; use its grantor", g)
			}
		}
		if err == nil {
			err = cfg.verify(ctx, tx)
		}
		untested := err == nil && !planChecks(g) && !witnessed[g]
		if untested {
			result.Unverified = append(result.Unverified, g)
		}
		if err != nil || untested {
			if code := tabula.SQLState(err); err != nil && code != "42501" && code != "2BP01" {
				return result, err
			}
			if _, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT tabula_acl_trial"); err != nil {
				return result, err
			}
		} else {
			kept = candidate
			result.Removed = append(result.Removed, g)
		}
		if _, err = tx.ExecContext(ctx, "RELEASE SAVEPOINT tabula_acl_trial"); err != nil {
			return result, err
		}
	}
	if err := cfg.verify(ctx, tx); err != nil {
		return result, err
	}
	result.Kept = kept
	delta, err := CompareGrants(GrantManifest{scope, kept}, original)
	if err != nil {
		return result, err
	}
	result.RevokeSQL = delta.SQL
	for _, g := range kept {
		sql, _ := g.sql(true, false)
		result.GrantSQL = append(result.GrantSQL, sql)
	}
	if err := tx.Rollback(); err != nil {
		return Minimum{}, err
	}
	return result, nil
}
