# compose

**Warning: This package is still in development and things keep changing
so things may break slightly**

A collection of opinionated modules created for building golang applications
quicker while keeping best practices.

## Why

---

Most applications have many components in common:

- Loading config & env variables
- Logging
- Keeping a state
- Long term storage
- APIs
- Authentication
- Role management

In the spirit of keeping it [DRY](https://en.wikipedia.org/wiki/Don%27t_repeat_yourself), this package standardizes everything so we can use the same components on every application.

## Instalation

---

```
go get -u github.com/vanclief/compose
```

## Usage

---

- [config](https://github.com/vanclief/compose/docs/config.md) - Loading env/ settings

## Schema

---

`ctrl.WithPostgres(cfg, schema)` and `ctrl.WithSQLite(cfg, schema)` call `db.Migrate(ctx, schema)` at boot. A `relational.Schema` declares everything the database must match:

- `Models`: bun models. A model can also declare what tags cannot express by implementing `Indexes() []relational.Index` or `Constraints() []relational.Constraint` (Postgres only). `Def` is the SQL after `ON table` or after `ADD CONSTRAINT name`.
- `Extensions`: Postgres extensions, created first.
- `Steps`: named `relational.Step`s, in order. Append new ones at the bottom and delete old ones from the top.
- `Floors`: the last deleted name of each deletion of steps, oldest first.
- `Mode`: `relational.Report` (default) or `relational.Apply`.

```go
func (*Transfer) Indexes() []relational.Index {
	return []relational.Index{{Name: "transfers_account_idx", Def: "(account_id) WHERE account_id IS NOT NULL"}}
}
```

An empty database is created at the declared shape in one transaction. Every step is recorded as applied without running. On an existing database, `Migrate` takes a lock (Postgres advisory lock), runs pending steps on the connection that holds it, and compares the declarations with the live catalog. It reports what is declared but missing, what is declared with another definition and what exists but is undeclared. `Migrate` never drops or alters anything.

- `Report` logs that plan and changes nothing else.
- `Apply` also creates what is safe while an older binary still serves: new tables with their indexes and constraints, nullable or constant-default columns (Postgres only), and non-unique indexes on tables up to `MaxIndexTableSize` (built `CONCURRENTLY` on Postgres, and refused when the build waits over `IndexBuildLockTimeout` for another transaction). Everything else is refused, including an index whose name another table already uses, and the log includes the statement an operator could run. Any different definition fails the boot. Switch to `Report` to boot anyway during an incident.

Use a step for anything a diff cannot decide: renames, backfills, type changes, drops, NOT NULL without a default, and unique indexes or constraints on populated tables (`relational.ApplyDeclared(ctx, db, models, name)` installs a declaration by name). Use `relational.ExecShort` for DDL that takes an exclusive lock. Steps are recorded in `schema_steps` on success. A failed step stays pending and is retried on the next boot. Nothing is rolled back automatically.

To delete old steps, delete a contiguous prefix once every database has recorded them, and append the last deleted name to `Floors`. A database that recorded no floor and no live step is refused. A database migrated by a newer release boots without running anything: a fresh database records every floor, so a release rolled back across a deletion still recognises the databases the newer release created. Keep the older floors for as long as you would roll back to a release that needs them, they are names only. Deleting steps without appending to `Floors` makes the release treat a database whose newest recorded step was deleted as newer than itself: it runs no steps, creates nothing and logs an error on every boot. `db.Plan(ctx, schema)` compares without running steps or changing anything (for CI or a `schema plan` command). `db.ResetSchema(ctx, schema)` recreates the declared schema for tests.

- Renaming a table leaves its sequence default and primary key named after the old table. The renaming step must rename them too, or they show as different forever.
- `ApplyDeclared` builds indexes and adds constraints without `CONCURRENTLY` or `NOT VALID`. On a populated table, the step author chooses the moment.
- The Postgres session advisory lock and the pinned connection need a direct connection or pgbouncer in session mode.
- SQLite has no schema lock. Two processes creating the same new database file at once race.

## Dependencies

---

- [ez](https://github.com/vanlcief/ez) - Better error handling & error stack traces
- [zerolog](https://github.com/rs/zerolog) - Lightweight and minimalistic logging
- [promtail-go](https://github.com/carlware/promtail-go) - Promtail + Grafana = Awesome logs
- [echo]() - HTTP router
- [ozzo-validation]() - Struct validation
- [viper]() - Env variables & config files
