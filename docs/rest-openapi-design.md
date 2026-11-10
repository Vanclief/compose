# compose REST: OpenAPI, schema-as-validator and SDK generation

Status: implemented on branch `Vanclief/sdk-generation` (packages `components/rest/api`,
`components/rest/api/openapi`, `components/rest/api/validate`, `components/rest/apitest`);
first ships in compose v1.30.0, on Echo v5. Last updated 2026-11-09.

## 1. Decision

Keep compose's REST layer as it is in spirit: Echo v5, the
`{"error":{"code","message","request_id"}}` envelope (written by `rest.ErrorHandler` for
registered routes, `ManageError` for plain ones), Sentry, `requests.Request`. Add a
declarative route registry that produces an OpenAPI document and validates every request
against that document before the app sees it. Registered routes call one typed handler per
operation instead of `App.HandleRequest`'s type switch, which stays for plain Echo routes
(section 4). Ozzo leaves the request path.

Do **not** adopt huma (or any other framework). Do **not** write our own reflector.
Glue two libraries (huma's code is copied into compose, see section 5):

| Role | Library | Why |
|---|---|---|
| Go types to OpenAPI schemas | `github.com/danielgtaylor/huma/v2` v2.39.1 schema, registry and OpenAPI code, copied into `components/rest/api/internal/huma` (MIT, trimmed; no `huma.API`, no router, no `huma.Validate`, no error model) | richest tag set, the vocabulary we already chose, panics on name collisions, native 3.1 plus a 3.0.3 `Downgrade()`, root package compiles with stdlib only |
| Request validation | `github.com/santhosh-tekuri/jsonschema/v6` | draft 2020-12 validates the 3.1 form huma emits, typed error kinds, uuid v7 passes, compiled once at startup, already a transitive dependency of kin-openapi |

Runner-up reflector: `swaggest/openapi-go` (designed for standalone use, native 3.0.3). It
loses because field-level constraint and enum tags are dropped when the field's named type
becomes a component, fields without a `json` tag vanish, its 3.0 output is lossy
(`exclusiveMinimum` becomes a boolean with no bound), and 3.0-native output would force
kin's lossy 3.0 validator. Astra confirmed in source that huma honours field-level `enum`
tags on aliased named types; the same tag handling narrows the enum components compose
registers (section 5), so there is no remaining condition under which swaggest wins.

## 2. How we got here

1. emma-backend has a 4.8k-line go/ast + regex OpenAPI generator (30% emma-specific, fragile).
   auditor3-api has a 1.4k-line hand-written `map[string]any` spec (accurate, 0 error
   responses; 0 operationIds until 2026-10-11, 50 since). Neither is worth moving into
   compose.
2. Models need real types and constraints, not only routes. compose relies on ozzo `Validate()`,
   whose `Required` means non-zero while OpenAPI `required` means key presence. Every
   code-first Go framework that works validates against the schema it emits. We adopt that.
3. Full huma was evaluated and rejected: it would force its problem+json error body, 422 for
   validation, required-by-presence in both directions, `huma.Context` in handlers, and a
   single-maintainer framework on the request path. The reflector is the only part worth
   taking, and it is importable on its own.
4. Survey of alternatives (2026-10-11, source read, probes run):

| Project | Verdict | Notes |
|---|---|---|
| huma (framework) | reject, import registry only | see above |
| fuego | port one idea | go-playground tags, silent component collisions across packages, mixed 3.0/3.1 nullable in one doc |
| fizz + tonic | port one idea | gin only, dormant since 2022, separates params from body in the input schema |
| goa, ogen, oapi-codegen | nothing to import | DSL / spec-first |
| go-restful, goyave | nothing | no validation / no OpenAPI |
| oaswrap/spec, gleece, okapi, apispec | reference only | < 200 stars, docs-only or own framework |
| kin-openapi `openapi3gen` | weakest reflector | no `required`, `uuid.UUID` becomes `{}`, invalid generic names, silent collisions |
| invopop/jsonschema | wrong output | JSON Schema `$defs`, not OpenAPI |
| swaggest openapi-go | runner-up | see above |
| kin `Schema.VisitJSON` | usable validator, not chosen | 3.1 mode marshals and recompiles with santhosh on every call and silently falls back to the 3.0 validator on compile failure (`openapi3/schema_jsonschema_validator.go:20-49, 290-297`) |
| `huma.Validate` standalone | avoid | `null` passes on optional fields, case-insensitive keys, echoes whole parent object |
| swaggest validator | avoid | pinned to santhosh v3 (2021, draft-7), stops at first failing location in map order |
| kaptinlin/jsonschema | avoid | needs Go 1.27, instance path resets inside `$ref` |

## 3. Target shape (auditor3 `runs`, the real ten routes)

```go
// application/resources/runs/update.go
type UpdateRequest struct {
    RunID          uuid.UUID          `json:"-" path:"id"`
    MaxTotalRuns   *int               `json:"max_total_runs,omitempty" minimum:"0"`
    CodeSnapshotID *uuid.UUID         `json:"code_snapshot_id,omitempty"`
    Status         *project.RunStatus `json:"status,omitempty" enum:"failed,cancelled"`
    FailureReason  *string            `json:"failure_reason,omitempty"`
}

// Validate keeps only cross-field rules. Shape rules live in tags.
func (r *UpdateRequest) Validate() error {
    if r.Status != nil && *r.Status == project.FailedStatus && r.FailureReason == nil {
        return ez.New(ez.EINVALID, "failure_reason is required when status is failed", nil)
    }
    return nil
}

// Requests with no cross-field rules embed the no-op.
type GetRequest struct {
    requests.NoRules
    RunID uuid.UUID `json:"-" path:"id"`
}

// One typed handler per operation. The requester comes from ctx (user.FromContext).
func (m *Module) Get(ctx context.Context, request *GetRequest) (project.Run, error)

// api/rest/server.go
e.HTTPErrorHandler = rest.ErrorHandler() // answers the errors operations return to Echo
reg := api.New(e,
    api.Info("auditor3", version),
    api.APIKeyHeader("API-KEY"),        // security scheme applied to every op unless api.Public()
    api.Authenticate(app.Authenticate), // every op is protected unless api.Public()
)
rg := reg.Group("/runs", api.Timeout(60*time.Second)) // tag "runs" derived from the prefix
api.GET(rg, "", "list", app.Runs.List) // Req and Res inferred from the handler
api.GET(rg, "/stats", "stats", app.Runs.Stats)
api.GET(rg, "/:id", "get", app.Runs.Get)
api.POST(rg, "", "create", app.Runs.Create)
api.POST(rg, "/:id/cancel", "cancel", app.Runs.Cancel)
api.POST(rg, "/:id/claim", "claim", app.Runs.Claim)
api.POST(rg, "/:id/complete", "complete", app.Runs.Complete)
api.POST(rg, "/:id/heartbeat", "heartbeat", app.Runs.Heartbeat)
api.PATCH(rg, "/:id", "update", app.Runs.Update)
api.PUT(rg, "/:id", "replace", app.Runs.Update, api.Hidden())
reg.Ready() // panics on a protected op without an authenticator, a replaced route, or Echo's default error handler

// Spec is a committed golden file, regenerated by a test with -update. Serving it at
// runtime is optional.
doc31, err := reg.Spec(api.V31)
doc30, err := reg.Spec(api.V30)
```

`requests.NoRules` cannot be embedded next to `pagination.CursorRequest`: the two promoted
`Validate` methods are ambiguous and the compiler rejects the registration. A type that
embeds `CursorRequest` already satisfies `requests.Body`.

Blobs and alternate representations (emma's shape; auditor3 has none; section 4):

```go
// Report returns api.Blob, so its media type is declared.
api.GET(rg, "/:id/report", "report", app.Runs.Report, api.ContentType("application/pdf"))
// Export returns a JSON Res; ExportCSV, func(ctx, *ExportRequest) (api.Blob, error),
// answers Accept: text/csv. Exactly one of them runs per request.
api.GET(rg, "/export", "export", app.Runs.Export, api.Produces("text/csv", app.Runs.ExportCSV))
```

Generic signature (pointer-receiver constraint keeps `requests.Body` satisfied by `*Req`):

```go
func Handle[Req, Res any, P interface {
    *Req
    requests.Body
}](g *Group, method, path, name string, fn func(context.Context, P) (Res, error), opts ...Option)
```

Go infers `P` and `Res` from `fn` and `Req` from `P`'s constraint, so registrations name no
types (explicit arguments still compile). `GET`, `POST`, `PUT`, `PATCH` and `DELETE` fix the
method.

Rules enforced at registration (fail boot):

- every `path:`/`query:`/`header:` field carries `json:"-"`, so a body key can never fill a
  param field and params never leak into the body component (huma skips `-`, `schema.go:915`)
- every path placeholder has a `path:` field and vice versa, and a placeholder name appears
  once in a path; a wildcard path (`/files/*`) is refused, because the document cannot
  describe it, and so is a full path that does not start with `/` (`Group("runs")`), which
  Echo would serve under another key than the document's, and a `:` that does not start
  its segment (`/v:version`), which Echo reads as a parameter and the document cannot
  describe
- operationId `<tag>.<name>` is unique, and so is method plus path (placeholder names do not
  count: `/runs/:id` and `/runs/:run_id` are one route, and every method of one path must
  spell its placeholders alike, since OpenAPI forbids two templated paths that differ only by
  those names); names and tags match
  `[A-Za-z0-9_-]+`. The tag is the last non-parameter segment of the group's full prefix
  (`/runs/:id/issues` gives `issues`); with none, the operationId is `<name>`
- tag names come from an allowlist (`json`, `path`, `query`, `header`, `form`, `required`,
  `default`, `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum`, `multipleOf`,
  `minLength`, `maxLength`, `pattern`, `minItems`, `maxItems`, `uniqueItems`, `enum`,
  `format`, `doc`, `example`, `deprecated`, `hidden`, `nullable`, plus `bun`, `db`, `yaml`,
  `xml`, `mapstructure` and `validate`, which the same structs carry for other libraries);
  Echo's `param` is rejected with a pointer to `path`, and `default` on a request type,
  because the registry applies no default: the handler sets it and `doc` describes it. The
  lint also rejects the `json` `string` option (the schema and `encoding/json` would disagree
  on the value's type), property names
  `contentEncoding` and `contentMediaType` (section 8), a `nullable` tag on an object
  reference, `minimum`/`maximum` outside a sized integer's range, a pointer to
  `types.Optional`, and on a request type a `path`, `query`, `header` or `form` tag in a
  struct the request type does not embed (a named field, a slice or map element), where the
  registry never reads it
- the keys `encoding/json` binds are exactly the schema's properties, at every depth of the
  request type: registration fails for unexported embedded structs, `hidden` body fields,
  embedded non-struct types and shadowed promoted fields, any of which would let a value the
  validator never saw reach the handler (`openapi/jsonkeys.go`); a `hidden` parameter fails
  too, since the document would hide a parameter the server still reads
- GET, HEAD and DELETE request types have no body properties (and no files); registration
  fails and points at the `query:` tag
- param fields are string, bool or number kinds, `encoding.TextUnmarshaler`s or slices of
  those: no structs, maps, `[]byte` or `types.Optional`, and none reached through an
  unexported embedded pointer, which reflection cannot allocate
- `Res` is the handler's return type, so the documented response cannot drift from what is
  written; it must not be an interface type, and `*api.Blob` is rejected (a nil pointer would
  need a runtime check; handlers return `api.Blob` by value). Any other pointer `Res` is
  allowed, and a handler that returns it nil without an error is answered as a 500
  `EINTERNAL`, not as the `200 null` the document does not describe
- a `Produces` handler takes the operation's `Req`: a built `Option` has lost its type
  parameters, so the compiler cannot check it and registration does
- every option is used at a scope it supports (`Info`, `Description`, `APIKeyHeader`,
  `Server`, `Namer` on `api.New` only; `ContentType`, `Produces` on an operation only), and
  one options list cannot hold both `Public()` and `Authenticate()`

Dropped from the first sketch after review: `api.DefaultError` (`rest.ErrorHandler`, like
`ManageError`, always writes `handler.ErrorResponse`, making it configurable invites drift),
`api.Empty` (add it only when an endpoint needs it, and then it means 204 via
`c.NoContent`, never `200 null`), `api.Tag` on groups (derived from the prefix, above),
`SpecHandler`.

## 4. Request pipeline per registered operation

Echo's binder is **not** used on registered routes. It reads the `param` tag, not `path`,
matches query keys with `EqualFold` (`bind.go:314`), binds query only on GET/DELETE/HEAD/QUERY
(`bind.go:142`), and `encoding/json` matches fields case-insensitively. Any of those lets a
value that validation never saw reach the app (`{"Status":"pending"}` or `?LIMIT=5000` pass
as unknown keys, then bind). Decode once instead.

1. Echo middleware runs first (`e.Use`, `Group.Use`, `api.Middleware`). It sees the raw
   request only: anything it rejects never reaches schema validation.
2. `requests.NewWithContext(c.Request().Context(), ...)`: the request context derives from
   the inbound one, so it keeps the values Echo middleware set and is cancelled when the
   client goes away. Its deadline is the earlier of the inbound deadline and the route or
   group timeout (`api.Timeout`, passed as `requests.WithTimeout`, which replaces the 15s
   default and panics on a duration that is not positive; auditor3 uses 60s). ID, `c.RealIP()`, `Client` header, locale and method as
   before. `API-KEY` is a security scheme the authenticator reads, not a header param. The
   request is stored with `handler.SetRequest` before anything can fail, so
   `handler.Request(c)` returns it to middleware and to the error handler's reporter and
   translator. `requests.New` roots the context in `context.Background()`, so a client
   disconnect never cancels the app's work and values set by Echo middleware are lost; that
   bug is latent in every `HandleRequest` route. `requests.New` is unchanged for plain Echo
   routes, which can switch on their own. `requests.WithTimeout` derives from the request's
   current context: as an option of `New` the deadline is exactly its duration, of
   `NewWithContext` the earlier of the parent's and its own, and applied later it can only
   shorten the deadline. Both add `DEFAULT_TIMEOUT` only when no `WithTimeout` option ran.
3. Negotiate the response media type from `Accept` ("Content negotiation", below) and put it
   on `request.GetAccept()` and `api.Accept(ctx)`. Then requests that arrive before `Ready`
   fail with `EINTERNAL`.
4. The operation's `Authenticate` function, unless it is `Public()`. No parameter has been
   coerced and no body byte read yet: `request.GetBody()` is nil inside it.
5. Coerce declared path, query and header params to the schema's type (integer, number,
   boolean, array via comma or repeated keys), dropping empty values (`?cursor=` is
   absent). An array header is read as an RFC 9110 list: each line is split on commas,
   spaces around the items are trimmed and empty items dropped. An integer is read as an
   int64, else as a uint64, and a number as the shortest text of the nearest float64. If
   parsing fails keep the raw string so santhosh reports `kind.Type` like any other type
   error. Header keys lowercased, only declared `header:` fields included.
   Missing stays missing (presence semantics).
6. Read the body through `http.MaxBytesReader` under the operation's limit
   (`api.MaxBodyBytes`, the nearest of `api.New`, the groups and the operation, else
   `api.DefaultMaxBody`, 1 MiB); a larger body is a 400 `body exceeds N bytes`, so registered
   routes do not need `rest.WithBodyLimit`. Decode with `jsonschema.UnmarshalJSON`
   (`loader.go:255-265`: `UseNumber`, rejects trailing data). One walk of the decoded
   body then bounds what validation costs. A body whose arrays and objects nest deeper than
   64 levels is a 400: every error carries its full path, so on a recursive type a small,
   deep body would cost megabytes. And every number is handed to the validator in canonical
   text (an integer's digits, else the shortest text of the nearest float64, else `1e309`
   for a number no float64 holds): santhosh parses numbers with `big.Rat`, where one
   `1e999999` costs 20 ms. The handler still decodes the client's own bytes. A non-JSON
   content type on a non-empty body returns `EINVALID`, since Echo's binder no longer produces the 415.
   Multipart operations parse `multipart/form-data` instead ("Multipart uploads", below).
7. Validate the synthetic `{path, query, header, body}` schema compiled at `Ready`.
   `body` is required only when the body variant has required properties. Add the
   case-collision check: a body key that matches a declared property ignoring case but not
   exactly is an error at every depth, because `encoding/json` matches case-insensitively at
   every depth (`config.Retries is not a known field, did you mean "retries"`,
   `api/collisions.go`). Collect every error. Map `kind.*` to one ez `EINVALID` whose message
   joins `<field> <reason>` with `; `, parameters prefixed with their location
   (`query limit must be <= 1000`), never echoing submitted values (400 under
   `rest.ErrorHandler`, same envelope).
8. Decode once: `json.Unmarshal(bodyBytes, req)` for the body, then set param fields by
   field index from the raw values, using `TextUnmarshaler` for the types that implement it
   (`uuid.UUID`), the string kind for compose enums, `types.NewLocaleString` for a
   `types.Locale` (as its `UnmarshalJSON` does in a body) and HTTP-date for a `time.Time`
   header. Embedded pointer structs on a parameter's path are allocated whether or not the
   parameter was sent, so a `Validate` promoted from one never meets nil.
   A conversion failure is a 400 (`query at is invalid`), not a 500: santhosh's
   `date-time` accepts inputs that `time.Time` rejects. Only an ez `EINTERNAL` from a
   `TextUnmarshaler`, its own failure rather than the client's, is a 500. `request.SetBody(req)`.
9. `Body.Validate()`, once, for cross-field rules.
10. The typed handler of the negotiated media type, exactly one: the `Produces` handler
    declared for it, else the primary handler. It receives `request.GetContext()` as it is
    after `Authenticate`. A result is written with `c.JSON(200, res)`, or `c.Blob` for an
    `api.Blob` (and `Content-Disposition` when it has a filename).
11. An error from any step is returned to Echo, and the operation writes nothing for it:
    the pipeline's own errors are ez errors, the handler's go back as it returned them. The
    app's `HTTPErrorHandler` answers it, `rest.ErrorHandler` in the envelope with the app's
    reporter and translator (below); Echo's default handler would answer every ez error as a
    500 `{"message":"Internal Server Error"}`, so `Ready()` panics while it is installed
    (see "Typed handlers and the authentication slot"). On every exit the registry cancels
    its request context. Nothing is copied back onto the Echo request: middleware after
    `next(c)` reads what the authenticator added from `handler.Request(c).GetContext()`,
    which is cancelled by then, and does I/O with `c.Request().Context()`.

**`Authenticate` runs before anything is parsed.** Until 2026-11-09 it ran after
`Validate()`; the order is reversed. An unauthenticated caller gets 401 and learns nothing
about the request's shape, where a 400 would tell it which fields exist, which are required
and which cross-field rules apply, and the registry parses nothing for it: no parameter
coercion, no body read, no multipart form spilling to temporary files. Echo middleware in
front of the registry still runs first: `rest.RejectNUL`, which `rest.Configure` installs,
reads a JSON, form or multipart body before the authenticator. The cost is one
credential lookup per malformed request that carries a credential, plus whatever else the
authenticator does (emma's updates `last_active_at`), and the authenticator never sees the
body; neither app's needs it. Malformed and cross-field-invalid requests still never reach a
typed wrapper or the handler, so nothing is charged there for them and the handler never sees
an invalid body (Echo middleware still sees them, step 1). `Validate()` cannot use the
requester: it receives neither a context nor the request. Identity-dependent checks stay in
the handler or module.

**Errors go back to Echo.** `api.New(e, opts...)` takes no `BaseHandler` and the registry
builds no handler. `rest.ErrorHandler(opts...)` answers an ez error, which is an `*ez.Error`
itself or an error with one in its chain and no Echo status, with `ez.ErrorToHTTPStatus`,
`ez.ErrorCode` and `ez.ErrorMessage` of the error, like `ManageError`; ez reads only a direct
ez chain, so an ez error wrapped in another kind (a recovered panic of one,
`fmt.Errorf("...: %w", err)`) is a 500 `EINTERNAL` with ez's generic message. An Echo error
that wraps an ez error, as `c.Bind` returns when a field's `UnmarshalJSON` failed with one,
keeps Echo's status. Any other error gets Echo's status and the status text or an `*echo.HTTPError`'s
message; one without an Echo status is classified with `ez.ErrorCode` like `ManageError`
(`context.Canceled` a 499 `ECANCELED`, `context.DeadlineExceeded` a 504 `ETIMEOUT`, with ez's
fixed text, anything else a 500 with the status text), never the error's own text. It takes
two options.
`rest.WithErrorReporter(func(c *echo.Context, err error))` is called once per answered error
with the original error, before any translation, and the app decides what to report;
`handler.ReportToSentry` is one (classified with `ez.ErrorCode`: `EINTERNAL`, which includes
non-ez errors without an Echo status such as recovered panics, at error level, `ETIMEOUT` and
`EUNAVAILABLE` at warning; client errors, `ECANCELED` and Echo's sentinels skipped).
`rest.WithErrorTranslator(func(c *echo.Context,
err error) error)` changes the message of ez errors only; code and status stay the
original's. A translator reads the locale through `handler.Request(c)`, which is nil for an
unknown route, a 405 or a panic outside an operation. `handler.ManageError` stays for plain routes;
it sets `handler.Request(c)` too and still reports every `EINTERNAL`, `ETIMEOUT` and
`EUNAVAILABLE` it answers to Sentry, an Echo error it answers as a 500 included. The request log
of `rest.Configure` answers the error before logging it, so its `status` is the one sent and
its level follows it (error for 5xx, warn for 4xx, info otherwise).

### Typed handlers and the authentication slot

A handler is `func(ctx context.Context, request *Req) (Res, error)`. `ctx` carries the
request ID, the negotiated media type, the inbound values and whatever the authenticator
added; the app reads its requester from it (`user.FromContext(ctx)`).

```go
type AuthenticateFunc func(request requests.Request) error
```

The authenticator changes the request in place (`request.SetContext`, `request.SetLocale`)
instead of returning a context, so an app helper that caches into the request context on its
failure paths (emma's bearer resolution) cannot lose those additions. An error rejects the
request: it is returned to Echo like every other (step 11).

- **Inheritance**: options of `api.New`, then of each group from the outside in, then of the
  operation; the nearest wins. `Authenticate(fn)` sets the function and marks the operations
  protected, so a route under a `Public()` group can restore protection. `Public()` skips
  authentication and drops the security requirement from the spec. Both in one options list
  panic at boot.
- **`Ready()`** panics listing every protected operation, hidden ones included, that has no
  authenticator. An operation is never unauthenticated by omission. Before that it calls the
  Echo instance's `HTTPErrorHandler` once with an ez `EINVALID` error on a made-up request
  and panics unless the handler answers it with a 4xx, so Echo's default handler (a 500), a
  nil one or one that writes nothing is reported at boot. That call reaches the handler's
  reporter and translator like any error, with `handler.Request(c)` nil;
  `handler.ReportToSentry` skips it as a client error. It then asks Echo's router which route
  a request to each operation's path reaches, and panics naming the operation when it is no
  longer the registry's: Echo's default router lets a route registered later on the same
  method and path (`e.GET("/runs/:id", ...)` left over from a migration) replace it, and the
  operation's authentication and validation would silently stop running. Registry routes are
  named `api:<operationId>` in Echo for that check; a replacement made after `Ready()` goes
  unnoticed. A `Ready()` that panicked leaves the registry not ready, and the next call
  checks again.
- **App-local typed wrappers** are the supported way to run app code after validation and
  before the handler. They are plain function composition; compose adds no hook. emma's
  public-route quota must not charge invalid bodies, so it cannot be Echo middleware:

```go
// publicHandler charges the client IP's public budget, then calls fn.
func publicHandler[Req, Res any](app *App, fn func(context.Context, *Req) (Res, error)) func(context.Context, *Req) (Res, error) {
    return func(ctx context.Context, request *Req) (Res, error) {
        var zero Res
        ip, _ := ctx.Value(clientIPKey{}).(string) // set by app middleware from c.RealIP()
        if !app.RateLimiter.Allow(rateLimitIPKey(ip)) {
            return zero, rateLimitExceeded()
        }
        return fn(ctx, request)
    }
}

api.POST(auth, "/login", "login", publicHandler(app, app.Auth.Login), api.Public())
```

- **Echo middleware** does everything transport-level: CORS, recovery, trusted-caller
  detection, the IP peek, logging. A logger installed outside recovery reads the outcome
  after `next(c)`:

```go
func requestLog(next echo.HandlerFunc) echo.HandlerFunc {
    return func(c *echo.Context) error {
        started := time.Now()
        err := next(c) // a registry error, unanswered unless an inner middleware answered it
        if err != nil {
            c.Echo().HTTPErrorHandler(c, err) // answer it now, so status is the one sent
        }
        _, status := echo.ResolveResponseStatus(c.Response(), err) // the committed status
        event := log.Info().
            Str("id", c.Response().Header().Get(echo.HeaderXRequestID)).
            Str("route", c.Path()).
            Int("status", status).
            Dur("latency", time.Since(started)).
            Err(err) // as returned, before any translation
        request := handler.Request(c) // nil when no registered operation or ManageError ran
        if request != nil {
            event = event.Type("body_type", request.GetBody())
            // Cancelled by now: read its values, do I/O with c.Request().Context().
            requester := user.FromContext(request.GetContext())
            if requester != nil {
                event = event.Interface("user_id", requester.ID)
            }
        }
        event.Msg("Request Handled")
        return err // Echo's second call to the error handler skips the committed response
    }
}
```

### Blob responses

`api.GET(g, "/report", "report", app.Report, api.ContentType("application/pdf"))` for a
handler whose `Res` is `api.Blob`. It returns `api.Blob{ContentType, Body, Filename}` by
value (section 3). An empty `ContentType` means the negotiated type, `Filename` adds
`Content-Disposition: attachment`; errors keep the JSON envelope. Registration fails for a
Blob `Res` without `ContentType`, `ContentType` on any other `Res`, and `ContentType` or
`Produces` declaring `application/json` (a JSON `Res` produces it). Declared types are
lowercase and carry no parameters or wildcards (`text/plain`, while the Blob may send
`text/plain; charset=utf-8`); the Blob's own `ContentType` is written as given.

### Content negotiation

`api.Produces("text/csv", app.ExportCSV)` adds a representation with its own handler,
`func(ctx, *Req) (api.Blob, error)`, whose `Req` must be the operation's (checked at boot).
The declared types are the primary one (`application/json` for a JSON `Res`, the
`ContentType` of a Blob) then the `Produces` types in declaration order, none repeated. The
pipeline picks the first `Accept` entry, in header order, that matches a declared type
(`type/*` and `*/*` match the first declared type in the range), else the primary type;
q-values are ignored and there is no 406. Exactly one handler runs: the negotiated type's
`Produces` handler, else the primary one, so a handler never returns the wrong kind of value.
Every registered operation sets the choice on `requests.Request.GetAccept()` and
`api.Accept(ctx)`. The 200 response lists one content entry per declared type: `Res`'s schema
for JSON, `{type: string, format: binary}` for the others (huma adds
`contentMediaType: application/octet-stream`, which the 3.0 downgrade renames
`x-contentMediaType`). `apitest.Contract.Do` checks a non-JSON 200's content type against the
declared types.

### Multipart uploads

Request fields of type `*multipart.FileHeader` or `[]*multipart.FileHeader` tagged
`form:"name"` and `json:"-"` make the operation `multipart/form-data`. The other body fields
are read from the form values named by their JSON keys (empty values are absent, unknown and
case-mismatched keys are ignored), coerced like query params and validated against the body
component; then they decode through `encoding/json`, so named types decode as in JSON bodies.
File fields are checked for presence (`required:"true"`), and a single-file field rejects
repeated parts. The body limit wraps `ParseMultipartForm` (32 MiB in memory, the rest spills
to temporary files). Registration fails for file fields without both tags, `form` tags on
the request type's other fields or in a struct it does not embed, body fields a form value cannot carry (nested objects, maps, `[]byte`) and files
on GET, HEAD or DELETE. The request body is documented as `multipart/form-data` with the body
properties inline, files as binary strings (arrays of them for slices) and `required` by
presence. The `requestBody` itself is always `required: true`, since the server refuses a
request without a multipart body.

### Left out of v1 on purpose

- **`Around`/`Before` hooks.** Additive later as new options. The pipeline keeps the
  boundary invoke → result or error → write (the selected handler returns before anything is
  written), so a hook can wrap it without touching handler signatures. Typed wrappers cover
  the one position the apps need now.
- **`Response[T]`, status codes, response headers.** No registered route needs them: every
  success is a 200. `api.Status(...)` would be the additive first step, and `Response[T]` can
  later be recognised as a return type next to plain `T`. Health checks and other routes that
  need them stay plain Echo routes.
- **A typed requester** (`Registry[U]`, handlers taking `U`). Apps read it from `ctx` with
  their own `user.FromContext(ctx)`; compose does not know the app's user type.
- **A fallback to `App.HandleRequest`.** Each app migrates its registered routes in one
  change; plain Echo routes (`handler.NewHandler`, `JSONResponse`) are the boundary. The typed
  `Legacy[Req, Res]` registration and `x-compose-legacy` were dropped before release: blobs,
  negotiation and multipart (above) cover what emma-backend's handlers do through
  `BaseHandler`.
- **XML request bodies.** No emma-backend or auditor3-api REST handler calls
  `BindedXMLResponse` (emma's only XML is an outgoing client), so there is no consumer.
  `BaseHandler.BindedXMLResponse` is unchanged; registered operations accept JSON and
  multipart bodies only.

## 5. Reflector glue (huma schema code as a library)

huma's schema code only produces component schemas. In library mode it never produces
parameters (param extraction lives in `huma.Register`, `huma.go:202`), its map registry
hardcodes `additionalProperties: false` on every struct (`registry.go:216`, applied at
`schema.go:973, 985`), and it passes itself, not a wrapper, into `SchemaFromType`
(`registry.go:162`), so a wrapper around it would never see nested fields. The reflector
therefore implements `huma.Registry` itself (`openapi/reflector.go`), re-implementing huma's
`mapRegistry`: it matches huma on recursion placeholders, the duplicate-name panic, anonymous
numbering, `time.Time`, `TextUnmarshaler` and `SchemaProvider`, and every nested field passes
through it, so compose's hooks apply at every depth. Schema generation stays huma's
`SchemaFromType` and `SchemaFromField`. The post-pass is a real component: the `openapi`
package is about 1,650 lines (the reflector, the request variants, the `encoding/json` key
check, the lint and the parameters) plus golden fixtures, in separate walks for parameters,
lint, key check and variants.

- Components are named by a package-qualified namer (`openapi.DefaultNamer`):
  `project.Run`, `runs.ListResponse`. Generic type arguments are qualified too
  (`Page[a.User]` is `pkg.Page_a.User`), so `Page[a.User]` and `Page[b.User]` do not
  collide. `api.Namer(openapi.SegmentsNamer(2))` qualifies with two path segments; a
  remaining collision fails registration and names that fix.
- **Parameters**: compose walks `reflect.VisibleFields` of the request type (this is what
  picks up embedded `pagination.CursorRequest`), builds `path`/`query`/`header` parameter
  objects, and uses `huma.SchemaFromField` per field for constraints.
- **Request variants**: for every component reachable from a request type, rewrite
  `Required` to "fields with `required:"true"`" (path params are always required), and make
  pointer fields nullable (`type: [T, "null"]`, a `$ref` as `anyOf [{$ref}, {type: null}]`)
  unless required, so `"failure_reason": null` keeps meaning absent, as `encoding/json`
  treats it today. A component gets a variant when its rewritten schema differs from huma's
  (compared as marshalled JSON) or when it references one (fixpoint, which also resolves
  cycles); parents re-point their refs. The variant is `<pkg>.<Type>Input` when the component
  is also reachable from a response, else it takes the component's own name. Response
  components keep huma's default: required unless `omitempty`/`omitzero`, a pointer to a
  struct non-null, and a pointer to a scalar nullable unless it carries `omitempty`.
- **Clear `additionalProperties: false`** on every published component. Converted endpoints
  must keep ignoring unknown request keys as today, and progenitor's typify turns `false`
  into `deny_unknown_fields`, which would make adding a response field break deployed Rust
  clients. Contract tests compile a strict copy with `false` re-injected on response
  components so undocumented response fields are still caught in CI.
- **Type table** (`builtinSchemas`): `uuid.UUID` is `{type: string, format: uuid}`
  (`TextUnmarshaler` types otherwise become plain `string`), `types.Email` `format: email`,
  `types.PhoneNumber` `format: phone`, `types.Locale` and `types.Language` plain strings,
  `types.UnixSeconds` `{type: integer, format: int64}`. The registry dereferences pointers
  before the lookup and marks a pointer's schema nullable itself. compose `types/` do not
  implement `huma.SchemaProvider`: the copy is internal, so `types/` cannot import it, and
  huma stays confined to `components/rest/api/...`. The copy still honours `SchemaProvider`,
  but nothing implements it.
- **Enums as named components**: `enums.Set[T]` records the values by `reflect.Type`
  (`enums.Values`); repeated `Set` calls for one type record the union in first-seen order,
  and a field narrows with the `enum:"a,b"` tag. For a recorded type the registry registers a
  `<pkg>.<T>` component with the values and returns a fresh `&Schema{Ref: ...}` on every call.
  Two conditions verified in source: the registry must return a fresh pointer because
  `SchemaFromField` (`schema.go:572`) applies the field-level `enum` tag to it
  (`schema.go:608-614`), and `$ref` with a sibling `enum` is emitted side by side
  (`schema.go:208, 217`), valid in 2020-12 where santhosh applies both. Field-level
  `enum:"failed,cancelled"` therefore narrows a named enum; the 3.0.3 file moves the `$ref`
  under `allOf`, since 3.0 ignores `$ref` siblings. Named components mean
  openapi-python-client and progenitor emit one enum type per Go type, not one per field.
- Build the document on `huma.OpenAPI` types so `Downgrade()` is free for 3.0.3.
  `OpenAPI.MarshalJSON` prunes components no path references. Both files are re-encoded with
  sorted keys, and the 3.0.3 one gets what `Downgrade` leaves invalid rewritten:
  `anyOf [X, {type: null}]` becomes `nullable: true` (beside `allOf [{$ref}]` for a ref)
  (`api/spec.go`, `canonical`).
- operationId `<tag>.<name>` (`runs.update`) so hey-api nests clients. Every operation gets a
  `default` response pointing at the `handler.ErrorResponse` component, under the name the
  namer gives it. The `api.APIKeyHeader` security
  scheme applies to every operation unless `api.Public()`.
- huma is not a module dependency. Its `schema.go`, `registry.go` and `openapi.go` from
  v2.39.1 are copied into `components/rest/api/internal/huma` (MIT, `LICENSE` alongside),
  trimmed to what compose uses: no `huma.Validate` message precomputation (only its
  `pattern` compile check stays), no YAML output, no framework-only `Operation` fields, no
  registry config, no `SchemaTransformer`. `file:line` references to huma in this document
  point at upstream v2.39.1. Golden tests guard the copy like they guarded a version pin.

### Optional body fields (`types.Optional[T]`)

A pointer field cannot tell `"failure_reason": null` from an absent key, and a PATCH
handler that checks which fields changed needs both. `types.Optional[T]` records it:
absent leaves `Set` false, `null` sets `Set` and `Null`, a value sets `Set` and `Value`
(`Get()` returns the value and whether one was sent). It marshals to `null` when unset or
null and its `IsZero` is `!Set`, so `omitzero` drops an unset field from a response.

- **Schema**: the schema of `T` made nullable, exactly like a pointer field in a request:
  a scalar or array is `type: [T, "null"]`, an enum or struct is
  `anyOf [{$ref}, {type: null}]` (3.0: `nullable: true` beside `allOf [{$ref}]`). Field
  tags apply to `T`'s schema, so `minLength` lands on the string and `enum:"a"` narrows the
  enum inside the `$ref` branch. `Optional` itself never becomes a component.
- **Request variants**: an Optional field is required only with `required:"true"`; then
  the key must be present and `null` is still valid. **Responses**: a nullable leaf,
  required unless `omitempty`/`omitzero`.
- **Body fields only**: registration fails for an Optional path, query or header parameter
  and for an Optional multipart form field (use a pointer there). The lint, the key check
  and the case-collision walk look through it into `T`.
- **Hook**: the reflector finds it by method, `OptionalType() reflect.Type` returning `T`,
  through an unexported interface in `openapi`, so `types/` depends on neither the api
  packages nor huma.

## 6. Validator glue (santhosh v6)

- `compiler.AddResource("mem:///spec.json", ...)` once with a validation document holding every
  component, including those only hidden operations use (the public document prunes them),
  then per operation compile a synthetic schema `{properties:{path,query,header,body}}`
  whose parts `$ref` into it. Compiled schemas are immutable and goroutine-safe; the
  `Compiler` is not, so compile in `Ready` in one goroutine.
- `AssertFormat()` on. `uuid` (structural, v7 passes by default) and `date-time` are built
  in; register `email` and `phone` (compose's `types` checks). Unknown formats (`int64`,
  `double`) are ignored, which is fine.
- Error mapping: `kind.Required{Missing}` becomes `field is required`, `kind.Type` becomes
  `field must be <want>`, `kind.Enum` becomes `field must be one of ...`, `kind.Format`
  becomes `field must be a valid <name>`, `kind.Minimum`/`Maximum`/`MinLength`/... become
  `field must be ...`. Instance location from `InstanceLocation` joined with `.`. Build a
  `[]FieldError{In, Name, Message}` internally (`validate.Fields`) even though the envelope
  does not expose it yet.
- Never pass santhosh messages through; they contain the submitted value.

## 7. Ideas to steal for the compose REST module (with source)

Registry and documentation

- **Route metadata next to registration** (go-restful `Reads/Writes/Returns/Param`, huma
  `Operation`): the registry call is the single source of truth for method, path, tag,
  operationId, request type, response type, hidden, deprecated, timeout, security.
- **Panic on component name collision at startup** (huma registry): never silently merge two
  types. Package-qualified names make collisions rare; the panic catches the rest.
- **Params never enter the body component** (fizz separates params from body in its input
  schema; we enforce it with `json:"-"` on param fields). Input variants are named by type
  (`<pkg>.<Type>Input`), not by operation, so a request type shared by PATCH and PUT yields
  one component.
- **Deterministic document output for golden tests** (oaswrap/spec): sorted keys throughout,
  paths, components and properties alike. The spec is a committed file; CI diffs it on every
  PR and regenerates SDKs from it.
- **Declare the default error response once** (ogen `convenient_errors`, swaggest
  `AddRespStructure` with `IsDefault`): every operation references the same
  `handler.ErrorResponse` component (named by the namer) under `default`, so every generated SDK gets a typed
  error.
- **Security scheme declared once, opted out per operation** (huma `Security`,
  oapi-codegen): SDKs need it to send the header; `api.Public()` for Bootstrap-style
  unauthenticated endpoints.
- **`hidden`, `deprecated`, `doc`, `example` tags** (huma): free documentation quality.
  `readOnly`/`writeOnly` are not used; direction variants replace them and santhosh treats
  them as annotations only.
- **Generate 3.0.3 and 3.1 files side by side** (huma `Downgrade`): progenitor needs 3.0.x,
  hey-api and openapi-python-client prefer 3.1.

Validation

- **Schema-as-validator, validate the decoded `any`, then decode once** (huma, swaggest/rest,
  oapi-codegen strict + openapi3filter): one declaration, one truth, presence semantics for
  `required`, and nothing reaches the struct that the schema did not see.
- **Collect every error across locations before returning** (santhosh, kin `MultiError`,
  huma `ValidateResult`): never the swaggest behaviour of returning the first failing
  location in map order.
- **Typed error kinds mapped to our own messages** (santhosh `kind.*`, goa error-name
  vocabulary `missing_field`, `invalid_enum_value`, `invalid_format` with a `Field`
  pointer): stable, localisable, and nothing echoes the submitted value.
- **Case-exact keys with a did-you-mean check** (santhosh is case-sensitive; `encoding/json`
  and Echo are not): reject `"Status"` when `status` is declared, instead of silently
  binding it.
- **`null` on an optional request field means absent** (today's `encoding/json` behaviour,
  which huma's own validator breaks): request variants make pointer fields nullable.
- **Resolvers for cross-field rules** (huma `Resolver`): our `Validate()` already is this.
  Keep it mandatory, strip it to cross-field rules only, ship `requests.NoRules` to embed.
- **Validate responses in contract tests, not in production** (kin `ValidateResponse`,
  huma response modes): a compose `apitest` helper compiles the strict copy of the document
  and asserts every fixture response matches its declared `Res` schema.

Errors

- **One function every error class passes through** (ogen `ErrorHandler`, goa `formatter`,
  tonic `ErrorHook`): `rest.ErrorHandler` is it for registered routes and for every error
  Echo answers, `ManageError` for plain routes; both write the same envelope.
- **`request_id` in every error body** (own): keep. `rest.Configure` also sends it as the
  `X-Request-Id` response header for log correlation (v1.29.0).
- **Optional `fields` list in the envelope** (huma `ErrorDetail`, ogen `FieldError`, goa
  `Field`): `{"error":{"code","message","request_id","fields":[{"name","message"}]}}`.
  Deferred. The mapper builds the list internally now; exposing it is permanent once SDKs
  see it, so decide once a UI asks for per-field highlighting.

Types

- **Formats for compose types in one place** (huma `SchemaProvider`, done as a table in the
  reflector, section 5): `types.Email` and friends are described once; every app gets the
  right format.
- **Enum values registered by `reflect.Type` as named components** (own):
  `enums.Set[T]` stays the runtime truth and the schema reads from it (`enums.Values`).
- **Version-agnostic uuid format** (kin `FormatOfStringForUUIDOfRFC9562`, santhosh
  structural check): both apps use UUIDv7, the common RFC 4122 regex rejects it.
- **Pointer-to-struct response fields carry `omitempty`** (forced by huma: pointer-to-struct
  is never nullable in a response, and a `nullable:"true"` tag on an object ref fails
  registration): Go then omits instead of emitting `null`, which keeps progenitor's Rust
  `Option<T>` honest. This changes the wire format (section 11).

SDK pipeline

- **Generated clients committed in the API repo, not per-language repos** (own):
  `auditor3-api/sdk/python` (openapi-python-client, httpx) and `sdk/rust` (progenitor CLI
  output, separate crate not opting into ozai's workspace lints, consumed by ozai as a git
  dependency pinned to a rev). emma keeps emma-sdk-ts but regenerates from the new spec.
- **Spec diff gates SDK regeneration and version bump** (emma release.yml): keep that flow,
  feed it the committed golden spec.

Cut after review: `RejectUnknownQueryParameters` (not needed yet), serving both spec
versions at runtime (generate files instead), `readOnly`/`writeOnly`.

Related hardening, shipped separately from this design: Echo's `BodyLimit` through
`rest.WithBodyLimit` (one opt-in limit for the server, in bytes since v1.28.0; registered
operations also cap their own body, section 4 step 6), server timeouts in `rest.Serve`
(v1.29.0; always a 10s read-header and a 120s idle timeout, read and write timeouts unlimited
unless `WithReadTimeout`/`WithWriteTimeout` is passed; huma uses a 5s body read deadline),
and `rest.ErrorHandler` (v1.29.0), so 404, 405, 413 and 415 produce the envelope.

## 8. Sharp edges to design around

- The copy is huma's library mode, a public API upstream but not a documented pattern.
  compose uses: `Registry` (implemented by the reflector; its `RegisterTypeAlias` only
  because the interface requires it, nothing registers an alias), `Schema`, `SchemaFromType`,
  `SchemaFromField`, `DefaultSchemaNamer` (unnamed types), `NewMapRegistry` (as the
  document's component map), `OpenAPI` types, `Downgrade`, all from the copy in
  `internal/huma` (section 5). `Validate`, `PrecomputeMessages`, `Config` and `NewAPI` are
  not in the copy (the `NewAPI(cfg, nil)` trick to reach `FieldsOptionalByDefault` works
  upstream but is one setting for both directions and relies on a nil adapter being
  tolerated).
- huma's `go.mod` requires gin, fiber v2 and v3, echo v4 and v5, bunrouter and sonic
  (`go.mod:6-21`). As a dependency they joined MVS for compose and both apps; the copy in
  `internal/huma` removes them.
- `Downgrade` walks maps blindly and renames properties literally named `contentEncoding`
  or `contentMediaType`. The lint rejects those names.
- `OpenAPI.MarshalJSON` prunes components no path references. Good for output, surprising
  in tests.
- Generic type arguments lose their package in `DefaultSchemaNamer`, so compose's namer
  qualifies them (section 5). Lists stay app-declared anyway:

  ```go
  type ListResponse struct {
      pagination.CursorResponse
      Runs []project.Run `json:"runs"`
  }
  ```

- santhosh ignores OpenAPI 3.0 `nullable`. Always validate the 3.1 document.
- The pipeline does not call `c.Bind` (`handler/requests.go:74, 89` still do, for plain
  routes), and its Go code has no `if err := f(); err != nil` one-liners; AGENTS.md forbids
  them.

## 9. Work done and remaining

Done:

0. auditor3 is on compose v1.12.0, up from v1.8.5 (ez v1.6.0 signature change
   `ez.New(code, msg, err)`, scheduler v1.12 migration).
1. compose, this branch: the type table for `uuid.UUID` and `types/`, enum values recorded
   by `enums.Set`, `requests.NoRules`, `json:"-"` on `pagination.CursorRequest`.
2. compose, this branch: reflector glue and post-pass with golden files
   (`api/openapi/testdata/components.golden.json`, `api/testdata/openapi-3.{0,1}.golden.json`)
   and tests for enums, request variants (both directions, recursion), the namer, nullability,
   sized integers and the 3.0.3 downgrade.
3. compose, this branch: registry, pipeline, santhosh glue, error mapping, `apitest` helper,
   blob responses, content negotiation and multipart uploads (section 4).
4. Hardening, shipped separately (section 7): body limit, server timeouts, envelope for
   404/405/413/415.

The planned spike (port auditor3 `runs` onto the huma registry and santhosh directly) became
compose's test fixture: `components/rest/api/internal/fixture` mirrors auditor3's `runs`
module. Its tests cover the spike's checks that need no real app: the param walker picks up
the embedded `CursorRequest` and `json:"-"` keeps params out of the body component; field-level
`enum:"failed,cancelled"` on `*RunStatus` narrows the schema while the enum stays a named
component; presence `required` works, `null` is accepted on optional pointer fields, and
case-mismatched keys (`{"Status":"failed"}`) return 400; `?LIMIT=5000` is ignored (unknown
query parameters are not rejected and never bind), so the handler sees the default limit; the
published components have no `additionalProperties: false`; error messages never contain the
submitted value.

Remaining:

1. Release compose v1.30.0.
2. auditor3: all 50 routes migrate to the typed registry in one PR (there is no untyped
   registration to park routes on), hide `PUT /runs/:id`, commit `openapi.json` and
   `openapi-3.0.json`, CI spec diff plus hey-api, openapi-python-client and progenitor
   generation, contract tests. It also runs the spike checks that need the generators and the
   real server: progenitor's output has no `deny_unknown_fields`; the 3.0.3 file loads in
   progenitor and the 3.1 file in hey-api and openapi-python-client, each generating a named
   `RunStatus` enum and a typed error from `default`; progenitor reads a nullable field that
   narrows an enum, `{nullable: true, allOf: [{$ref}], enum: [...]}` in the 3.0.3 file, as
   optional (the sibling `enum` carries no `null`, which a strict 3.0.3 reader takes as
   forbidding it while the server accepts it; add `null` there if progenitor needs it); every success and error response is
   byte-identical to the current server for the ten `runs` operations, except the intended
   stricter validation cases.
3. emma: migrates once v1.30.0 is released, with a codemod for request structs
   (section 11). The generator is deleted in the change that commits the registry spec;
   there is no parallel generator lane.

## 10. Decisions from review round 4

- **One registry** plus the recursive variant pass. Two registries would duplicate every
  nested type and the collision handling.
- **Query coercion is our own code** (about 60 lines). `swaggest/form` decodes into structs,
  which is exactly the step being removed.
- **`Validate()` stays mandatory**: the pipeline calls `Body.Validate()` after schema validation
  and before the handler, so cross-field rules have one home. `requests.NoRules` is the embeddable
  no-op. Revisit after emma migrates.
- **`fields` in the envelope: later.** Build the list internally now.
- **Response validation lives in a compose `apitest` helper**, because it needs compose's
  compiled strict copy of the document.

## 11. Adopting in an app

- **Every registered route moves in one change.** There is no fallback to
  `App.HandleRequest` and no `Legacy` registration (section 4). `handler.App`,
  `HandleRequest`, `NewHandler` and `JSONResponse` stay for plain Echo routes.
- **Install the error handler.** Operations return every error to Echo (section 4), so
  adopting the registry includes `e.HTTPErrorHandler = rest.ErrorHandler(opts...)`, set
  before `Ready()`, which panics while Echo's default handler is installed. It logs
  nothing itself: in an app that runs neither `rest.Configure` nor `rest.Start` nothing logs a
  500. emma passes
  `rest.WithErrorReporter(handler.ReportToSentry)` and a `rest.WithErrorTranslator` that
  returns nil when `handler.Request(c)` is nil and `app.TranslateError(err,
  handler.Request(c))` otherwise; its `BaseHandler.ErrorTranslator` stays for its plain
  routes. auditor3 passes nothing: `rest.ErrorHandler()`.
- **The error handler is checked, wrapped ez errors are not.** `Ready()` probes the error
  handler and panics unless it answers an ez error with a 4xx, so Echo's default handler is
  caught at boot, and a reporter or translator passed to `rest.ErrorHandler` gets that probe
  once. An ez error wrapped in a non-ez error (`fmt.Errorf("...: %w", err)`) is answered as a
  500 `EINTERNAL`, so propagate with `ez.Wrap`.
- **A client that disconnects now cancels the handler's work.** The registry derives the
  request context from the inbound request, where `requests.New` roots it in
  `context.Background()`. A cancelled query that is propagated with `ez.Wrap`, or returned
  as it is, answers 499 and is not reported; one wrapped as `ez.New(ez.EINTERNAL, ..., err)`
  is a 500 and a Sentry event for every client that gave up. The same context is already
  cancelled when the error handler runs, so a translator or reporter reads values from
  `handler.Request(c).GetContext()` and does I/O with `c.Request().Context()`.
- **ozzo `Required` does not map to `required:"true"` alone.** ozzo's `validation.Required`
  rejects the zero value; schema `required` only checks the key is present, so `""`, `0` and
  `false` pass. Each converted rule needs `required:"true"` plus the constraint that keeps the
  zero value out where it must stay out (`minLength:"1"`, `minimum:"1"`, `minItems:"1"`, an
  `enum`). Sizes measured 2026-11-09: emma-backend 530 `validation.Required` uses in 504
  `Validate()` methods over 246 routes; auditor3-api 50 uses in 56 methods over 50 routes.
  The codemod for emma must apply this rule, not only suggest tags.
- **Converted endpoints change what they accept**: `0`, `false`, `""` now pass a presence
  `required` (above); stricter uuid and enum checks reject what ozzo let through;
  case-mismatched keys are rejected at every depth instead of bound. Contract tests per
  converted module (`apitest`).
- **Param fields get tags.** auditor3 `runs.GetRequest.RunID` must become `uuid.UUID` with
  `json:"-" path:"id"`, and `ListRequest` fields need `json:"-" query:"..."` tags.
  `pagination.CursorRequest` fields carry `json:"-"` and are query parameters only: check
  emma's POST search bodies first in case any carry `limit`/`cursor` in the body. A request
  that embeds `CursorRequest` does not embed `requests.NoRules` (section 3).
- **Component names.** emma has about 25 duplicated package names (`finance/units` vs
  `property/units`), so it needs `api.Namer(openapi.SegmentsNamer(2))`.
- **Compare the old spec with the registry's before switching** (emma): paths, methods and
  operationIds, because emma-sdk-ts method names and the MCP server's function names come
  from them.
- **Pointer-to-struct response fields get `omitempty`** (section 7): `null` becomes absent on
  the wire. Apply in auditor3 first; audit emma-sdk-ts consumers before emma.
- **emma's handlers change shape.** Its negotiating list handlers split into a JSON handler
  plus a `Produces` handler per other format instead of filling `Format` from
  `NegotiateContentType` (so `Validate()` stops requiring `Format`; query-format overrides
  become route middleware that rewrites `Accept`), reports that return serialized JSON as
  `[]byte` return a typed `Res` (registering `[]byte` would publish a base64 string), and
  binary handlers return `api.Blob`; its upload requests tag `File`/`Files` with
  `form:"..."` and `json:"-"`.
- **Accounting moves**, accepted for both apps. Schema, decode and `Validate()` failures are
  rejected before any app code but the authenticator runs (section 4), so they are never
  charged: emma's invalid-body quota charge and its 429-for-400 replacement go away for
  registered routes (a spent bearer device now gets the 400), and every `Validate()` failure
  becomes uncharged, not only cross-field ones (emma also normalizes there). Admission that
  has to stay ahead of the app moves to Echo middleware, which runs before schema validation:
  emma's IP peek (and its verified-bearer exception) and auditor3's API-key/IP charge now also
  see parse and schema failures, and an exhausted IP gets 429 for input the registry would
  have answered with 400. Admission that must follow validation (emma's public-route quota,
  the STP gateway exception) is an app-local typed wrapper (section 4).
- **Not expressible in the registry yet** (they stay plain Echo routes, outside the spec):
  emma's signed webhooks (`whatsapp.ReceiveWebhook`, `crm.CalWebhook`) need the exact raw
  body bytes for the signature check, and the system health handler answers non-200
  statuses, `HEAD` with no body and its own cache headers.
