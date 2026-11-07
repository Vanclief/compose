# Migrations

compose runs bun migrations with `relational.DB.RunMigrations`. Every migration is a Go or SQL
file registered on a `migrate.Migrations`, and bun records the ones it has run in the
`bun_migrations` table.

## How the runner behaves

A migration is recorded as applied only after its `Up` returns without error. A container killed
halfway through a multi-statement `Up` leaves nothing in the ledger, so the next start runs it
again from the top. Every migration must therefore be safe to run twice (`ADD COLUMN IF NOT
EXISTS`, `CREATE INDEX IF NOT EXISTS`, a catalog check before a rename).

A failed migration returns its error and stays pending. Nothing is rolled back automatically: a
`Down` that drops a column while the previous release is still serving traffic is worse than a
retry. Rolling back is an explicit call to `RollbackLastMigration`, which undoes the last group
and unmarks each migration only after its `Down` succeeds.

A migration that is retried on the next start lands in a new group. After a deploy that ran A and
failed on B, A is in one group and the fixed B in the next, so `RollbackLastMigration` undoes only
B. Roll back A with a second call if the deploy must be undone as a whole.

bun matches ledger rows to files by name, so two Go files that share a name (two migrations
registered on the same day with an 8-digit prefix) are one migration to it: if the second fails
after the first succeeded, the retry skips it, and a rollback of the pair runs both `Down`s but
deletes only one of their two rows, so neither ever runs again. Give every migration a unique
name; a 14-digit timestamp prefix avoids the problem.

Migrations run on their own pool, `relational.DB.MigrationDB`, not on the one the app queries
with. That is the `db` a migration's `Up` and `Down` receive; a migration that reaches for the
app's own handle instead runs on the main pool with its limits. Only
`postgres.ConnectToDatabase` opens the second pool: on SQLite, or on a `relational.DB` built by
hand, `MigrationDB` is nil and migrations run on the main one, so check it for nil before
calling anything on it. On Postgres the pool's connections have no
`statement_timeout` and no socket read timeout, so a long `CREATE INDEX` or backfill runs to
completion, and they have their own `lock_timeout`, `migrationLockTimeout` in the Postgres config
(milliseconds; 0 or unset means the 10 s default, there is no off). A migration that waits longer
than that for a lock held by another session fails, the boot fails, and the next start retries it
instead of hanging the deploy. The server also checks every 5 s that the client is still there
(`client_connection_check_interval`: Postgres 14 or newer on Linux, 15 or newer on macOS and BSD,
never on Windows; a server that refuses it fails the pool's first statement, the baseline load or
`RunMigrations` or `RollbackLastMigration` whenever a migration is registered, not the connect),
so a container killed mid-migration does not leave a backend holding locks until the statement
ends. The pool shares the main pool's model registry, so a migration may use registered models,
and it drops its idle connections after 30 s, so it holds nothing once migrations are done.
`db.Close()` closes both pools.

A migration that runs longer than your platform's health window gets its container killed and
retried on every start. Run work that long out of band, not as a migration.

`CREATE INDEX CONCURRENTLY` waits for older transactions and that wait counts against the lock
timeout. When it is cancelled it leaves an invalid index behind, which `IF NOT EXISTS` would then
skip forever. Write `DROP INDEX CONCURRENTLY IF EXISTS` before it so the retry rebuilds it, each as
its own `Exec` call, or separated by a `--bun:split` line in an `.up.sql` file: two statements in
one string run as one implicit transaction, which `CONCURRENTLY` refuses, and a `.tx.up.sql` file
cannot hold it at all.

## The schema baseline

Over the years the migration folder grows into hundreds of files that no database will ever run
again, because every database already has them.

The baseline keeps the folder short. The app commits one `pg_dump --schema-only` of its database
and deletes the migration files whose work that dump already holds. A fresh database (tests, a
local checkout, a new environment) loads the dump and runs only the files written after it. A
database that already has a `bun_migrations` table (production, staging) never reads the dump: it
is already in that state, so it only runs the files written after it, as before.

This is what Atlas calls a checkpoint and Flyway a baseline migration.

## Declaring it

```go
import _ "embed"

//go:embed schema.sql
var schemaSQL string

var baseline = relational.Baseline{
	SQL:   schemaSQL,
	Floor: "20240215", // highest name among the deleted migration files
}

err = db.RunMigrationsFromBaseline(baseline, migrations)
```

`Floor` is one migration name, the way Flyway's `baselineVersion` and an Atlas checkpoint version
are. A migration's name is what bun stores in the `name` column of `bun_migrations`: for a file
`20240215_add_user_email.go` registered with `MustRegister` that is the digits before the first
underscore, `20240215`, not the file name. Names are compared as strings, so every migration whose
name sorts at or below the floor is meant to be in the dump and its file must be gone. Everything
above the floor stays in the folder. A `Baseline` is either empty (no checkpoint yet) or has both
`SQL` and `Floor`; one without the other is refused.

Nothing may create tables before this call on a fresh database. The dump is a plain
`CREATE TABLE` per table, so a model table that already exists makes the load fail. Pass `nil`
models to `WithPostgres` and, if you still build tables from models, call `CreateTables` after
`RunMigrationsFromBaseline`: it then creates only the tables of models added after the checkpoint.
The component table helpers (`session.CreateTable`, `oauth.CreateTable`) move after the call too;
they are `IF NOT EXISTS`, so they are harmless there.
That reverses the old order, where model tables existed before migrations ran, so a migration that
touches a table newer than the dump must create it itself with `CREATE TABLE IF NOT EXISTS`.
Extensions are fine before the call when the role that loads the dump owns them or is a
superuser: pg_dump writes `CREATE EXTENSION IF NOT EXISTS` and then `COMMENT ON EXTENSION`, which
only the owner or a superuser may run. An extension the platform created first fails the load for
an app role.

With an empty `Baseline` the call is `RunMigrations` plus the Postgres check; steps 1 to 3 below
only apply once a checkpoint exists. With a checkpoint, `RunMigrationsFromBaseline` does, in order:

1. Refuses a registered migration whose name is at or below `Floor`. A migration is either at or
   below the floor (file deleted) or above it (file kept), never both, so a rollback can never
   reach into the baseline. When the refused file is one the dump already holds, delete it. When
   it was written on a branch before the checkpoint and merged after it, the dump does not hold
   it: rename it above the floor so it runs.
2. On a database without a `bun_migrations` table, loads `SQL` in one transaction and inserts the
   floor name into `bun_migrations` as group 1. The dump must include the `bun_migrations` table,
   or the load is rolled back and refused with `ez.ECONFLICT`; a dump of a real database always
   does. When Postgres refuses a statement of the dump, a table or other object that already
   exists is `ez.ECONFLICT`, a setting, role, type or collation the server does not have
   (SQLSTATE 42704) is `ez.EINVALID`, and anything else, such as an extension the server does
   not have or one another role owns, is `ez.EINTERNAL`. The text of those errors, `Error()`,
   has the Postgres message with its SQLSTATE and, for `ez.ECONFLICT` and `ez.EINVALID`,
   compose's hint on what to fix; Postgres's own DETAIL and HINT are not in that text, they stay
   on the `pgdriver.Error` in the error chain.
3. On a database that has the table, checks that the floor name is in it and refuses with
   `ez.ECONFLICT` if it is not. That database is older than the baseline and the files that would
   bring it forward are gone. A database whose ledger exists but is empty is refused the same way.
   On production, restore a newer backup or deploy an older release of the app. On a local
   database, drop it and let the baseline load.
4. Runs the pending migrations with `RunMigrations`. An empty folder is fine and is the normal
   state right after a checkpoint; `RollbackLastMigration` on an empty folder is a no-op too. A
   `//go:embed *.sql` pattern with nothing to match fails the build, though, and so does an
   embedded directory left empty: keep the dump's `schema.sql` in that directory (`Discover`
   skips files not named `.up.sql` or `.down.sql`) or keep one `.up.sql` migration above the
   floor there.

The dump is executed on a migration pool connection that is discarded afterwards, so the `SET`
and `search_path` lines pg_dump emits never reach the migrations that follow. The `\restrict` line
pg_dump 17.6, 16.10, 15.14, 14.19, 13.22 and later open the dump with, and the `\unrestrict` line
that closes it, are dropped before execution; they are recognised only there, at the start and the
end, so a line of that shape inside a function body, or a dump without the frame, is passed through
as is. `RunMigrationsFromBaseline` is Postgres only, even with an empty `Baseline`; an app whose
tests run on SQLite keeps calling `RunMigrations` there.

Two instances booting together against the same fresh database both try the load; the second fails
on a duplicate object and passes on its next start, since the first has committed the floor by then.
Running migrations from two instances at once is not supported either, because bun's ledger has no
unique name. Run one instance while a migration is pending.

## Taking a checkpoint

Do this whenever the folder has grown and every database you run has applied every file in it,
which for a single production database is right after a deploy. Adding a migration never moves
the floor; only a checkpoint does.

1. Dump the schema of the database that is the source of truth, into the directory of the
   package that embeds it:

   ```
   pg_dump --schema-only --no-owner --no-privileges "$DATABASE_URL" > schema.sql
   ```

   `--no-owner` and `--no-privileges` keep owner and grant role names out of the file, so a test
   database owned by a different user can load it. A row-level security policy keeps the roles
   it names (`CREATE POLICY ... TO app_role`); create those roles wherever the dump loads. pg_dump
   refuses a server newer than itself, and it writes `SET` lines of its own version that only
   that major or a newer one loads: a file from pg_dump 17 or later carries
   `SET transaction_timeout`, which Postgres 16 rejects as an unrecognized configuration
   parameter. So dump with the pg_dump of the dumped server's major, and run every database that
   loads the dump, tests and local checkouts, on that major or a newer one.

2. Set `Floor` (on the first checkpoint, in the `Baseline` you declare in step 4) to the highest
   name, in bun's string order, among the migrations you are about to delete, the `name` column
   value, which for a file `20240215_add_user_email.go` is `20240215`.
   Confirm it on the source database before merging; a typo here is caught only when an existing
   database boots:

   ```
   SELECT 1 FROM bun_migrations WHERE name = '20240215';
   ```

3. Delete every migration file named at or below the floor, and the tests that exercise those
   files by name.

   The dump holds schema only: no rows, no `GRANT` because of `--no-privileges`, and never roles
   or `ALTER DATABASE ... SET`. A migration that inserts rows a fresh database needs (lookup values,
   built-in roles, a default settings row), or grants a privilege, cannot be deleted: move its
   statement into a migration above the floor, or keep the floor below it. A moved statement runs
   on databases that already ran the deleted file, so it must tolerate its own rows being there
   (`INSERT ... ON CONFLICT DO NOTHING`); tests start from the dump and would not catch a plain
   `INSERT` that fails on production with a duplicate key. A backfill over rows that already exist
   is a no-op on a fresh database and can go.

4. The first checkpoint only: switch the boot over. Embed the dump, declare the `Baseline` and
   replace the `RunMigrations` call with `RunMigrationsFromBaseline`, as under "Declaring it".
   Pass `nil` models to `WithPostgres` and move `CreateTables` and the component `CreateTable`
   helpers after the call, so nothing creates a table before the load; a forgotten one fails the
   first fresh boot with the "must have no tables" message. Check who owns the extensions the
   dump recreates.

5. Commit the dump, the `Floor` change, the deletions and, on the first checkpoint, the switch
   together. A commit that has deleted the files but still boots with `RunMigrations` leaves
   every fresh database with no way to build the tables those files created. Later checkpoints
   repeat steps 1 to 3 and 5.

Keep the newest migration's file out of the deletion, with the floor set to the one before it, if
you want `RollbackLastMigration` to be able to undo it. The dump already contains its effect, so
on a fresh database it runs a second time over a schema that already has it: write every migration
so that running it twice is harmless (`ADD COLUMN IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`, a
catalog check before a rename).

With ten migrations and the wish to keep only the tenth: dump, set `Floor` to the ninth name,
delete files one to nine.

## Writing a change after the switch

Edit the model, write one migration file as before, run the tests. The test database loads the
dump and runs that one file, before any model table is built, so a file that touches a table the
dump does not have creates it with `CREATE TABLE IF NOT EXISTS`. Use the statement bun generates
for the model, `db.NewCreateTable().Model((*T)(nil)).IfNotExists().String()`, also what the
verbose log prints for `CreateTables`, so the migration builds the table `CreateTables` would. Do
not copy the table from the dump: pg_dump writes the id sequence as a `CREATE SEQUENCE` and the
primary key and unique constraints as `ALTER TABLE ... ADD CONSTRAINT`, and neither has
`IF NOT EXISTS`. Deploy; production runs that one file. The floor stays where it is.
When you feel like it, take a new checkpoint and the folder is short again.

## Restore floor

A backup taken before the floor migration ran cannot be upgraded by a release that has deleted
the files: the floor check fails on boot with the floor name in the message. That is by design.
The floor moves forward with every checkpoint, so take one only when you no longer need to restore
a backup older than the new floor.

The check looks at the floor name only. A backup that has the floor but skipped an older migration
passes, which happens when a migration with an older timestamp was merged after a newer one had
already run and the backup was taken in between, or in the duplicate-name case above, where the
ledger holds the name but not the second file's work. bun runs everything pending in name order at
every boot, so after any deploy the set below the floor is complete.
