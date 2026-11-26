# Fixture login labels and orphaned test databases

Test processes sharing a disposable PostgreSQL cluster can label their logins and bind the lifetime of a clone to the process login's existing maintenance session.

## Public API

- `LoginRoleOptions.Label` accepts an empty string or 1–25 ASCII lowercase letters, digits and underscores. Invalid labels return an error. The generated name is `tabula_login_<24 random hex digits>[_<label>]`. Each acquisition draws fresh randomness, including when two workers supply the same label. The longest name occupies 63 bytes. The empty label produces the v0.3.8 name.
- `Fixture.CloneWithLease(ctx, installation, login)` creates a clone backed by a live `*LoginRole`. Separate pools may point to the same maintenance database. It uses the same clone names and installation cache as `Clone`. The host grants any required CONNECT privileges and calls `Database.Drop` during normal cleanup.
- `Fixture.PruneClones(ctx, maxIdle)` reaps abandoned leased clones. Zero requests immediate collection. A positive grace period starts when a sweep first observes that the lease has ended and the database has no connections. An observed connection clears the idle timestamp. The general `Prune` includes this sweep with its supplied grace period and reports dropped database names.

## Ownership and lifetime

The fixture publishes a versioned database comment through the login's pinned maintenance session before returning the clone. The mark binds the maintenance database OID, clone OID and name, and full generated login name. A label is for display. Lease identity comes from the full login name, including its random component. The lease can be checked even after the role has been removed.

The collector verifies the mark in its maintenance database, tries the login's advisory lock without waiting and rechecks identity and connections under a per-clone lock. Explicit cleanup of a leased handle uses that same clone lock. A worker with zero business connections retains all its clones while the lease is live. Automatic cleanup uses ordinary PostgreSQL `DROP DATABASE` so a connecting client can prevent the drop. It does not invoke a host backend that might force-disconnect clients.

PostgreSQL creates the database and records its comment in separate commands. A process killed between those commands can leave an unmarked database. Automatic collection preserves it, just as it preserves ordinary `Clone` databases, v0.3.8 clones and names with copied or mismatched ownership marks. The maintenance database and its catalog comments must be controlled by trusted administrators.
