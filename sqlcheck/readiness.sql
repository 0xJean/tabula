WITH target AS (
    SELECT oid FROM pg_catalog.pg_roles WHERE rolname::text = $1
), requested AS (
    SELECT DISTINCT * FROM pg_catalog.jsonb_to_recordset($2::jsonb) AS need(
        "Kind" text, "Object" text, "Column" text, "Privilege" text,
        "Schema" text, "Name" text, "Args" jsonb, "Routine" jsonb, "KeyColumns" jsonb,
        "WithSchema" text, "WithName" text, "WithColumn" text
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
    LEFT JOIN pg_catalog.pg_namespace pn ON pn.nspname::text = q."WithSchema"
    LEFT JOIN pg_catalog.pg_class pc ON pc.relnamespace = pn.oid AND pc.relname::text = q."WithName"
    LEFT JOIN pg_catalog.pg_attribute pa ON pa.attrelid = pc.oid AND pa.attname::text = q."WithColumn"
        AND pa.attnum > 0 AND NOT pa.attisdropped
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
              ) AND CASE WHEN q."Privilege" = 'EXECUTE' THEN pg_catalog.has_function_privilege(r.oid, p.oid, 'EXECUTE')
                    ELSE p.prokind = 'f' AND p.provariadic = 0
                      AND p.proretset = (q."Routine"->>'ReturnsSet')::boolean
                      AND p.prosecdef = (q."Routine"->>'SecurityDefiner')::boolean
                      AND (NOT (q."Routine"->>'ReadOnly')::boolean OR p.provolatile IN ('i','s'))
                      AND p.prorettype = (
                        SELECT t.oid FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace ns ON ns.oid = t.typnamespace
                        WHERE ns.nspname::text = q."Routine"->>'ResultSchema' AND t.typname::text = q."Routine"->>'ResultName'
                      )
                END
        )
        WHEN q."Kind" = 'KEY' THEN c.relkind IN ('r', 'p')
            AND (c.relkind = 'p' OR NOT EXISTS (
                SELECT FROM pg_catalog.pg_inherits h WHERE h.inhparent = c.oid
            )) AND EXISTS (
                SELECT FROM pg_catalog.pg_constraint k
                JOIN pg_catalog.pg_index ix ON ix.indexrelid = k.conindid
                JOIN pg_catalog.pg_class idx ON idx.oid = ix.indexrelid
                JOIN pg_catalog.pg_am am ON am.oid = idx.relam
                WHERE k.conrelid = c.oid AND k.contype = 'p' AND k.convalidated
                  AND NOT k.condeferrable AND NOT k.condeferred
                  AND ix.indisprimary AND ix.indisunique AND ix.indimmediate
                  AND ix.indisvalid AND ix.indisready AND ix.indpred IS NULL AND ix.indexprs IS NULL
                  AND am.amname = 'btree'
                  AND ARRAY(SELECT att.attname::text FROM pg_catalog.unnest(k.conkey) key(attnum)
                    JOIN pg_catalog.pg_attribute att ON att.attrelid = c.oid AND att.attnum = key.attnum
                    WHERE att.attnotnull AND NOT att.attisdropped ORDER BY att.attname::text COLLATE "C")
                    = ARRAY(SELECT col FROM pg_catalog.jsonb_array_elements_text(q."KeyColumns") cols(col) ORDER BY col COLLATE "C")
                  AND NOT EXISTS (
                    SELECT FROM ROWS FROM (pg_catalog.unnest(ix.indclass::oid[]), pg_catalog.unnest(ix.indcollation::oid[]), pg_catalog.unnest(ix.indkey::smallint[])) WITH ORDINALITY AS key(opclass, collid, attnum, pos)
                    JOIN pg_catalog.pg_opclass oc ON oc.oid = key.opclass
                    JOIN pg_catalog.pg_namespace ns ON ns.oid = oc.opcnamespace
                    JOIN pg_catalog.pg_attribute att ON att.attrelid = c.oid AND att.attnum = key.attnum
                    LEFT JOIN pg_catalog.pg_collation coll ON coll.oid = key.collid
                    WHERE key.pos <= ix.indnkeyatts AND (ns.nspname <> 'pg_catalog' OR NOT oc.opcdefault
                      OR key.collid <> att.attcollation OR NOT coalesce(coll.collisdeterministic, true))
                  )
            )
        WHEN q."Kind" = 'COMPARISON' THEN a.attnum IS NOT NULL
            AND (a.attcollation = 0 OR EXISTS (
                SELECT FROM pg_catalog.pg_collation coll WHERE coll.oid = a.attcollation AND coll.collisdeterministic
            ))
            AND EXISTS (
                SELECT FROM pg_catalog.pg_type typ JOIN pg_catalog.pg_namespace ns ON ns.oid = typ.typnamespace
                WHERE typ.oid = a.atttypid AND ns.nspname = 'pg_catalog'
            )
            AND (q."WithColumn" = '' OR pa.attnum IS NOT NULL
                AND (pa.attcollation = 0 OR EXISTS (
                    SELECT FROM pg_catalog.pg_collation coll WHERE coll.oid = pa.attcollation AND coll.collisdeterministic
                ))
                AND EXISTS (
                    SELECT FROM pg_catalog.pg_type typ JOIN pg_catalog.pg_namespace ns ON ns.oid = typ.typnamespace
                    WHERE typ.oid = pa.atttypid AND ns.nspname = 'pg_catalog'
                )
                AND (a.attcollation = pa.attcollation
                    OR a.attcollation IN (0, 'pg_catalog.default'::pg_catalog.regcollation)
                    OR pa.attcollation IN (0, 'pg_catalog.default'::pg_catalog.regcollation)))
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
            WHEN q."Privilege" = 'UPDATE ANY COLUMN' THEN pg_catalog.has_any_column_privilege(r.oid, c.oid, 'UPDATE')
        END
    END) IS NOT TRUE
)
SELECT kind, object, col, privilege FROM missing
ORDER BY kind COLLATE "C", object COLLATE "C", col COLLATE "C", privilege COLLATE "C"
