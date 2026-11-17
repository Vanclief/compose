# compose

Building blocks for Go backend services, covering the parts most of them share: configuration, logging, a REST layer on [Echo](https://echo.labstack.com) whose routes generate and enforce their own OpenAPI document, an MCP server over that API, authentication, a scheduler, and PostgreSQL helpers on [bun](https://bun.uptrace.dev).

It is the base of services that run in production, and it is published as it is: an opinionated library that is still being shaped. If you build this kind of service in Go and have views on how these parts should work, issues and design feedback are welcome.

```
go get github.com/vanclief/compose@latest
```

## A small example

One typed route, the OpenAPI document generated from it, and an MCP server over the same API:

```go
package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/labstack/echo/v5"
	"github.com/rs/zerolog"

	"github.com/vanclief/compose/components/mcp"
	"github.com/vanclief/compose/components/rest"
	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/requests"
)

type GreetRequest struct {
	requests.NoRules
	Name string `json:"name" required:"true" minLength:"1" maxLength:"40"`
}

type GreetResponse struct {
	Message string `json:"message"`
}

func greet(ctx context.Context, req *GreetRequest) (GreetResponse, error) {
	return GreetResponse{Message: "Hello, " + req.Name}, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := zerolog.New(os.Stdout).With().Timestamp().Logger()

	e := echo.New()
	e.HTTPErrorHandler = rest.ErrorHandler()
	rest.Configure(e, log, rest.WithBodyLimit(1<<20))

	// A route is a typed function. Its types become the OpenAPI schema, and
	// every request is validated against that schema before greet runs.
	reg := api.New(e, api.Info("Hello API", "1.0.0"), api.Public())
	api.POST(reg.Group("/greetings"), "", "create", greet, api.Doc("Greet someone"))
	reg.Ready()

	spec, err := reg.Spec(api.V31)
	if err != nil {
		log.Fatal().Err(err).Msg("Could not build the OpenAPI document")
	}
	e.GET("/openapi.json", func(c *echo.Context) error {
		return c.Blob(http.StatusOK, "application/json", spec)
	})

	// The same document, served to AI agents as an MCP server. Tool calls are
	// dispatched to e in-process, so they pass the same validation.
	agents, err := mcp.New(mcp.Config{Spec: spec, Handler: e})
	if err != nil {
		log.Fatal().Err(err).Msg("Could not build the MCP server")
	}
	e.Any("/mcp", echo.WrapHandler(agents.Handler()))

	rest.Serve(ctx, e, log, "8080")
}
```

```
$ curl -s localhost:8080/greetings -H 'Content-Type: application/json' -d '{"name":"Ada"}'
{"message":"Hello, Ada"}

$ curl -s localhost:8080/greetings -H 'Content-Type: application/json' -d '{"name":""}'
{"error":{"code":"invalid","message":"name must be at least 1 character","request_id":"3dbc422c-…","fields":[{"in":"body","name":"name","message":"must be at least 1 character"}]}}
```

`/openapi.json` serves the OpenAPI 3.1 document, with `name` as a required string of 1 to 40 characters. An MCP client connected to `/mcp` finds `greetings.create` with `search_functions` and calls it with `call_function`; an empty name comes back to the agent as `error invalid: name must be at least 1 character`, followed by the function's signature.

## What is in it

Packages are imported one by one; import paths start with `github.com/vanclief/compose/`.

| Package | What it does |
| --- | --- |
| **HTTP and API description** | |
| `components/rest` | Echo v5 set up for a service: a request ID made by the server, a request log, panic recovery, CORS, a body limit, one JSON error envelope and graceful shutdown. |
| `components/rest/api` | A typed route registry. A handler is `func(ctx, *Req) (Res, error)`. The registry mounts it on Echo, publishes OpenAPI 3.1 and 3.0.3 documents, and validates every request against the published schema before the handler runs. |
| `components/rest/apitest` | Contract tests: checks the responses your tests record against the document. |
| `components/rest/handler`, `components/rest/requests` | The older handler style, without the registry: a request object, a `Validate()` method per body, and the same error envelope. |
| `components/rest/generator`, `components/rest/spectest` | For apps on the older handler style: builds an OpenAPI 3.0 document by reading the app's source, and checks that document against the running app from its tests. |
| `components/rest/reference` | Renders an OpenAPI 3.x document as a self-contained reference page, with no CDN, and compares two documents, flagging the changes that break existing clients so CI can gate on them. |
| **AI agents** | |
| `components/mcp` | Serves an API described by OpenAPI as an MCP server. It has a small fixed set of tools (`whoami`, `search_functions`, `describe_function`, `explain_term`, `call_function` and an optional `run_script`) instead of one tool per endpoint, so the tool list stays the same size as the API grows. Calls go to your HTTP handler in-process, so authentication, validation and errors are the REST API's own. `run_script` runs JavaScript in QuickJS compiled to WebAssembly, on [wazero](https://wazero.io). |
| `components/mcp/oauth` | An OAuth 2.1 authorization server (PKCE, client registration, refresh token rotation) for MCP clients that sign in through one, such as claude.ai and ChatGPT. |
| **Authentication** | |
| `components/auth/token` | Opaque bearer tokens with a prefix, compared and stored by hash. |
| `components/auth/session` | Rotating session tokens for first-party apps, stored in memory or through bun. |
| `components/auth/oidc` | Verifies ID tokens from Google, Apple and Microsoft. |
| **Running a service** | |
| `components/configurator`, `components/ctrl` | Reads environment variables and `<environment>.config.json` into structs, and wires logging, the database, S3 and SES from them. |
| `components/scheduler`, `components/scheduler/claims` | Recurring jobs on minute boundaries and keyed one-off jobs. With the PostgreSQL claims and leases, several instances share one schedule and a keyed job runs on one of them. |
| `components/ratelimit` | A window counter per key, in memory. |
| `components/logger`, `integrations/promtail` | A small logger interface over zerolog or slog, and a batched Loki client that is an `io.Writer`. |
| **Data** | |
| `drivers/databases/relational` | A bun handle with migrations under an advisory lock, a schema baseline that replaces old migration files, and helpers against lost updates: `Patch` writes only the columns a request sent, `Mutate` and `LockRow` lock a row and read it again before the write. PostgreSQL and SQLite (cgo-free). |
| `drivers/databases/relational/postgres/…` | `pagination`: cursor pagination with optional signed cursors. `query`: search and time range filters. `pgtest`: one database per test run. |
| `integrations/aws/s3`, `integrations/aws/ses` | S3 and SES clients, each with a stand-in for tests: an in-memory bucket and a recording outbox. |
| `types`, `primitives/enums` | `Optional[T]`, `Email`, `PhoneNumber`, `Locale`, `UnixSeconds`, and string-backed enums for JSON and SQL. |

## What it is opinionated about

- **The document is the validator.** A registered route has no hand-written validation and no hand-written OpenAPI. Both come from the request and response types, and the server enforces exactly the schema it publishes, so the two cannot drift.
- **Fail at startup.** The registry refuses at registration whatever it could not document truthfully, such as a response struct that writes its own JSON, or a tag the decoder would read differently from the validator. A wrong registration stops the process at boot instead of misleading a client later.
- **One error shape.** Errors are [ez](https://github.com/vanclief/ez) errors with a code. The code decides the HTTP status, and every failure leaves as `{"error": {"code", "message", "request_id", "fields"}}`.
- **Agents get the same API as other clients.** The MCP server has no logic of its own to keep in step with the REST API: it reads the OpenAPI document and calls the HTTP handler.
- **More than one instance is the normal case.** Migrations and boot-time table creation take a PostgreSQL advisory lock, the scheduler can claim jobs in the database, and [docs/concurrent-writes.md](docs/concurrent-writes.md) says which parts still count per process.
- **Tests need no cloud account.** S3 and SES have stand-ins chosen by the test's own setup code, never by a configuration flag, and `pgtest` gives each test run its own database.

## Why Go, and why it suits AI agents

Much of compose is written with AI coding agents, under the conventions in [AGENTS.md](AGENTS.md). Go is the best fit we know for code that agents write and people have to trust:

- **Typed, and checked in seconds.** The compiler catches a wrong field, a missing return or an unused variable right away, and builds are fast enough to run after every edit. An agent finds out it was wrong before a person has to tell it.
- **A small language.** There are 25 keywords, one formatter and usually one obvious way to write something. Code looks alike whoever wrote it, and there are fewer clever constructs to get subtly wrong.
- **Readable by whoever approves it.** Control flow and error handling are explicit. That costs lines, but nothing happens out of sight, so reviewing a change is a matter of reading it.
- **Fast without the cost.** Compiled code, cheap concurrency and a garbage collector give a service the performance it needs without anyone managing lifetimes.
- **Networking in the standard library.** HTTP, TLS, JSON, cryptography and cancellation come with the language, so there are fewer dependencies to choose and to audit.
- **Runs almost anywhere.** One static binary, cross-compiled by setting two environment variables.
- **Mature libraries** for the rest: databases, cloud services, observability.

It is not free. Error handling is repetitive, and the type system has no sum types, so some of what compose checks at startup a richer type system would check at compile time. For this kind of service we find that a fair price.

## Status

- **Production use.** Most of compose runs in production services. The route registry (`components/rest/api`) is the newest part and has not carried a production service yet.
- **Breaking changes ship in minor releases.** The module stays on `v1`, and a minor release can change behaviour or an exported name. Each such change is listed under `### Breaking` in the [changelog](CHANGELOG.md) with what to do about it. Pin a version and read that section before upgrading.
- **Requirements.** Go 1.26.11 or later; CI also runs the tests on Go 1.27. Echo v5. PostgreSQL is the main target: the advisory locks, the schema baseline and the scheduler's claims need it. SQLite covers the rest.
- **Scope.** The library is maintained alongside the services that use it. There is nothing to sell and no roadmap to promise: it changes when those services need it to, or when someone shows a better design.

## Known limits

- The rate limiter counts in memory, so with N instances a client gets up to N times the limit. There is no shared store.
- Without a claimer, every instance runs every scheduled job. Claims and leases exist for PostgreSQL only.
- In the route registry every success is a 200, bodies are JSON or multipart, and wildcard paths are refused. Routes that need a status code, response headers or the raw body, such as a signed webhook, stay plain Echo routes.
- The OpenAPI generator reads source without type checking, writes OpenAPI 3.0, and does not read a `go.work` file or a `vendor` directory.
- The reference page and the diff read OpenAPI 3.x only.
- `rest.Serve` listens on every interface; it takes a port, not an address.

## Where feedback helps most

These are open design questions, and a well-argued issue on any of them is worth more than a pull request:

- **The registry's gaps.** How status codes, response headers and raw request bodies should look without losing "the document is the validator". The reasoning so far is in [docs/rest-openapi-design.md](docs/rest-openapi-design.md).
- **Two ways to describe an API.** The registry and the source-reading generator both exist. Whether the generator should stay, and how an app moves from one to the other route by route.
- **The MCP tool set.** A few fixed tools with search work well for a large API; whether that holds for small ones, and what a scripting tool should be allowed to do.
- **What counts as breaking.** The API diff decides which OpenAPI changes break a client. Cases it gets wrong are very welcome, with the two documents.
- **State across instances.** Whether a shared rate-limit store belongs here, and what it should be built on.

Reports of something that is simply wrong are just as welcome. A failing test or the smallest program that shows it is the fastest way to a fix.

## Documentation

- [docs/rest-openapi-design.md](docs/rest-openapi-design.md): the route registry, how a request is validated, and what was left out and why
- [docs/auth.md](docs/auth.md): tokens, sessions, ID token verification, CORS and cookies, MCP and its OAuth server
- [docs/concurrent-writes.md](docs/concurrent-writes.md): why two requests can undo each other's changes to one row, the helpers that prevent it, and what is per process when an app runs several instances
- [docs/migrations.md](docs/migrations.md): the migration runner, its connection pool and lock, and the schema baseline
- [docs/scheduler.md](docs/scheduler.md): recurring and keyed jobs, on one instance or several
- [docs/testing.md](docs/testing.md): running an app's tests without AWS credentials, and the test database helpers
- [components/configurator/README.md](components/configurator/README.md): environment variables and config files
- [CHANGELOG.md](CHANGELOG.md): every release from v1.18.0, with its breaking changes

## CLI

The `compose` binary previews API reference pages and writes the comments of Go types for the route registry:

```
go install github.com/vanclief/compose/cmd/compose@latest
compose reference -head openapi.yaml -base previous.yaml -serve 127.0.0.1:8090
compose descriptions -out descriptions.json .
```

The first serves the reference page of `openapi.yaml` with what changed since `previous.yaml`, rendered again on every reload. Use `-out page.html` instead of `-serve` to write a standalone file.

The second writes the doc comments of the types under the current directory, and of their fields, to `descriptions.json`. The registry loads that file with `api.Descriptions`, so its documents describe what the comments describe. A type declared more than once in a package, such as in a `cgo` and a `!cgo` file, is left out with its fields and named in a warning, so the file is the same on every platform and never describes the wrong declaration. The types of a `package main` are also written under `main` only when it is the only main package read that declares types. With two or more, a warning names them, so run the command on one command's directories at a time.

## Contributing

Open an issue before a large change, so the design can be agreed first. The Go conventions for this repository are in [AGENTS.md](AGENTS.md).

```
go test ./...
```

The PostgreSQL tests skip when no server is reachable on `localhost:5432`. CI runs them against PostgreSQL 16 with the race detector; set `COMPOSE_TEST_POSTGRES=1` to make an unreachable server a failure instead of a skip.

## License

[MIT](LICENSE)
