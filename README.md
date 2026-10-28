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

## Dependencies

---

- [ez](https://github.com/vanlcief/ez) - Better error handling & error stack traces
- [zerolog](https://github.com/rs/zerolog) - Lightweight and minimalistic logging
- [echo]() - HTTP router
- [viper]() - Env variables & config files
