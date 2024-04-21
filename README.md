# compose

**Warning: This package is still in development and things keep changing
so things may break.**

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

In the spirit of keeping it [DRY](https://en.wikipedia.org/wiki/Don%27t_repeat_yourself), this package standarizes everything so we can use the same components on every application.

## Instalation

---

```
go get -u github.com/Vanclief/compose
```

## Usage

---

- [config](https://github.com/vanclief/compose/docs/config.md) - Loading env/ settings

## Dependencies

---

- [ez](https://github.com/vanlcief/ez) - Better error handling & error stack traces
- [zerolog](https://github.com/rs/zerolog) - Lightweight and minimalistic logging
- [promtail-go](https://github.com/carlware/promtail-go) - Promtail + Grafana = Awesome logs
- [echo]() - HTTP router
- [ozzo-validation]() - Struct validation
- [viper]() - Env variables & config files
