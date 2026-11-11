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

### CLI

The `compose` binary previews API reference pages:

```
go install github.com/vanclief/compose/cmd/compose@latest
compose reference -head openapi.yaml -base previous.yaml -serve 127.0.0.1:8090
```

This serves the reference page of `openapi.yaml` with what changed since `previous.yaml`, re-rendered on every reload. Use `-out page.html` instead of `-serve` to write a standalone file.

## Usage

---

- [config](https://github.com/vanclief/compose/docs/config.md) - Loading env/ settings
- [concurrent writes](docs/concurrent-writes.md) - why two requests can undo each other's changes to one row, and the helpers that prevent it: `relational.Patch` writes only the columns a request sent, `relational.Mutate` and `relational.LockRow` lock the row and read it again before the write
- [migrations](docs/migrations.md) - how the migration runner and its own connection pool behave, and the schema baseline that replaces old migration files
- [scheduler](docs/scheduler.md) - running recurring jobs on one instance per minute boundary when an app runs several instances
- [testing](docs/testing.md) - running an app's tests with no AWS credentials: the in-memory S3 client, the recording SES client and the test database helpers

## Dependencies

---

- [ez](https://github.com/vanlcief/ez) - Better error handling & error stack traces
- [zerolog](https://github.com/rs/zerolog) - Lightweight and minimalistic logging
- [echo]() - HTTP router
- [viper]() - Env variables & config files
