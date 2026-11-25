WITH target AS (
    SELECT oid FROM pg_catalog.pg_roles WHERE rolname::text = $1
), requested AS (
    SELECT DISTINCT * FROM pg_catalog.jsonb_to_recordset($2::jsonb) AS need(
        "Kind" text, "Object" text, "Column" text, "Privilege" text,
        "Schema" text, "Name" text, "Args" jsonb
    )
), missing AS (
    SELECT 'ROLE' AS kind, $1::text AS object, '' AS col, 'EXISTS' AS privilege
    WHERE NOT EXISTS (SELECT FROM target)
    UNION ALL
    SELECT q."Kind", q."Object", q."Column", q."Privilege"
    FROM requested q CROSS JOIN target r
    LEFT JOIN pg_catalog.pg_namespace n ON n.nspname::text = q."Schema"
    LEFT JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid AND c.relname::text = q."Name"
    LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attname::text = q."Column"
        AND a.attnum > 0 AND NOT a.attisdropped
    WHERE (CASE
        WHEN q."Kind" = 'SCHEMA' THEN pg_catalog.has_schema_privilege(r.oid, n.oid, 'USAGE')
        WHEN q."Kind" = 'FUNCTION' THEN EXISTS (
            SELECT FROM pg_catalog.pg_proc p
            WHERE p.pronamespace = n.oid AND p.proname::text = q."Name" AND p.prokind <> 'p'
              AND ARRAY(SELECT pg_catalog.unnest(p.proargtypes)) = ARRAY(
                  SELECT CASE WHEN arg->>'Schema' = '' THEN pg_catalog.to_regtype(arg->>'Spelling')::oid
                      WHEN (arg->>'Array')::boolean THEN typ.typarray ELSE typ.oid END
                  FROM pg_catalog.jsonb_array_elements(q."Args") WITH ORDINALITY AS args(arg, pos)
                  LEFT JOIN pg_catalog.pg_namespace ns ON ns.nspname::text = arg->>'Schema'
                  LEFT JOIN pg_catalog.pg_type typ ON typ.typnamespace = ns.oid AND typ.typname::text = arg->>'Name'
                  ORDER BY pos
              ) AND pg_catalog.has_function_privilege(r.oid, p.oid, 'EXECUTE')
        )
        WHEN q."Kind" = 'SEQUENCE' AND q."Column" = '' THEN
            CASE WHEN c.relkind = 'S' THEN pg_catalog.has_sequence_privilege(r.oid, c.oid, 'USAGE') END
        WHEN q."Kind" = 'SEQUENCE' THEN EXISTS (
            SELECT FROM pg_catalog.pg_depend d
            JOIN pg_catalog.pg_class seq ON seq.oid = d.objid AND seq.relkind = 'S'
            WHERE d.refclassid = 'pg_catalog.pg_class'::regclass AND d.classid = d.refclassid
              AND d.refobjid = c.oid AND d.refobjsubid = a.attnum AND d.deptype IN ('a', 'i')
              AND pg_catalog.has_sequence_privilege(r.oid, seq.oid, 'USAGE')
        )
        WHEN q."Privilege" = 'ENABLE RLS' THEN c.relkind IN ('r','p') AND c.relrowsecurity
        WHEN q."Privilege" = 'FORCE RLS' THEN c.relkind IN ('r','p') AND c.relforcerowsecurity
        WHEN c.relkind IN ('r','p','v','m','f') THEN CASE
            WHEN q."Kind" = 'COLUMN' THEN a.attnum IS NOT NULL AND pg_catalog.has_column_privilege(r.oid, c.oid, a.attnum, q."Privilege")
            WHEN q."Privilege" = 'DELETE' THEN pg_catalog.has_table_privilege(r.oid, c.oid, 'DELETE')
            WHEN q."Privilege" = 'SELECT ANY COLUMN' THEN pg_catalog.has_any_column_privilege(r.oid, c.oid, 'SELECT')
        END
    END) IS NOT TRUE
)
SELECT kind, object, col, privilege FROM missing
ORDER BY kind COLLATE "C", object COLLATE "C", col COLLATE "C", privilege COLLATE "C"
