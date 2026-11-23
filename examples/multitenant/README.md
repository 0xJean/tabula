# Multitenant notes

From this directory, run the demo against its own PostgreSQL instance.

```sh
docker compose up -d --wait && go run .
```

The migration creates the table, a tenant policy and a non-owner application role. The Go program connects as that role, verifies the schema and statements, and checks that RLS is enabled and forced. It binds each tenant with a transaction-local setting and streams a hand-written query that leaves filtering to PostgreSQL.

```text
Schema, statements and RLS verified
north sees "Hello from north"
south sees "Hello from south"
```

The published port defaults to localhost port 55432. Set `TABULA_DEMO_PORT` for both commands to choose another port. Run `docker compose down -v` to discard the example database.
