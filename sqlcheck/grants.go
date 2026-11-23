package sqlcheck

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/0xJean/tabula/internal/pgstmt"
)

// Grant is a direct ACL entry. Kind is TABLE, COLUMN, SEQUENCE or FUNCTION.
// Arguments uses schema-qualified PostgreSQL type names from the snapshot,
// including the array type name for variadic functions. Grantor is immaterial.
type Grant struct {
	Role, Schema, Kind, Object, Privilege string
	Column                                string `json:",omitempty"`
	Arguments                             string `json:",omitempty"`
	GrantOption                           bool   `json:",omitempty"`
}

// GrantManifest declares the exact grants expected in its CatalogScope.
type GrantManifest struct {
	CatalogScope
	Grants []Grant
}

// GrantDiff contains both sides of the discrepancy and reviewable repair SQL.
type GrantDiff struct {
	Missing, Extra []Grant
	SQL            []string
}

func readGrants(ctx context.Context, tx *sql.Tx, schemas, roles string) ([]Grant, error) {
	grants, err := readRows(ctx, tx, `WITH relations AS (
 SELECT n.nspname AS ns,c.* FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid=c.relnamespace
 WHERE c.relkind IN ('r','p','v','m','f','S') AND n.nspname`+schemaFilter+`), objects AS (
 SELECT ns,relname AS obj,CASE WHEN relkind='S' THEN 'SEQUENCE' ELSE 'TABLE' END AS kind,'' AS col,'' AS args,
 coalesce(relacl,pg_catalog.acldefault(CASE WHEN relkind='S' THEN 's'::"char" ELSE 'r'::"char" END,relowner)) AS acl FROM relations
 UNION ALL SELECT ns,relname,'COLUMN',a.attname,'',a.attacl FROM relations t JOIN pg_catalog.pg_attribute a ON a.attrelid=t.oid
 WHERE a.attnum>0 AND NOT a.attisdropped AND a.attacl IS NOT NULL
 UNION ALL SELECT n.nspname,p.proname,'FUNCTION','',`+routineArgs+`,coalesce(proacl,pg_catalog.acldefault('f',proowner))
 FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE p.prokind IN ('f','a','w') AND n.nspname`+schemaFilter+`),
 entries AS (SELECT CASE WHEN grantee=0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(grantee)::text END AS role,
 ns,kind,obj,col,args,privilege_type,is_grantable FROM objects CROSS JOIN LATERAL pg_catalog.aclexplode(acl))
 SELECT role,ns,kind,obj,col,args,privilege_type,pg_catalog.bool_or(is_grantable) FROM entries
 WHERE role IN (SELECT pg_catalog.jsonb_array_elements_text($2)) GROUP BY role,ns,kind,obj,col,args,privilege_type`, func(r *sql.Rows) (Grant, error) {
		var g Grant
		err := r.Scan(&g.Role, &g.Schema, &g.Kind, &g.Object, &g.Column, &g.Arguments, &g.Privilege, &g.GrantOption)
		return g, err
	}, schemas, roles)
	slices.SortFunc(grants, compareGrant)
	return grants, err
}

func compareGrant(a, b Grant) int {
	return cmp.Or(strings.Compare(a.Role, b.Role), strings.Compare(a.Schema, b.Schema), strings.Compare(a.Object, b.Object), strings.Compare(a.Kind, b.Kind), strings.Compare(a.Arguments, b.Arguments), strings.Compare(a.Column, b.Column), strings.Compare(a.Privilege, b.Privilege))
}
func (g Grant) key() Grant { g.GrantOption = false; return g }

func signature(args string) (string, error) {
	if args == "" {
		return "()", nil
	}
	t := pgstmt.Lex(args)
	ident := func(t pgstmt.Token) bool { return t.Kind == pgstmt.Word || t.Kind == pgstmt.Quoted }
	var types []string
	for {
		if len(t) < 3 || !ident(t[0]) || t[1] != (pgstmt.Token{Kind: pgstmt.Symbol, Text: "."}) || !ident(t[2]) {
			return "", fmt.Errorf("invalid qualified argument types %q", args)
		}
		types = append(types, quoteIdent(t[0].Text)+"."+quoteIdent(t[2].Text))
		t = t[3:]
		if len(t) == 0 {
			break
		}
		if t[0] != (pgstmt.Token{Kind: pgstmt.Symbol, Text: ","}) {
			return "", fmt.Errorf("invalid argument separator in %q", args)
		}
		t = t[1:]
	}
	return "(" + strings.Join(types, ", ") + ")", nil
}

func (g Grant) sql(grant, optionOnly bool) (string, error) {
	allowed := map[string]string{"TABLE": "SELECT INSERT UPDATE DELETE TRUNCATE REFERENCES TRIGGER MAINTAIN", "COLUMN": "SELECT INSERT UPDATE REFERENCES", "SEQUENCE": "SELECT UPDATE USAGE", "FUNCTION": "EXECUTE"}
	if !slices.Contains(strings.Fields(allowed[g.Kind]), g.Privilege) || g.Role == "" || g.Schema == "" || g.Object == "" || (g.Kind == "COLUMN") != (g.Column != "") || (g.Kind != "FUNCTION" && g.Arguments != "") || strings.ContainsRune(g.Role+g.Schema+g.Object+g.Column+g.Arguments, 0) {
		return "", fmt.Errorf("invalid grant %+v", g)
	}
	role := quoteIdent(g.Role)
	if g.Role == "PUBLIC" {
		role = "PUBLIC"
	}
	kind := g.Kind
	priv := g.Privilege
	if kind == "COLUMN" {
		kind = "TABLE"
		priv += " (" + quoteIdent(g.Column) + ")"
	}
	obj := quoteIdent(g.Schema) + "." + quoteIdent(g.Object)
	if kind == "FUNCTION" {
		args, err := signature(g.Arguments)
		if err != nil {
			return "", err
		}
		obj += args
	}
	body := priv + " ON " + kind + " " + obj
	if grant {
		suffix := ""
		if g.GrantOption {
			suffix = " WITH GRANT OPTION"
		}
		return "GRANT " + body + " TO " + role + suffix + ";", nil
	}
	prefix := "REVOKE "
	if optionOnly {
		prefix += "GRANT OPTION FOR "
	}
	return prefix + body + " FROM " + role + " RESTRICT;", nil
}

// CompareGrants computes exact direct ACL differences, including grant options.
// PostgreSQL revokes same-privilege column ACLs when revoking from a table;
// surviving explicit column grants are restored in the generated script.
func CompareGrants(manifest GrantManifest, actual []Grant) (GrantDiff, error) {
	var d GrantDiff
	if _, err := scopeArray(manifest.Schemas); err != nil {
		return d, err
	}
	if _, err := scopeArray(manifest.Roles); err != nil {
		return d, err
	}
	sets := [2]map[Grant]Grant{}
	for i, grants := range [][]Grant{manifest.Grants, actual} {
		set := map[Grant]Grant{}
		for _, g := range grants {
			if _, err := g.sql(true, false); err != nil {
				return d, err
			}
			if !slices.Contains(manifest.Roles, g.Role) || !slices.Contains(manifest.Schemas, g.Schema) {
				return d, fmt.Errorf("grant outside scope: %+v", g)
			}
			k := g.key()
			if _, ok := set[k]; ok {
				return d, fmt.Errorf("duplicate grant: %+v", g)
			}
			set[k] = g
		}
		sets[i] = set
	}
	want, have := sets[0], sets[1]
	for k, g := range want {
		if h, ok := have[k]; !ok || h != g {
			d.Missing = append(d.Missing, g)
		}
	}
	for k, g := range have {
		if w, ok := want[k]; !ok || w != g {
			d.Extra = append(d.Extra, g)
		}
	}
	slices.SortFunc(d.Missing, compareGrant)
	slices.SortFunc(d.Extra, compareGrant)
	restore := map[Grant]bool{}
	for _, g := range d.Extra {
		w, keep := want[g.key()]
		if keep && w.GrantOption {
			continue
		}
		stmt, _ := g.sql(false, keep)
		d.SQL = append(d.SQL, stmt)
		if g.Kind == "TABLE" {
			for _, c := range want {
				if c.Kind == "COLUMN" && c.Role == g.Role && c.Schema == g.Schema && c.Object == g.Object && c.Privilege == g.Privilege {
					restore[c] = true
				}
			}
		}
	}
	for _, g := range d.Missing {
		if h, ok := have[g.key()]; !ok || !h.GrantOption {
			restore[g] = true
		}
	}
	var add []Grant
	for g := range restore {
		add = append(add, g)
	}
	slices.SortFunc(add, compareGrant)
	for _, g := range add {
		stmt, _ := g.sql(true, false)
		d.SQL = append(d.SQL, stmt)
	}
	return d, nil
}

// CheckGrantManifest compares a declaration against every supported object in its schemas.
func CheckGrantManifest(ctx context.Context, db *sql.DB, m GrantManifest) (GrantDiff, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return GrantDiff{}, err
	}
	defer func() { _ = tx.Rollback() }()
	schemas, roles, err := m.bind(ctx, tx)
	if err != nil {
		return GrantDiff{}, err
	}
	grants, err := readGrants(ctx, tx, schemas, roles)
	if err != nil {
		return GrantDiff{}, err
	}
	return CompareGrants(m, grants)
}
