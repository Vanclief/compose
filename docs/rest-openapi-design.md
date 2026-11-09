# compose REST: OpenAPI, schema-as-validator and SDK generation

Status: implemented in compose on branch `Vanclief/sdk-generation` (packages
`components/rest/api`, `components/rest/api/openapi`, `components/rest/api/validate`,
`components/rest/apitest`), not yet released (first ships in compose v1.29.0, on Echo v5).
Last updated 2026-11-09: retargeted from v1.27.0 on Echo v4 to v1.29.0 on Echo v5
(`github.com/labstack/echo/v5`). 2026-11-08: typed handlers and the authentication slot
replace `App.HandleRequest` dispatch on registered routes (section 4). 2026-10-20: `Legacy`
removed; blob responses, content negotiation and multipart uploads added. Section 12 lists
where the implementation deviates from the design.

## 1. Decision

Keep compose's REST layer as it is in spirit: Echo v5, the
`{"error":{"code","message","request_id"}}` envelope, `ManageError`, Sentry,
`requests.Request`. Add a declarative route registry that produces an OpenAPI document and
validates every request against that document before the app sees it. Registered routes call
one typed handler per operation instead of `App.HandleRequest`'s type switch, which stays for
plain Echo routes (section 4). Ozzo leaves the request path.

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
tags on aliased named types (section 5), so there is no remaining condition under which
swaggest wins.

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
reg := api.New(e, h,
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
reg.Ready() // panics on a bad registration or a protected op without an authenticator

// Spec is a committed golden file, regenerated by a test with -update. Serving it at
// runtime is optional.
doc31, err := reg.Spec(api.V31)
doc30, err := reg.Spec(api.V30)
```

Blobs and alternate representations (emma's shape; auditor3 has none):

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
- every path placeholder has a `path:` field and vice versa
- operationId `<tag>.<name>` is unique
- tag names come from an allowlist (`param`, `path`, `query`, `header`, `form`, `required`,
  `default`, `minimum`, `maximum`, `exclusiveMinimum`, `exclusiveMaximum`, `multipleOf`,
  `minLength`, `maxLength`, `pattern`, `minItems`, `maxItems`, `uniqueItems`, `enum`,
  `format`, `doc`, `example`, `deprecated`, `hidden`, `nullable`)
- `Res` is the handler's return type, so the documented response cannot drift from what is
  written; it must not be an interface type, and `*api.Blob` is rejected (a nil pointer would
  need a runtime check; handlers return `api.Blob` by value)
- a `Produces` handler takes the operation's `Req`: a built `Option` has lost its type
  parameters, so the compiler cannot check it and registration does
- every option is used at a scope it supports (`Info`, `APIKeyHeader`, `MaxBody`, ... on
  `api.New` only; `ContentType`, `Produces` on an operation only), and one options list
  cannot hold both `Public()` and `Authenticate()`

Dropped from the first sketch after review: `api.DefaultError` (`ManageError` always writes
`handler.ErrorResponse`, making it configurable invites drift), `api.Empty` (add it only when
an endpoint needs it, and then it means 204 via `c.NoContent`, never `200 null`),
`api.Tag` on groups (derived from the prefix, nested groups append), `SpecHandler`.

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
   group timeout (`api.Timeout`; auditor3 uses 60s, not the 15s default). ID, `c.RealIP()`,
   `Client` header, locale and method as before. `API-KEY` is a security scheme the
   authenticator reads, not a header param. The request is stored for `api.Request(c)` and
   the context sync of step 11 is deferred before anything can fail.
3. Negotiate the response media type from `Accept` (section 12) and put it on
   `request.GetAccept()` and `api.Accept(ctx)`. Requests that arrive before `Ready` fail
   here with `EINTERNAL`.
4. Coerce declared path, query and header params to the schema's type (integer, number,
   boolean, array via comma or repeated keys), dropping empty values (`?cursor=` is
   absent). If parsing fails keep the raw string so santhosh reports `kind.Type` like any
   other type error. Header keys lowercased, only declared `header:` fields included.
   Missing stays missing (presence semantics).
5. Read the body under the operation's limit (`api.MaxBody`, `api.MaxBodyBytes`). Decode
   with `jsonschema.UnmarshalJSON` (`loader.go:255-265`: `UseNumber`, rejects trailing
   data). A non-JSON content type on an operation with a body returns `EINVALID`, since
   Echo's binder no longer produces the 415. Multipart operations parse
   `multipart/form-data` instead (section 12).
6. Validate the synthetic `{path, query, header, body}` schema compiled at registration.
   `body` is required only when the body variant has required properties. Add the
   case-collision check: a body key that matches a declared property ignoring case but not
   exactly is an error (`unknown field "Status", did you mean "status"`). Collect every
   error. Map `kind.*` to one ez `EINVALID` with `field: reason` lines, never echoing
   submitted values. Return through `ManageError` (400, same envelope).
7. Decode once: `json.Unmarshal(bodyBytes, req)` for the body, then set param fields by
   field index from the coerced values, using `TextUnmarshaler` for named types
   (`uuid.UUID`, enums). `request.SetBody(req)`.
8. `Body.Validate()`, once, for cross-field rules.
9. The operation's `Authenticate` function, unless it is `Public()`.
10. The typed handler of the negotiated media type, exactly one: the `Produces` handler
    declared for it, else the primary handler. It receives `request.GetContext()` as it is
    after `Authenticate`. An error from any step goes through `ManageError`, which records
    it for `handler.HandledError(c)` before any `ErrorTranslator` runs; a result is written
    with `c.JSON(200, res)`, or `c.Blob` for an `api.Blob` (and `Content-Disposition` when
    it has a filename).
11. On every exit, errors and the not-ready case included, the registry stores
    `context.WithoutCancel(request.GetContext())` on the Echo request and then cancels its
    own timer. Middleware after `next(c)` therefore sees what the authenticator added and
    can still do I/O with `c.Request().Context()`, but that context no longer carries the
    registry's deadline, nor any cancellation.

**`Validate()` runs before `Authenticate`.** Malformed and cross-field-invalid requests never
reach the authenticator or the handler: no charge made there or in a typed wrapper, no
database write (emma's authenticator updates `last_active_at`), and the handler never sees
an invalid body (Echo middleware still sees them, step 1). Neither app's authenticator needs the body, and `Validate()` cannot
use the requester anyway: it receives neither a context nor the request. Identity-dependent
checks stay in the handler or module. The cost is that cross-field messages are visible
before authentication.

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
request through `ManageError`.

- **Inheritance**: options of `api.New`, then of each group from the outside in, then of the
  operation; the nearest wins. `Authenticate(fn)` sets the function and marks the operations
  protected, so a route under a `Public()` group can restore protection. `Public()` skips
  authentication and drops the security requirement from the spec. Both in one options list
  panic at boot.
- **`Ready()`** panics listing every protected operation, hidden ones included, that has no
  authenticator. An operation is never unauthenticated by omission.
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
        err := next(c)
        _, status := echo.ResolveResponseStatus(c.Response(), err) // sent status, else err's (500 if none)
        ctx := c.Request().Context() // read after next(c): the registry synced it back
        event := log.Info().
            Str("id", requests.RequestID(ctx)).
            Str("route", c.Path()).
            Int("status", status).
            Dur("latency", time.Since(started)).
            Err(handler.HandledError(c)) // what ManageError wrote, before translation
        request := api.Request(c) // nil when no registered operation ran
        if request != nil {
            event = event.Type("body_type", request.GetBody())
        }
        requester := user.FromContext(ctx)
        if requester != nil {
            event = event.Interface("user_id", requester.ID)
        }
        event.Msg("Request Handled")
        return err
    }
}
```

Left out of v1 on purpose:

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
  change; plain Echo routes (`handler.NewHandler`, `JSONResponse`) are the boundary.

## 5. Reflector glue (huma registry as a library)

The registry only produces component schemas. In library mode it never produces parameters
(param extraction lives in `huma.Register`, `huma.go:202`), it hardcodes
`additionalProperties: false` on every struct (`registry.go:216`, applied at
`schema.go:973, 985`), and it dereferences pointers before the alias lookup
(`registry.go:94, 102`) so nullability of aliased types is ours to set. The post-pass is
therefore a real component, about 300 lines plus golden fixtures, done in one walk over the
Go types that also runs the tag lint:

- `reg := huma.NewMapRegistry("#/components/schemas/", namer)` with a package-qualified
  namer: `project.Run`, `runs.ListResponse`. Handle generic type arguments explicitly
  (`Page[a.User]` and `Page[b.User]` otherwise collide and panic).
- **Parameters**: compose walks `reflect.VisibleFields` of the request type (this is what
  picks up embedded `pagination.CursorRequest`), builds `path`/`query`/`header` parameter
  objects, and uses `huma.SchemaFromField` per field for constraints.
- **Request variants**: for every component reachable from a request type, emit
  `<pkg>.<Type>Input` with `Required` rewritten to "fields with `required:"true"`" (path
  params are always required), and pointer fields made nullable (`type: [T, "null"]`) so
  `"failure_reason": null` keeps meaning absent, as `encoding/json` treats it today. Recurse:
  a nested type gets its own variant whenever its rewritten schema differs, parents re-point
  their refs, cycles resolve by fixpoint. Deduplicate against the response component by
  comparing marshalled JSON. Response components keep huma's default (required unless
  `omitempty`/`omitzero`, pointers non-null).
- **Clear `additionalProperties: false`** on every published component. Converted endpoints
  must keep ignoring unknown request keys as today, and progenitor's typify turns `false`
  into `deny_unknown_fields`, which would make adding a response field break deployed Rust
  clients. Contract tests compile a strict copy with `false` re-injected on response
  components so undocumented response fields are still caught in CI.
- `uuid.UUID` registered through `RegisterTypeAlias` to a `SchemaProvider` emitting
  `{type:string, format:uuid}` (`TextUnmarshaler` types otherwise become plain `string`).
- **Enums as named components**: `enums.Set[T]` constructor registers a generic `enumOf[T]`
  alias whose `Schema(r)` registers a `<pkg>.<T>` component with the values and returns a
  fresh `&Schema{Ref: ...}` on every call. Two conditions verified in source: the provider
  must return a fresh pointer because the field-level `enum` tag mutates it
  (`schema.go:614`), and `$ref` with a sibling `enum` is emitted side by side
  (`schema.go:208, 217`), valid in 2020-12 where santhosh applies both. Field-level
  `enum:"failed,cancelled"` therefore narrows an aliased named type: `schema.go:572` →
  `registry.go:94-115` → `schema.go:788-792` → `schema.go:608-614`. Named components mean
  openapi-python-client and progenitor emit one enum type per Go type, not one per field.
- compose `types/` (Email, PhoneNumber, Locale, UnixSeconds) implement `huma.SchemaProvider`.
- Build the document on `huma.OpenAPI` types so `Downgrade()` is free for 3.0.3.
  `OpenAPI.MarshalJSON` prunes components no path references.
- operationId `<tag>.<name>` (`runs.update`) so hey-api nests clients. Every operation gets a
  `default` response pointing at `handler.ErrorResponse`. Security scheme `API-KEY` header
  applied to every operation unless `api.Public()`.
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

- `compiler.AddResource("spec.json", doc31)` once, then per operation compile a synthetic
  schema `{properties:{path,query,header,body}}` whose parts `$ref` into the document.
  Compiled schemas are immutable and goroutine-safe; the `Compiler` is not, so compile at
  registration in one goroutine.
- `AssertFormat()` on. Register `uuid` (structural, v7 passes by default), `email`, compose
  formats. Unknown formats (`int64`, `double`) are ignored, which is fine.
- Error mapping: `kind.Required{Missing}` becomes `field is required`, `kind.Type` becomes
  `field must be <want>`, `kind.Enum` becomes `field must be one of ...`, `kind.Format`
  becomes `field must be a valid <name>`, `kind.Minimum`/`Maximum`/`MinLength`/... become
  `field must be ...`. Instance location from `InstanceLocation` joined with `.`. Build a
  `[]FieldError{Name, Message}` internally even though the envelope does not expose it yet.
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
- **Deterministic document output for golden tests** (oaswrap/spec): sorted paths, sorted
  components, stable property order from struct order. The spec is a committed file; CI
  diffs it on every PR and regenerates SDKs from it.
- **Declare the default error response once** (ogen `convenient_errors`, swaggest
  `AddRespStructure` with `IsDefault`): every operation references the same
  `handler.ErrorResponse` component under `default`, so every generated SDK gets a typed
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
  tonic `ErrorHook`): `ManageError` already is it.
- **`request_id` in every error body** (own): keep. An `X-Request-ID` response header for log
  correlation is cheap and additive.
- **Optional `fields` list in the envelope** (huma `ErrorDetail`, ogen `FieldError`, goa
  `Field`): `{"error":{"code","message","request_id","fields":[{"name","message"}]}}`.
  Deferred. The mapper builds the list internally now; exposing it is permanent once SDKs
  see it, so decide once a UI asks for per-field highlighting.

Types

- **`SchemaProvider` on compose types** (huma): `types.Email` and friends describe
  themselves once; every app gets the right format.
- **Enum values registered by `reflect.Type` as named components** (own, enabled by huma
  `RegisterTypeAlias`): `enums.Set[T]` stays the runtime truth and the schema reads from it.
- **Version-agnostic uuid format** (kin `FormatOfStringForUUIDOfRFC9562`, santhosh
  structural check): both apps use UUIDv7, the common RFC 4122 regex rejects it.
- **Pointer-to-struct response fields carry `omitempty`** (forced by huma: pointer-to-struct
  is never nullable and `nullable:"true"` on an object ref panics): Go then omits instead of
  emitting `null`, which keeps progenitor's Rust `Option<T>` honest. This changes the wire
  format (`null` becomes absent). Apply in auditor3 first; audit emma-sdk-ts consumers before
  emma.

SDK pipeline

- **Generated clients committed in the API repo, not per-language repos** (own):
  `auditor3-api/sdk/python` (openapi-python-client, httpx) and `sdk/rust` (progenitor CLI
  output, separate crate not opting into ozai's workspace lints, consumed by ozai as a git
  dependency pinned to a rev). emma keeps emma-sdk-ts but regenerates from the new spec.
- **Spec diff gates SDK regeneration and version bump** (emma release.yml): keep that flow,
  feed it the committed golden spec.

Cut after review: `RejectUnknownQueryParameters` (not needed yet), serving both spec
versions at runtime (generate files instead), `readOnly`/`writeOnly`.

Related but separate hardening PR, not part of this design: Echo `BodyLimit(1 << 20)` with
per-group override (huma defaults to 1 MiB), `ReadHeaderTimeout`/`ReadTimeout` in
`rest.Start` (currently none; huma uses a 5s body read deadline), and a compose
`HTTPErrorHandler` so 404, 405, 413 and 415 produce the envelope (emma's router already does
this locally).

## 8. Sharp edges to design around

- huma library mode is a public API but not a documented pattern. We use: `Registry`,
  `Schema`, `SchemaProvider`, `SchemaFromField`, `RegisterTypeAlias`, `DefaultSchemaNamer`,
  `OpenAPI` types, `Downgrade`, all from the copy in `internal/huma` (section 5). `Validate`,
  `PrecomputeMessages`, `Config`, `NewAPI` are not used (the `NewAPI(cfg, nil)` trick to
  reach `FieldsOptionalByDefault` works today but is one setting for both directions and
  relies on a nil adapter being tolerated).
- huma's `go.mod` requires gin, fiber v2 and v3, echo v4 and v5, bunrouter and sonic
  (`go.mod:6-21`). As a dependency they joined MVS for compose and both apps; the copy in
  `internal/huma` removes them.
- `Downgrade` walks maps blindly and renames properties literally named `contentEncoding`
  or `contentMediaType`. Lint for those names.
- `OpenAPI.MarshalJSON` prunes components no path references. Good for output, surprising
  in tests.
- Generic type arguments lose their package in `DefaultSchemaNamer`. Our namer handles them
  or we avoid generic response wrappers. Lists stay app-declared:

  ```go
  type ListResponse struct {
      pagination.CursorResponse
      Runs []project.Run `json:"runs"`
  }
  ```

- Converted endpoints change what they accept: `0`, `false`, `""` now pass a presence
  `required`; stricter uuid and enum checks reject what ozzo let through; case-mismatched
  keys are rejected instead of bound. Contract tests per converted module.
- santhosh ignores OpenAPI 3.0 `nullable`. Always validate the 3.1 document.
- `pagination.CursorRequest` fields need `json:"-"` added. Check emma's POST search bodies
  first in case any carry `limit`/`cursor` in the body.
- The pipeline must not carry over compose's existing `if err := c.Bind(body); err != nil`
  one-liners (`requests.go:40, 57`); AGENTS.md forbids them.

## 9. Order of work

0. auditor3 prerequisite PR shipped alone: compose v1.8.5 to v1.12.x (ez v1.6.0 signature
   change `ez.New(code, msg, err)`, scheduler v1.12 migration).
1. compose: `SchemaProvider` on `types/`, enum alias registration in `enums.Set`, uuid
   alias, `requests.NoRules`, `json:"-"` on `pagination.CursorRequest`.
2. compose: reflector glue and post-pass plus golden tests on a fixture package covering
   every tag, embedded structs, pointers, enums, generics, request variants, recursion,
   3.0.3 downgrade.
3. compose: registry, pipeline, santhosh glue, error mapping, `apitest` helper, then blob
   responses, content negotiation and multipart uploads (section 12). Release as a minor.
4. auditor3: all 50 routes migrate to the typed registry in one PR (there is no
   untyped registration to park routes on), hide `PUT /runs/:id`, commit `openapi.json` and
   `openapi-3.0.json`, CI spec diff plus hey-api, openapi-python-client and progenitor
   generation, contract tests.
5. emma: migrates once the step 3 capabilities are released, with a suggest-only codemod
   for request structs. The generator is deleted in the change that commits the registry
   spec; there is no parallel generator lane.
6. Separate hardening PR: `BodyLimit`, server timeouts, envelope for 404/405/413/415.

## 10. Spike (two days, before step 2)

Port auditor3 `runs` on a branch using the huma registry plus santhosh directly, without the
registry sugar. Check, in order:

1. Compose's param walker picks up the embedded `CursorRequest`, and `json:"-"` keeps params
   out of the body component.
2. Field-level `enum:"failed,cancelled"` on `*project.RunStatus` narrows the schema, the
   provider returns a fresh pointer each call, and the enum is a named component.
3. Request variant: presence `required` works, `null` is accepted on optional pointer
   fields, `{"Max_Total_Runs": -1}` returns 400. `?LIMIT=5000` is ignored (unknown query
   parameters are not rejected and never bind), so `Limit` stays 0 and the app default applies.
4. The published document has no `additionalProperties: false`, and progenitor's output has
   no `deny_unknown_fields`.
5. 3.0.3 downgrade loads in progenitor, 3.1 loads in hey-api and openapi-python-client, each
   generates a named `RunStatus` enum and a typed error from `default`.
6. Every success and error response is byte-identical to the current server for the ten
   `runs` operations, except the intended stricter validation cases.
7. Error messages never contain the submitted value.

## 11. Decisions from review round 4

- **One registry** plus the recursive variant pass. Two registries would duplicate every
  nested type and the collision handling.
- **Query coercion is our own code** (about 60 lines). `swaggest/form` decodes into structs,
  which is exactly the step being removed.
- **`Validate()` stays mandatory** because both apps call `GetBody().Validate()` in
  `HandleRequest`. `requests.NoRules` is the embeddable no-op. Revisit after emma migrates.
- **`fields` in the envelope: later.** Build the list internally now.
- **Response validation lives in a compose `apitest` helper**, because it needs compose's
  compiled strict copy of the document.

## 12. Implementation notes (review round 5, 2026-10-11)

Deviations from the sections above, all accepted in review:

- **The reflector re-implements huma's `mapRegistry`** (`openapi/reflector.go`) instead of
  wrapping it. huma's registry passes itself, not the wrapper, into `SchemaFromType`
  (`registry.go:155`), so a wrapper would never see nested fields. The copy matches huma on
  recursion placeholders, the duplicate-name panic, anonymous numbering, `time.Time`,
  `TextUnmarshaler`, `SchemaProvider` and `SchemaTransformer` (the latter since dropped
  from the copy, nothing implements it). compose `types/` do not implement
  `huma.SchemaProvider`; the reflector has a type table instead, so huma stays confined to
  `components/rest/api/...`.
- **huma is copied, not imported**: the rest of huma's code compose uses (schema and tag
  parser, `mapRegistry`, the OpenAPI document types and `Downgrade`) lives in
  `components/rest/api/internal/huma`, and `go.mod` has no huma requirement (section 5).
- **`types.Optional[T]` uses a method hook, not `SchemaProvider`**: the reflector matches
  `OptionalType() reflect.Type` through an unexported interface and documents `T`'s schema
  made nullable (section 5, "Optional body fields"). This replaces the earlier idea of
  compose types implementing `huma.SchemaProvider`, which would have made `types/` import
  huma, which is now impossible since it is internal.
- **Enum components come from `enums.Set`**, which records values by `reflect.Type`
  (`enums.Values`). Repeated `Set` calls for one type record the union in first-seen order;
  narrow a field with the `enum:"a,b"` tag.
- **Tag** is the last non-parameter segment of the group prefix (`/runs/:id/issues` gives
  `issues`).
- **Validators compile against every component, including those only hidden operations use.**
  The public document prunes them.
- **`requests.NoRules` cannot be embedded next to `pagination.CursorRequest`**: the two
  promoted `Validate` methods are ambiguous and the compiler rejects the registration. A type
  that embeds `CursorRequest` already satisfies `requests.Body`.
- **Request body keys are checked recursively for case collisions** (`api/collisions.go`),
  because `encoding/json` matches case-insensitively at every depth, and registration fails
  when `encoding/json` would bind a key the schema does not declare (unexported embedded
  structs, `hidden` fields, embedded non-struct types, shadowed promoted fields).
- **Param conversion failures are 400**, not 500: santhosh's `date-time` accepts inputs that
  `time.Time` rejects.
- **Body size is capped in the pipeline** (`http.MaxBytesReader`, 1 MiB default,
  `api.MaxBody`), so the hardening PR no longer needs `BodyLimit` for registered routes.
- **GET, HEAD and DELETE operations must not have body properties**; registration fails and
  points at the `query:` tag.
- **`?LIMIT=5000` is ignored**, not rejected (section 10, check 3 updated).
- **No `Legacy` registration.** The typed `Legacy[Req, Res]` and `x-compose-legacy` were
  dropped before release: apps migrate every route to the typed registry, and the four
  capabilities below cover what emma-backend's handlers do through `BaseHandler`.
- **Blob responses**: `api.GET(g, "/report", "report", app.Report,
  api.ContentType("application/pdf"))` for a handler whose `Res` is `api.Blob`. It returns
  `api.Blob{ContentType, Body, Filename}` by value: `*api.Blob` is rejected at registration,
  since a nil pointer would need a runtime check. An empty `ContentType` means the negotiated
  type, `Filename` adds `Content-Disposition: attachment`; errors keep the JSON envelope.
  Registration fails for a Blob `Res` without `ContentType`, `ContentType` on any other
  `Res`, and `ContentType` or `Produces` declaring `application/json` (a JSON `Res` produces
  it). Declared types are lowercase and carry no parameters or wildcards (`text/plain`, while
  the Blob may send `text/plain; charset=utf-8`); the Blob's own `ContentType` is written as
  given.
- **Content negotiation**: `api.Produces("text/csv", app.ExportCSV)` adds a representation
  with its own handler, `func(ctx, *Req) (api.Blob, error)`, whose `Req` must be the
  operation's (checked at boot). The declared types are the primary one (`application/json`
  for a JSON `Res`, the `ContentType` of a Blob) then the `Produces` types in declaration
  order, none repeated. The pipeline picks the first `Accept` entry, in header order, that
  matches a declared type (`type/*` and `*/*` match the first declared type in the range),
  else the primary type; q-values are ignored and there is no 406. Exactly one handler runs:
  the negotiated type's `Produces` handler, else the primary one, so a handler never returns
  the wrong kind of value. Every registered operation sets the choice on
  `requests.Request.GetAccept()` and `api.Accept(ctx)`. The 200 response lists one content
  entry per declared type: `Res`'s schema for JSON, `{type: string, format: binary}` for the
  others (huma adds `contentMediaType: application/octet-stream`, which the 3.0 downgrade
  renames `x-contentMediaType`). `apitest.Contract.Do` checks a non-JSON 200's content type
  against the declared types.
- **Multipart uploads**: request fields of type `*multipart.FileHeader` or
  `[]*multipart.FileHeader` tagged `form:"name"` and `json:"-"` make the operation
  `multipart/form-data`. The other body fields are read from the form values named by their
  JSON keys (empty values are absent, unknown and case-mismatched keys are ignored), coerced
  like query params and validated against the body component; then they decode through
  `encoding/json`, so named types decode as in JSON bodies. File fields are checked for
  presence (`required:"true"`), and a single-file field rejects repeated parts. The body
  limit wraps `ParseMultipartForm` (32 MiB in memory, the rest spills to temporary files).
  Registration fails for file fields without both tags, `form` tags on other types, body
  fields a form value cannot carry (nested objects, maps, `[]byte`) and files on GET, HEAD
  or DELETE. The request body is documented as `multipart/form-data` with the body
  properties inline, files as binary strings (arrays of them for slices) and `required` by
  presence.
- **XML request bodies were considered and skipped**: no emma-backend or auditor3-api REST
  handler calls `BindedXMLResponse` (emma's only XML is an outgoing client), so there is no
  consumer. `BaseHandler.BindedXMLResponse` is unchanged; registered operations accept JSON
  and multipart bodies only.
- **Typed handlers, not `App.HandleRequest`** (handler contract review, 2026-11-08):
  registration takes the handler, `Authenticate`/`Public()` replace the app's own
  authentication step, and the pipeline calls `Validate()` (section 4). `handler.App`,
  `HandleRequest`, `NewHandler` and `JSONResponse` stay for plain Echo routes.
- **Accounting moves when an app migrates**, accepted for both apps. Schema, decode and
  `Validate()` failures are rejected before any app code runs, so they are never charged:
  emma's invalid-body quota charge and its 429-for-400 replacement go away for registered
  routes (a spent bearer device now gets the 400), and every `Validate()` failure becomes
  uncharged, not only cross-field ones (emma also normalizes there). Admission that has to
  stay ahead of the app moves to Echo middleware, which runs before schema validation:
  emma's IP peek (and its verified-bearer exception) and auditor3's API-key/IP charge now
  also see parse and schema failures, and an exhausted IP gets 429 for input the registry
  would have answered with 400. Admission that must follow validation (emma's public-route
  quota, the STP gateway exception) is an app-local typed wrapper (section 4).
- **`requests.NewWithContext` replaces the `context.Background()` request**: `requests.New`
  roots the context in `context.Background()`, so a client disconnect never cancelled the
  app's work and values set by Echo middleware were lost; that bug is latent in every
  `HandleRequest` route. Registered operations derive the context from the inbound one
  (section 4, step 2). `requests.New` is unchanged for plain Echo routes, which can switch
  on their own.
- **Not implemented**: the `Res`/`*Res` response check (moot: `Res` is the handler's return
  type), `fields` in the envelope (still deferred).
- **Known app adoption items**: emma has about 25 duplicated package names
  (`finance/units` vs `property/units`), so it needs `api.Namer`/`openapi.SegmentsNamer(2)`;
  auditor3 `runs.GetRequest.RunID` must become `uuid.UUID` with `json:"-" path:"id"`, and
  `ListRequest` fields need `json:"-" query:"..."` tags. emma's negotiating list handlers
  split into a JSON handler plus a `Produces` handler per other format instead of filling
  `Format` from `NegotiateContentType` (so `Validate()` stops requiring `Format`; query-format
  overrides become route middleware that rewrites `Accept`), reports that return serialized
  JSON as `[]byte` return a typed `Res` (registering `[]byte` would publish a base64 string),
  and binary handlers return `api.Blob`; its upload requests tag `File`/`Files` with
  `form:"..."` and `json:"-"`.
- **Not expressible in the registry yet** (they stay plain Echo routes, outside the spec):
  emma's signed webhooks (`whatsapp.ReceiveWebhook`, `crm.CalWebhook`) need the exact raw
  body bytes for the signature check, and the system health handler answers non-200
  statuses, `HEAD` with no body and its own cache headers.
