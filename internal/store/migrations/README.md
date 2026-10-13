# Schema migrations

`0001_baseline.sql` is the whole v1 schema. Pre-1.0 databases are not
upgraded: a v1 install starts from the baseline on an empty database.
The committed snapshot `internal/store/testdata/schema.snapshot.sql`
pins the result (`TestBaseline_SchemaMatchesSnapshot`; regenerate it
with `TCDI_UPDATE_SNAPSHOT=1` after a deliberate schema change).

Each `NNNN_name.sql` file runs once, in filename order, inside its own
transaction, recorded by its integer version in `schema_migrations`
(`internal/store/migrate.go`, `applyMigration`).

Rules for new migrations:

- **Numbering.** One baseline. New files start at `0002_*.sql` and use a
  four-digit prefix.
- **Forward-only.** There is no down path; rollback across a migration is
  a `pg_dump` restore.
- **Expand-only while old replicas can still run.** A rolling upgrade
  keeps the previous binary alive next to the new schema: never drop or
  reshape a column an older replica still names — contract first, drop in
  a later release.
- **Idempotent.** `IF NOT EXISTS` / guarded `ALTER`s; a migration that
  half-applied must be re-runnable (the transaction normally guarantees
  atomicity, but keep the statements safe anyway).
- **No `CONCURRENTLY`.** Every file runs inside the per-migration
  transaction, and `CREATE INDEX CONCURRENTLY` cannot run in a
  transaction. A plain `CREATE INDEX` on a non-trivial table takes a
  `SHARE` lock that blocks all writes for the build. For a table that can
  be large in production, ship the index as a documented
  `CREATE INDEX CONCURRENTLY IF NOT EXISTS` runbook step, run by the
  operator *before* the upgrade and named to match the `IF NOT EXISTS`
  name the migration would have used — the migration's own statement is
  then a cheap no-op when the runbook ran. Accept the lock only when the
  table is bounded small, and say why in the file header.
- **No data migrations that outlive the transaction budget.** Row rewrites
  belong in code paths or a documented operator step, not in a startup
  migration that every replica waits on.
