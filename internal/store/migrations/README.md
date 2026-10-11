# Schema migrations

Each `NNN_name.sql` file runs once, in filename order, inside its own
transaction, recorded in `schema_migrations` (`internal/store/migrate.go`,
`applyMigration`). Migrations are forward-only: there is no down path, and
rollback across one is a `pg_dump` restore (`docs/runbooks/upgrade.md`).

Rules for new migrations:

- **Expand-only while old replicas can still run.** A rolling upgrade
  keeps the previous binary alive next to the new schema: never drop or
  reshape a column an older replica still names — contract first, drop in
  a later release (016/017→019 is the model).
- **Idempotent.** `IF NOT EXISTS` / guarded `ALTER`s; a migration that
  half-applied must be re-runnable (the transaction normally guarantees
  atomicity, but keep the statements safe anyway).
- **`CREATE INDEX CONCURRENTLY` never goes in a migration file.** Every
  file runs inside the per-migration transaction, and `CONCURRENTLY`
  cannot run in a transaction. A plain `CREATE INDEX` on a non-trivial
  table takes a `SHARE` lock that blocks all writes for the build — the
  whole table stalls while replicas roll (migrations 020 and 023 do this
  to `connection_lease`/`launch_ticket`; see `docs/runbooks/upgrade.md`).
  For a table that can be large in production, ship the index as a
  documented `CREATE INDEX CONCURRENTLY IF NOT EXISTS` runbook step in
  `docs/runbooks/upgrade.md`, run by the operator *before* the upgrade
  and named to match the `IF NOT EXISTS` name the migration would have
  used — the migration's own `IF NOT EXISTS` statement is then a cheap
  no-op when the runbook ran and the slow path when it did not. This
  stands until the runner gains a no-transaction mode. Accept the lock
  only when the table is bounded small, and say why in the file header.
- **No data migrations that outlive the transaction budget.** Row rewrites
  belong in code paths or a documented operator step, not in a startup
  migration that every replica waits on.
