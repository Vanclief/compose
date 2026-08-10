# Schema & Migration Guidelines

`InitSchema` keeps two cooperating sources of truth. Breaking their contract
is the main way to corrupt a deployment, so read this before touching either.

1. **Model structs (plus idempotent boot steps) describe the complete current
   schema.** Fresh databases are built exclusively from them — migrations are
   never executed on a fresh database, only recorded as applied.
2. **Migrations describe deltas for databases created by older releases.**
   They exist to carry an existing deployment from one release's schema to the
   next.

## Rules

### A migration must never be the only home of a schema element

Whatever a migration does must also exist in the fresh-install path, in the
same commit:

- **Columns, types, defaults, constraints** → the model struct's `bun` tags.
- **Indexes, views, triggers** → an idempotent boot step the application runs
  after `InitSchema` (e.g. `CREATE INDEX IF NOT EXISTS` via bun's
  `NewCreateIndex`), since `CreateTables` does not derive these from structs.
- **Seed or reference data** → application bootstrap logic, not a migration.
  A fresh install skips migrations, so seeds that live only there will be
  silently missing.

If a change only makes sense as a migration (backfills, data repairs on
existing rows), it needs no struct counterpart — fresh databases have no rows
to repair. That is the only exception.

### Migrations must run on every dialect the application supports

How portable a migration must be is the application's decision, not this
library's — compose serves single-dialect and multi-dialect consumers
alike:

- An application deploying on one dialect may freely use that dialect's
  SQL in its migrations.
- An application supporting several dialects (e.g. SQLite locally and
  Postgres hosted) must write migrations that run on all of them: prefer
  bun's query and DDL builders, which render per-dialect, and branch on
  `db.Dialect().Name()` when raw SQL is unavoidable.
- Where SQLite is among the supported dialects, note it cannot express
  some operations (`ALTER COLUMN ... TYPE`, dropping constraints); such
  changes need the rebuild-table pattern (create new, copy, drop, rename).

### Unrecorded databases are assumed current

A database with no recorded migrations — created by an older compose flow,
by `InitSchema` before the project had migrations, or by hand — is treated
as already matching the current structs: its missing tables are created and
all registered migrations are recorded as applied without running. This is
correct whenever structs and deployment moved together. If you are adopting
`InitSchema` on a database whose schema is genuinely **older** than the
structs, reconcile it manually first; `InitSchema` will not detect that
case.

### Never rename an applied migration

A renamed migration looks missing under its old name and pending under its
new one, and running it would replay its changes. `InitSchema` refuses to
start when it sees that combination (missing applied migrations alongside
unknown pending ones). Squashing — removing old migrations from the
registry — is a two-step, deliberate operation: ship the squash on its own
(boot continues with a warning), then run `PruneMigrationRecords` once to
delete the stale records. Until pruned, adding any new migration is
indistinguishable from a rename and will be refused.

Prune only after **every** instance runs a release without the removed
migrations. An older binary still registering them would see the pruned
records as pending and replay them — the same reason every migration
framework tells you to keep squashed migrations around until the whole
fleet has moved past them.

## Accepted tradeoffs

Deliberate decisions, not defects. Review findings that rediscover these
should be closed by pointing here.

- **No freshness marker beyond migration records.** A database with zero
  recorded migrations is assumed to match the current structs (see
  "Unrecorded databases are assumed current"). Detecting an older schema
  would require a second source of truth that can drift; the contract is
  documented instead.
- **A stale migration lock is surfaced, never auto-cleared.** A crash while
  migrating leaves the lock held; the next `InitSchema` fails loudly and
  `VerifySchema` reports `Locked` instead of trusting records the crashed
  run may have left behind (bun records a migration before executing it).
  Clearing the lock is a human decision, because the state is
  indistinguishable from a crash mid-DDL, which deserves inspection.
- **Prune sequencing is the operator's job.** Pruning while older binaries
  still register the removed migrations replays them; every migration
  framework has this property.
- **Boots overlapping a running migration are the caller's responsibility.**
  Bun records before executing, so sequencing `InitSchema` ahead of
  dependent processes cannot be enforced from inside the library.

## What InitSchema guarantees

- Database with no recorded migrations → schema from structs + all
  migrations recorded as applied, atomically (single transaction; a crash or
  concurrent bootstrap either commits everything or nothing). Existing
  tables are left untouched and assumed current.
- Recorded migrations, nothing pending → no-op, after confirming the
  migration lock is free. A held lock fails loudly: the records may have
  been left by a run that crashed mid-execution.
- Pending migrations → executed while holding bun's migration lock. A
  concurrent `InitSchema` fails fast instead of waiting, and can never
  corrupt: the loser cannot mark or roll back the winner's work.
- A failed migration that rolls back cleanly releases the lock — retrying
  needs no manual cleanup. A failed migration whose rollback also fails
  keeps the lock held as the tombstone, because the schema state is
  unknown and later boots must refuse rather than trust the records.
- Missing and pending migrations together → hard error, never a guess.
  `PruneMigrationRecords` is the explicit, operator-driven resolution after
  a squash.

Coordination is the application's job. Run `InitSchema` from one place — a
deploy step, a migrate command, or a single owning process — and gate other
entry points with `VerifySchema`. Note that bun records a migration before
executing it, so a process that boots while a migration is mid-flight may
read the schema as current; sequencing `InitSchema` before dependent
processes start is the caller's responsibility.
