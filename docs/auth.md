# Authentication

Compose authenticates every client with one header:

```
Authorization: Bearer <token>
```

The token is opaque. Its prefix says what kind of credential it is. The application keeps one
authentication function that branches on the prefix and resolves every kind to the same requester
type.

This guide covers:

- [Tokens](#tokens)
- [The Authorization header](#the-authorization-header)
- [Cookies and the CSRF rule](#cookies-and-the-csrf-rule)
- [Wiring one authentication function](#wiring-one-authentication-function)
- [Sessions](#sessions)
- [Verifying provider ID tokens](#verifying-provider-id-tokens)
- [OAuth for MCP](#oauth-for-mcp)
- [MCP](#mcp)
- [Generator](#generator)
- [Migrating from API-KEY and API-SECRET](#migrating-from-api-key-and-api-secret)

The packages are:

| Package | Role |
| --- | --- |
| `components/auth/token` | Generates, checks and hashes tokens. |
| `components/rest/requests` | Reads the credential of a request (`Authenticate`, `ParseBearer`). |
| `components/auth/session` | Issues and resolves rotating session tokens for first-party apps. |
| `components/auth/oidc` | Verifies Google, Apple and Microsoft ID tokens and returns a normalized identity. |
| `components/mcp/oauth` | OAuth 2.1 server for hosted MCP clients, and `Verify` for the REST side. |
| `components/mcp` | Forwards the MCP client's bearer to the REST API (`BearerPassthrough`). |
| `components/rest/generator` | Documents the bearer scheme in the OpenAPI spec. |

## Tokens

Every token is `<prefix>_<body>`. The body is 43 base62 characters (`[0-9A-Za-z]`) drawn
uniformly with `crypto/rand`, which is 256 bits of entropy. Applications store only the SHA-256
of the token (`token.Hash`) and look it up by that digest.

With `acme` as the application prefix:

| Kind | Format | Lifetime (default) | Issued by |
| --- | --- | --- | --- |
| API key | `acme_<43>` | Defined by the application. | The application, with `token.Generate("acme")`. |
| OAuth access token | `acmeat_<43>` | 1 hour (`AccessTokenTTL`). | `oauth.Server`, at `/oauth/token`. |
| OAuth refresh token | `acmert_<43>` | 30 days (`RefreshTokenTTL`), counted from the code exchange. Rotated on every use; the new token keeps the expiry of the first one, so 30 days bounds the whole connection. | `oauth.Server`, at `/oauth/token`. |
| Session | `acmes_<43>` | 30 days idle (`IdleTTL`), 90 days max (`MaxTTL`). Rotated on the first use after 1 hour of age (`RotateAfter`); the old token stays valid for 60s (`Grace`) after the rotation while the new one is unused, and for 60s after the new one is first used. | `session.Manager.Issue`. |

Prefix rules:

| Where | Rule | Why |
| --- | --- | --- |
| `token.Generate`, `token.ValidPrefix` | `^[a-z0-9]{2,12}$` | The base rule. A prefix has no `_`. |
| `oauth.New` (`Config.TokenPrefix`) | `^[a-z0-9]{2,10}$` | `at` and `rt` are appended. |
| `session.New` (`Config.Prefix`) | `^[a-z0-9]{2,11}$` | `s` is appended. |

Using the same `acme` for all three gives four distinct prefixes: `acme`, `acmeat`, `acmert` and
`acmes`. Pick prefixes so that no kind starts like another.

The `token` package:

| Function | Does |
| --- | --- |
| `Generate(prefix) (Token, error)` | Returns `Token{Value, Hash, Hint}`. Show `Value` once, store `Hash`, list keys by `Hint` (the last four characters). `ez.EINVALID` for a bad prefix. |
| `Hash(value) []byte` | SHA-256 of the value. Deterministic and unsalted. |
| `Prefix(value) (string, bool)` | The part before the first `_`. `false` when there is no `_` or the prefix is empty. |
| `Check(prefix, value) bool` | The value has this prefix and exactly 43 base62 characters after `_`. Use it before hashing so garbage never reaches the database. |
| `Equal(a, b []byte) bool` | Compares two hashes in constant time. |

## The Authorization header

`requests.ParseBearer` is the single parser. `Authenticate` and the MCP server both use it.

- The value is `Bearer <token>`. The scheme compares case-insensitively (`bearer x` works).
- One or more spaces or tabs separate the scheme from the token, as RFC 7235 allows. The MCP
  SDK's own bearer check is looser: it reads the first header and splits on any whitespace. A
  header it admits but `ParseBearer` rejects reaches the REST API with no credentials and is
  refused there.
- The token is not empty and contains no whitespace.
- A request with more than one `Authorization` header is malformed.

| Header | Result |
| --- | --- |
| Absent | `ez.ENOTAUTHENTICATED` |
| `Bearer abc`, `Bearer  abc`, `Bearer\tabc` | `abc` |
| Empty, `Bearer`, `Bearer x y`, `Basic abc`, two headers | `ez.EINVALID`, message `Authorization header must be Bearer <token>` |

`StandardRequest.Authenticate(opts ...requests.AuthOption)` reads the credential in this order:

1. The `Authorization` bearer token.
2. The cookie named by `requests.WithCookie`, when that option is passed.

It returns an `*AuthenticatedRequest`:

| Field | Meaning |
| --- | --- |
| `Token` | The bearer token, or the cookie value. |
| `FromCookie` | `true` when `Token` came from the cookie. |

Errors:

| Code | When |
| --- | --- |
| `ez.EINVALID` | The `Authorization` header is present but malformed. The cookie is not tried. |
| `ez.ENOTAUTHORIZED` | The request carries the cookie and fails the CSRF rule. |
| `ez.ENOTAUTHENTICATED` | No credentials at all. |

`Authenticate` only extracts the credential. The application verifies it. Through
`ez.ErrorToHTTPStatus`, `EINVALID` answers 400, `ENOTAUTHORIZED` 403 and `ENOTAUTHENTICATED` 401.

The legacy `API-KEY` and `API-SECRET` pair is not part of `Authenticate`.
`StandardRequest.LegacyCredentials(keyHeader, secretHeader)` returns it on its own: both headers
absent is `ez.ENOTAUTHENTICATED`, only one present is `ez.EINVALID`. See
[the migration](#migrating-from-api-key-and-api-secret) for how to compose the two.

## Cookies and the CSRF rule

Web apps send the session token in a cookie:

```go
req := requests.New(c.Request().Header, c.RealIP(), requests.WithMethod(c.Request().Method))
authReq, err := req.Authenticate(requests.WithCookie("acme_session", "https://app.acme.com"))
```

`WithCookie(name, allowedOrigins...)` reads the cookie only when no `Authorization` header is
present. A cookie with an empty value is skipped. A request authenticated by cookie must pass the
CSRF rule:

- `GET`, `HEAD` and `OPTIONS` pass.
- Any other method needs an `Origin` header listed in `allowedOrigins`. The match is exact,
  including case.
- With no `allowedOrigins`, cookies authenticate only `GET`, `HEAD` and `OPTIONS`.
- The method comes from `requests.WithMethod`, passed to `requests.New`. Without it `Method` is
  empty, and an empty method is treated as unsafe: a cookie then authenticates only a request
  whose `Origin` is listed, whatever its real method.

A failed check is `ez.ENOTAUTHORIZED` with the message
`Origin is not allowed for cookie authentication`.

A cookie only reaches an API on another origin when the browser is allowed to send it:
`SameSite=Lax` limits that to the same site, and the response needs credentialed CORS. With
`rest.Start` pass `rest.WithCredentialedCORS()` together with `rest.WithAllowedOrigins(...)`.

The session token may also arrive in the `Authorization` header, for example from a mobile app
that keeps it in the keychain. Only the cookie path runs the CSRF rule. `FromCookie` tells the two
apart, and `session.Manager.SetRotated` uses it to decide how to return a rotated token.

## Wiring one authentication function

The application keeps one function that branches on `token.Prefix(value)`:

| Prefix | Credential | Resolved with |
| --- | --- | --- |
| `acme` | API key | A lookup by `token.Hash` in the application's API key table. |
| `acmeat` | OAuth access token | `oauth.Server.Verify` |
| `acmes` | Session | `session.Manager.Resolve` |
| anything else | Not accepted here | `ez.ENOTAUTHENTICATED` |

A refresh token (`acmert_`) is only sent to `/oauth/token`. The example below rejects it with the other unknown prefixes.

### The API key table

Compose does not ship an API key table. A table shaped like this fits the token package:

| Column | Notes |
| --- | --- |
| `secret_hash` | `token.Hash(value)`, unique index. The value itself is never stored. |
| `hint` | `token.Hint`, the last four characters, safe to show in lists. |
| `expires_at` | Nullable. The application decides the lifetime. |
| `last_used_at` | Updated on use. |

Look the key up by `secret_hash` through the unique index. When the row is found by another id
instead, compare hashes with `token.Equal`.

### Example

```go
package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/uptrace/bun"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/auth/session"
	"github.com/vanclief/compose/components/auth/token"
	"github.com/vanclief/compose/components/mcp/oauth"
	"github.com/vanclief/compose/components/rest/requests"
)

const (
	prefix        = "acme"
	sessionCookie = "acme_session"
)

// Requester is who a request acts for, whatever credential it carried.
type Requester struct {
	AccountID string
	Via       string
	Scope     string
}

// APIKey is the application's key table.
type APIKey struct {
	bun.BaseModel `bun:"table:api_keys"`

	ID         string    `bun:"id,pk"`
	AccountID  string    `bun:"account_id,notnull"`
	SecretHash []byte    `bun:"secret_hash,notnull,unique"`
	Hint       string    `bun:"hint,notnull"`
	ExpiresAt  time.Time `bun:"expires_at,nullzero"`
	LastUsedAt time.Time `bun:"last_used_at,nullzero"`
}

type Auth struct {
	db       *bun.DB
	oauth    *oauth.Server
	sessions *session.Manager
}

// Authenticate builds the compose request and resolves its credential.
func (a *Auth) Authenticate(c echo.Context) (*requests.AuthenticatedRequest, Requester, error) {
	req := requests.New(c.Request().Header, c.RealIP(), requests.WithMethod(c.Request().Method))
	authReq, err := req.Authenticate(requests.WithCookie(sessionCookie, "https://app.acme.com"))
	if err != nil {
		return nil, Requester{}, ez.Wrap(err)
	}
	ctx := authReq.Context

	kind, _ := token.Prefix(authReq.Token)
	// Only sessions travel in the cookie.
	if authReq.FromCookie && kind != prefix+"s" {
		return nil, Requester{}, ez.New(ez.ENOTAUTHENTICATED, "Unknown credential", nil)
	}

	switch kind {
	case prefix:
		requester, err := a.ResolveAPIKey(ctx, authReq.Token)
		if err != nil {
			return nil, Requester{}, ez.Wrap(err)
		}
		return authReq, requester, nil
	case prefix + "at":
		grant, err := a.oauth.Verify(ctx, authReq.Token)
		if err != nil {
			return nil, Requester{}, ez.Wrap(err)
		}
		return authReq, Requester{AccountID: grant.Subject, Via: "oauth", Scope: grant.Scope}, nil
	case prefix + "s":
		s, rotated, err := a.sessions.Resolve(ctx, authReq.Token)
		if err != nil {
			return nil, Requester{}, ez.Wrap(err)
		}
		// A new cookie for web clients, the Session-Token header for the rest.
		a.sessions.SetRotated(c.Response(), sessionCookie, rotated, authReq.FromCookie)
		return authReq, Requester{AccountID: s.Subject, Via: "session"}, nil
	}
	return nil, Requester{}, ez.New(ez.ENOTAUTHENTICATED, "Unknown credential", nil)
}

// ResolveAPIKey finds a live API key by the hash of its value.
func (a *Auth) ResolveAPIKey(ctx context.Context, value string) (Requester, error) {
	if !token.Check(prefix, value) {
		return Requester{}, ez.New(ez.ENOTAUTHENTICATED, "Unknown API key", nil)
	}
	key := new(APIKey)
	err := a.db.NewSelect().Model(key).Where("secret_hash = ?", token.Hash(value)).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return Requester{}, ez.New(ez.ENOTAUTHENTICATED, "Unknown API key", nil)
	}
	if err != nil {
		return Requester{}, ez.New(ez.EINTERNAL, "Reading the API key failed", err)
	}
	if !key.ExpiresAt.IsZero() && !time.Now().Before(key.ExpiresAt) {
		return Requester{}, ez.New(ez.ENOTAUTHENTICATED, "Expired API key", nil)
	}
	_, err = a.db.NewUpdate().Model(key).Set("last_used_at = ?", time.Now()).WherePK().Exec(ctx)
	if err != nil {
		return Requester{}, ez.New(ez.EINTERNAL, "Updating the API key failed", err)
	}
	return Requester{AccountID: key.AccountID, Via: "api_key"}, nil
}

// CreateAPIKey stores a new key and returns its value, shown to the user once.
func (a *Auth) CreateAPIKey(ctx context.Context, id, accountID string) (string, error) {
	tok, err := token.Generate(prefix)
	if err != nil {
		return "", ez.Wrap(err)
	}
	key := &APIKey{ID: id, AccountID: accountID, SecretHash: tok.Hash, Hint: tok.Hint}
	_, err = a.db.NewInsert().Model(key).Exec(ctx)
	if err != nil {
		return "", ez.New(ez.EINTERNAL, "Saving the API key failed", err)
	}
	return tok.Value, nil
}
```

`SetRotated(w, cookieName, rotated, fromCookie)` does nothing when `rotated` is empty. When the
request came with the cookie it sets a fresh cookie. Otherwise it writes the new token to the
`Session-Token` response header (`session.TokenHeader`). Clients that keep the token themselves
must read that header and replace the stored token.

A browser app on another origin, for example `admin.acme.com` calling `api.acme.com`, can only
read that header when the response carries `Access-Control-Expose-Headers: Session-Token`.
`rest.Start` exposes it by default. An application with its own CORS middleware must add it, or
the app keeps sending the previous token, which is refused `Grace` after the rotation and the
session is deleted.
Cookies cross-origin also need `Access-Control-Allow-Credentials: true`, which `rest.Start` sends
only with `rest.WithCredentialedCORS()` together with `rest.WithAllowedOrigins(...)`.

## Sessions

`components/auth/session` issues and resolves rotating session tokens for first-party apps.

```go
err := session.CreateTable(ctx, db, "sessions")
if err != nil {
	return ez.Wrap(err)
}
sessions, err := session.New(session.Config{
	Prefix: "acme",
	Store:  session.NewBunStore(db, "sessions"),
})
if err != nil {
	return ez.Wrap(err)
}
```

### Config

A zero duration takes its default. Negative durations other than `session.Never` are rejected
with `ez.EINVALID`.

| Field | Default | `session.Never` |
| --- | --- | --- |
| `Prefix` | Required, 2 to 11 characters. | |
| `Store` | Required. | |
| `IdleTTL` | 30 days | No idle expiry. Otherwise it must be longer than `TouchEvery`, or `New` fails with `ez.EINVALID`. |
| `MaxTTL` | 90 days | No max expiry. `Session.ExpiresAt` stays zero (NULL in the bun store). Also the cookie `MaxAge`, see [Cookies](#cookies). |
| `RotateAfter` | 1 hour | The token never rotates. |
| `Grace` | 60s | Rejected. Grace must be bounded. It must also exceed the slowest authenticated handler: the new token travels with the response, and another tab still sending the old token deletes the session once `Grace` has passed. |
| `TouchEvery` | 1 minute | Rejected. Must be at least one second, or every request would write and concurrent requests on one session would fail on the optimistic lock, and shorter than a finite `IdleTTL`, so coalesced writes cannot swallow idle expiry. |

### Issue, Resolve and Revoke

| Method | Does |
| --- | --- |
| `Issue(ctx, subject) (string, Session, error)` | Creates a session and returns the token. `ez.EINVALID` for an empty subject. |
| `Resolve(ctx, value) (Session, string, error)` | Authenticates the token. The second result is the rotated token, empty when this call did not rotate. |
| `Revoke(ctx, value) error` | Deletes the session of the token. |
| `RevokeSubject(ctx, subject) error` | Deletes every session of the subject. |
| `Sweep(ctx) (int64, error)` | Deletes every session past `MaxTTL` or unused for `IdleTTL` (skipped when `IdleTTL` is `Never`) in one Store call and returns the count. Nothing calls it; run it on a ticker, as below. |

`Resolve` returns `ez.ENOTAUTHENTICATED` for a malformed, unknown, expired or reused token. Its
rules:

- **Expiry.** A session expires when unused for `IdleTTL` or `MaxTTL` after creation. `Resolve`
  deletes the expired row and fails. The delete follows a conditional write on the revision it
  read, so a session a concurrent request just extended is read again instead of deleted.
- **Rotation.** On the first use of the current token once `RotateAfter` has passed since the
  session was issued or last rotated, `Resolve` issues a new token. The presented token becomes the previous token.
  A rotation that comes due while the previous token is still inside its grace waits until that
  grace ends, so the previous token is never dropped early; a rotation can therefore run up to
  `Grace` late.
- **Grace.** The previous token stays accepted for `Grace` after the rotation while the new token
  has never been used, and for `Grace` after the new token's first use once it has been used, so
  requests already in flight from other tabs still succeed. `Resolve` succeeds quietly, with no
  write.
- **Reuse.** The previous token presented after that grace deletes the session. A rotation
  response lost on the wire therefore costs the user a login, and a previous token stolen before
  the rotation cannot take the session over: it is refused and the session ends. The same happens
  when the rotating request runs longer than `Grace` while another tab keeps sending the old
  token, or when the client aborts it before the response arrives, so `Grace` must exceed the
  slowest authenticated handler.

`Resolve` with the current token writes the session when something besides `last_used_at`
changed, or when the stored `last_used_at` is older than `TouchEvery`. A busy session therefore
writes its row at most once per `TouchEvery`, and idle expiry can fire up to `TouchEvery` early.
A previous token accepted inside its grace does not write.

### Concurrency

`Store.Update` writes every mutable column in one conditional write on the revision the request
read (`UPDATE ... SET revision = ? + 1 WHERE id = ? AND revision = ?`) and returns `ez.ENOTFOUND`
when no row matched. A request whose write matches nothing reads the session again and re-runs
the rules on what is stored now, up to three times, then fails with `ez.EINTERNAL`. Of many
requests racing past `RotateAfter` exactly one rotates and the others find their token as the
previous one inside `Grace`. A reuse delete decided while a concurrent request was first using the
current token misses: the retry sees the first use and accepts the previous token inside `Grace`
of it. Deletes decided by `Resolve` (expiry, reuse) go through `Store.DeleteRevision`, one conditional `DELETE ... WHERE id = ? AND
revision = ?` that also returns `ez.ENOTFOUND` and retries when the row changed, so a stale read
never deletes a session a concurrent request just used. `Revoke` uses the unconditional
`Store.Delete`. A custom `Store` must implement `Update` as one atomic compare-and-set and
`DeleteRevision` as one atomic compare-and-delete on `revision`; `Record.Revision` is 0 on
insert.

### Cookies

| Method | Returns |
| --- | --- |
| `Cookie(name, value)` | `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`. `MaxAge` from `MaxTTL`, or from `IdleTTL` when `MaxTTL` is `Never`. When both are `Never` it is a session cookie (`MaxAge` 0). The cookie is only re-sent on rotation, so `MaxAge` follows `MaxTTL` rather than `IdleTTL`; `Resolve` still enforces `IdleTTL`. With `MaxTTL` and `RotateAfter` both `Never` the cookie expires `IdleTTL` after login unless the application sends it again. |
| `ClearCookie(name)` | The same attributes, an empty value and `MaxAge` -1. |
| `SetRotated(w, name, rotated, fromCookie)` | See [the example](#example). |

### Stores

| Store | Notes |
| --- | --- |
| `NewBunStore(db, table)` | PostgreSQL or SQLite. Columns `id`, `subject`, `token_hash`, `previous_hash`, `created_at`, `last_used_at`, `rotated_at`, `first_used_at`, `expires_at` and `revision` (the optimistic-lock counter, 0 on insert). |
| `CreateTable(ctx, db, table)` | Creates the table if it does not exist, with a unique index on `token_hash` and plain indexes on `previous_hash`, `subject`, `expires_at` and `last_used_at`, named `<table>_token_hash`, `<table>_previous_hash`, `<table>_subject`, `<table>_expires_at` and `<table>_last_used_at`. Call it at startup; on an existing table it adds the missing indexes. |
| `NewMemoryStore()` | In process memory, for tests and single-process deployments. |

`Resolve` deletes an expired session it meets, but a session whose token is never presented again
(cookies cleared, device lost) stays until `Sweep` removes it. Run `Sweep` periodically:

```go
go func() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, err := sessions.Sweep(ctx)
			if err != nil {
				logger.Error("sweeping sessions failed", "error", err)
			}
		}
	}
}()
```

`Sweep` deletes rows with `expires_at <= now` (a `NULL` `expires_at`, from `MaxTTL` `Never`, never
matches) and, unless `IdleTTL` is `Never`, rows with `last_used_at <= now - IdleTTL`. `LastUsedAt` is
written at most once per `TouchEvery`, so a session can be swept up to `TouchEvery` before its idle
expiry, as `Resolve` already allows. A custom `Store` implements this as `DeleteExpired(ctx, now,
idleBefore)`; a migration written by hand adds the `expires_at` and `last_used_at` indexes.

## Verifying provider ID tokens

`components/auth/oidc` verifies the OpenID Connect ID tokens of Google, Apple and Microsoft.
Clients obtain an ID token from the provider's SDK and POST it to the application.
`Verifier.Verify` checks it and returns a normalized `Identity`. What the application does with
the identity (match a user, link accounts, issue a session) is the application's job.

The package fetches the provider's discovery document and signing keys itself. The token is
parsed, confined to the compact form and RS256 (which is what all three providers sign with) and
its signature verified by `github.com/go-jose/go-jose/v4`; the package reads no claim before that
succeeds. On the verified claims it then checks expiry, issuer, audience against `ClientIDs`,
nonce and the provider rules below.

### Config

`New(cfg Config) (*Verifier, error)` validates the configuration and does no network call. It
fails with `ez.EINVALID` when no provider has `ClientIDs`, when a client id is empty, when an
`Issuer` is not an absolute http(s) URL, when a Microsoft `Tenant` is not accepted or an `Issuer`
is set with a multi-tenant `Tenant`. Discovery and the key set are fetched lazily, the first
time a provider's `Verify` meets a token whose key is not cached. A fetched key set serves for
24 hours, then the next `Verify` starts a refresh behind it without waiting (Microsoft's
guidance is a refresh at least daily), so a key the provider removed stops verifying once that
refresh lands: within a day and a fetch under steady traffic. A service with no sign-ins for
days serves the first token after the quiet period from the cache; the 7 day limit below
bounds that. While a refresh fails the cached keys keep serving, with a retry at most once a
minute, for up to 7 days; after that `Verify` fails with the fetch's error rather than trust
keys the provider may have withdrawn. Set `OnFetchError` to hear about those failures while
the cache still covers them. A key fetch answered with a 4xx runs discovery again on the next
attempt, so a provider that moves its key endpoint is followed; a transport failure or a 5xx
keeps the known URL. `Verifier` is safe for concurrent use; build it once at startup.

| Field | Default | Notes |
| --- | --- | --- |
| `Google` | Disabled. | `ProviderConfig` for Google. |
| `Apple` | Disabled. | `ProviderConfig` for Apple. |
| `Microsoft` | Disabled. | `ProviderConfig` for Microsoft. |
| `HTTPClient` | A client with a 10 second timeout. | Used for discovery and key fetching. A fetch runs detached from the `Verify` call that started it, capped at 30 seconds whatever the client's timeout, so a caller hanging up neither stops the fetch the other callers wait for nor skips the pacing below. Callers with no cached key to serve wait for it until their own context ends. |
| `OnFetchError` | Nil, failures are not reported. | Called with the provider and the error each time a discovery or key fetch fails, from the fetch's own goroutine. `Verify` serves cached keys through failures for up to 7 days without surfacing them, so log or report them here to see an outage before sign-ins fail. |

`ProviderConfig`:

| Field | Default | Notes |
| --- | --- | --- |
| `ClientIDs` | Empty, which disables the provider. | The accepted audiences, one per client app (iOS, Android, web). `Verify` for a disabled provider is `ez.EINVALID`. |
| `Issuer` | The provider's public issuer, see [Provider rules](#provider-rules). | Overrides the issuer URL, for tests and private deployments. For Microsoft it needs `Tenant` set to a tenant id; with `common`, `consumers` or `organizations` it must be empty, or `New` is `ez.EINVALID`. A private or sovereign cloud sets `Tenant` to the tenant id and `Issuer` to that cloud's issuer URL, and the token's issuer must match it exactly. |
| `Tenant` | `common` | Microsoft only: `common`, `consumers`, `organizations` or a tenant id, in any case. A tenant id is a GUID in the 8-4-4-4-12 hex form and is lowercased. Anything else is `ez.EINVALID`. The three keywords are multi-tenant, public cloud only, and restrict the token's `tid`, see [Provider rules](#provider-rules). Ignored for the other providers. |
| `AllowMissingNonce` | `false` | Accepts a call with an empty nonce, see [Nonce](#nonce). Microsoft's MSAL libraries manage the nonce themselves, so applications set it for Microsoft. Without a nonce every token the provider signed for one of the `ClientIDs` counts as a sign-in; pass a nonce where the client library takes one (MSAL.js does). |

### Provider rules

Default issuers:

| Provider | Issuer | Notes |
| --- | --- | --- |
| Google | `https://accounts.google.com` | With this default, `accounts.google.com` without the scheme is accepted too, because Google has issued tokens in both forms. Nothing else is. |
| Apple | `https://appleid.apple.com` | |
| Microsoft | `https://login.microsoftonline.com/<Tenant>/v2.0` | For `common`, `consumers` and `organizations` every token names its real tenant: `iss` must match `https://login.microsoftonline.com/<tenant id>/v2.0` and the `tid` claim must equal that tenant id. `common` accepts any tenant, `consumers` only the personal-accounts tenant `9188040d-6c67-4c5b-b112-36a304b66dad`, and `organizations` any tenant but that one. For a fixed tenant id the issuer must match exactly. When the signing key carries an `issuer` property, its `{tenantid}` is replaced with the token's `tid` and the result must equal the token's `iss`, or the token is `ez.ENOTAUTHENTICATED`. |

Claims:

- `sub` is required for every provider.
- Microsoft access tokens are refused. When the app registration exposes an API, Microsoft signs
  its access tokens with the same keys, issuer and audience as the ID tokens, and an access token
  is not a sign-in. Delegated access tokens carry `scp`; app-only (client credentials) tokens carry
  no user, their `sub` equals `oid`, and `idtyp` is `app` when the registration asks for that
  claim. All three forms are `ez.ENOTAUTHENTICATED`. This matters most with `AllowMissingNonce`,
  where the nonce would not tell them apart; keep the sign-in app registration from exposing an
  API where you can.
- **Google.** `Email` from `email`, `EmailVerified` from the boolean `email_verified`,
  `HostedDomain` from `hd`, the Google Workspace domain, empty for personal accounts. To
  restrict sign-in to one workspace check `HostedDomain`, not the email suffix, as Google
  advises.
- **Apple.** `Email` from `email`. `email_verified` and `is_private_email` arrive as booleans or
  as the strings `"true"` and `"false"`; both forms are accepted. `PrivateEmail` is
  `is_private_email`. The user's name is never in the token.
- **Microsoft.** `Email` from `email`, only when present. `EmailVerified` is always `false`,
  because the claim is whatever the tenant admin typed. `preferred_username` is never read.

`Identity`:

| Field | Meaning |
| --- | --- |
| `Provider` | `oidc.Google`, `oidc.Apple` or `oidc.Microsoft`. |
| `Subject` | The provider's stable account id (`sub`). |
| `Email` | Empty when the token carries none. |
| `EmailVerified` | See the claims above. |
| `PrivateEmail` | An Apple "Hide My Email" relay address. |
| `HostedDomain` | Google's `hd`, the Workspace domain; empty for Apple and Microsoft. |
| `Issuer` | The token's issuer, for logging. |
| `Audience` | The entry of `ClientIDs` found in `aud`. |

### Nonce

`Verify` compares the token's `nonce` claim to its nonce argument, nothing more:

- The claim must equal the value or its SHA-256 as lowercase hex. Apple's native SDK sends the
  hash and the app passes the raw value; Google sends the value as given.
- An empty nonce argument is `ez.ENOTAUTHENTICATED`, unless the provider has
  `AllowMissingNonce`. Then the token passes with or without a `nonce` claim, since there is
  nothing to compare. MSAL manages the nonce itself, so Microsoft apps set it and rely on the
  token's short lifetime instead.

The check is only worth something when the application issued the nonce. Whoever holds a leaked
ID token can decode its `nonce` claim and send it as the argument, so a nonce the client
generates and forwards proves nothing. The application issues a random nonce per sign-in (for
example `POST /sign-in/nonce`, stored with a short TTL), the client hands it to the provider's
SDK, and the login handler consumes it exactly once before calling `Verify`. `Verify` cannot
enforce single use.

### Errors

| Code | When |
| --- | --- |
| `ez.EINVALID` | Unknown provider, provider with no `ClientIDs`, empty token. |
| `ez.ENOTAUTHENTICATED` | Bad signature, missing or unknown `kid`, a `kid` whose published entry cannot sign (the message names the `kid` and the reason: an encryption key, a key go-jose cannot read), wrong issuer, audience not in `ClientIDs`, expired, not yet valid, nonce mismatch or missing nonce, Microsoft access token. |
| `ez.EUNAVAILABLE` | The provider did not answer discovery or the key fetch (network error, non-200 status, a body that stalls or breaks off) or the caller's context ended while waiting for a fetch, with no cached key younger than 7 days to serve. |
| `ez.EINTERNAL` | The provider answered a malformed document, a discovery document naming another issuer or without `jwks_uri`, or a key set with no usable key, with no cached key younger than 7 days to serve. |

`ez.EUNAVAILABLE` and `ez.EINTERNAL` stay distinct from `ez.ENOTAUTHENTICATED` so an
application never answers "not registered" for an outage, and compose's handler maps
`ez.EUNAVAILABLE` to HTTP 503 so an outage is not reported as the service's own 500.
Messages are diagnostic strings. The application maps `ez.ENOTAUTHENTICATED` to its own
client-facing message.

The provider's keys are fetched again when a token names a `kid` that is not cached, at most
once a minute per provider: the first forged token with a made-up `kid` sent to the sign-in
route costs one outbound request, the ones in the minute after it are refused without one, and
a token that does not parse or has no `kid` costs none. One fetch runs at a time per provider,
discovery included; the tokens whose `kid` is cached are verified without waiting for it, and
the others wait for its result, success or failure, instead of downloading again. The minute also bounds a legitimate rollover: a token
signed with a key the provider added since the last fetch is refused until it passes.
Rate-limit the sign-in route anyway, for example with compose's `components/ratelimit`.

### Example

```go
verifier, err := oidc.New(oidc.Config{
	Google:    oidc.ProviderConfig{ClientIDs: []string{iosClientID, webClientID}},
	Apple:     oidc.ProviderConfig{ClientIDs: []string{bundleID, servicesID}},
	Microsoft: oidc.ProviderConfig{ClientIDs: []string{azureClientID}, AllowMissingNonce: true},
})
if err != nil {
	return ez.Wrap(err)
}
```

```go
// SignInRequest is the body of POST /sign-in.
type SignInRequest struct {
	Provider string `json:"provider"`
	IDToken  string `json:"id_token"`
	Nonce    string `json:"nonce"`
}

func (r *SignInRequest) Validate() error {
	return nil
}

// SignIn binds the body and hands it to the application.
func (h *Handler) SignIn(c echo.Context) error {
	req := requests.New(c.Request().Header, c.RealIP(), requests.WithMethod(c.Request().Method))
	return h.BindedJSONResponse(c, "SignIn", req, &SignInRequest{})
}

// SignIn runs for a SignInRequest. A Verify error keeps its ez code, which the
// compose envelope answers with its HTTP status.
func (a *Auth) SignIn(ctx context.Context, body *SignInRequest) (any, error) {
	// nonces is the application's store of issued nonces. Consume deletes the row
	// or fails with ez.ENOTAUTHENTICATED. Verify refuses an empty nonce itself
	// unless the provider has AllowMissingNonce.
	if body.Nonce != "" {
		err := a.nonces.Consume(ctx, body.Nonce)
		if err != nil {
			return nil, ez.Wrap(err)
		}
	}
	identity, err := a.verifier.Verify(ctx, oidc.Provider(body.Provider), body.IDToken, body.Nonce)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	// Matching a user, linking accounts and issuing a session are the application's.
	return a.app.SignIn(ctx, identity)
}
```

## OAuth for MCP

`components/mcp/oauth` lets hosted MCP clients such as claude.ai and ChatGPT connect. It hosts the
OAuth 2.1 authorization server and guards the MCP endpoint. No REST credential is stored: the MCP
server forwards the access token unchanged and the REST API resolves it with `Server.Verify`.

```go
err := oauth.CreateTable(ctx, db, "oauth_records")
if err != nil {
	return ez.Wrap(err)
}
authServer, err := oauth.New(oauth.Config{
	Issuer:      "https://api.acme.com",
	Resource:    "https://api.acme.com/mcp",
	TokenPrefix: "acme",
	Store:       oauth.NewBunStore(db, "oauth_records"),
	Login: oauth.Login{
		Title:     "Acme",
		Challenge: app.SendLoginCode,
		// Returns oauth.Grant{Subject: account.ID}.
		Verify: app.VerifyLoginCode,
	},
	// Claude, ChatGPT and native clients on a loopback port, such as
	// Claude Code.
	AllowedRedirectHosts: []string{"claude.ai", "claude.com", "chatgpt.com", "localhost", "127.0.0.1"},
	// Accepts the application's API keys at the MCP endpoint too. The
	// codes ResolveAPIKey uses for "not my token" are rejections; any
	// other error is a failed check and answers 500.
	Fallback: func(ctx context.Context, bearer string) (bool, error) {
		_, err := appAuth.ResolveAPIKey(ctx, bearer)
		if ez.ErrorCode(err) == ez.ENOTAUTHENTICATED {
			return false, nil
		}
		if err != nil {
			return false, ez.Wrap(err)
		}
		return true, nil
	},
})
if err != nil {
	return ez.Wrap(err)
}
server, err := mcp.New(mcp.Config{Spec: spec, Handler: e, Auth: authServer.AuthFunc()})
if err != nil {
	return ez.Wrap(err)
}
authServer.Mount(e, server.Handler())

// Expired rows (every row expires within RefreshTokenTTL or IdleTTL of its
// last write) stay in the table until swept. Nothing calls Sweep automatically.
go func() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		_, err := oauth.Sweep(ctx, db, "oauth_records")
		if err != nil {
			log.Println(err)
		}
	}
}()
```

`oauth.CreateTable` creates the table if it does not exist; call it at startup. `oauth.Sweep`
deletes the rows past their expiry in one statement and returns the count.

`AllowedRedirectHosts` restricts dynamically registered clients and Client ID Metadata Documents
to redirect URIs on those hosts, compared without port; `Clients` are not restricted. ChatGPT
sends the RFC 8707 `resource` parameter, which the server accepts when it names `Resource`.
Redirect URIs match exactly, except `http` URIs on a loopback host (`localhost`, `127.0.0.1`,
`::1`), which match ignoring the port (RFC 8252 §7.3): Claude Code registers
`http://localhost/callback` and returns to `http://localhost:<port>/callback`. A `redirect_uri`
sent to the token endpoint must be the URI used at authorize, port included.

### Your own consent page

Set `LoginURL` instead of `Login` to sign users in on the application's page:

1. Authorize sends the browser to `LoginURL` with the pending request id in the `request` query
   parameter.
2. `Pending(ctx, id)` returns the `ClientName`, `RedirectHost` and requested `Scope`. Show the
   client name and redirect host, sign the user in and wait for an explicit click.
3. On approval call `Approve(ctx, id, grant)`, which issues the authorization code. On refusal
   call `Deny(ctx, id)`, which issues nothing and returns the redirect URI with
   `error=access_denied` and the client's state.
4. Redirect the browser to the URL either returns.

`Approve` and `Deny` take the request from the Store, so each request is answered once. An
unknown, expired or already answered request is `ez.ENOTFOUND`.

### Grant

```go
type Grant struct {
	Subject string
	Scope   string
}
```

- `Login.Verify` returns the grant with `Subject` set, for example the account id. With
  `LoginURL`, the application's own page passes the grant to `Approve` instead. `Subject` is
  required: a grant without it fails the login with `ez.EINTERNAL`, since its tokens would carry
  no identity.
- `Scope` is exactly what `Verify` returned or `Approve` received, empty when the application
  granted none. The `scope` the client requested is never copied into it, so `Server.Verify`
  only returns a scope the application set and a REST handler may authorize on it. The requested
  scope only echoes in the `scope` field of the token response when the application set none.
  With `LoginURL`, `Pending.Scope` carries the requested scope so the application's page can
  grant, narrow or refuse it.
- An application that does not use scopes leaves `Scope` empty. Its tokens are unscoped: the
  server enforces nothing on them, and the token response echoes the requested scope because
  the client is refused nothing it asked for. An application that authorizes on scopes must set
  `Scope` on every grant, or refuse the request, so that its token responses state what was
  granted.

### Tokens and verification

- `TokenPrefix` is required, `^[a-z0-9]{2,10}$`. Access tokens are `<TokenPrefix>at_<43>`, refresh
  tokens `<TokenPrefix>rt_<43>`.
- The Store keeps only the hex SHA-256 of tokens and authorization codes as record ids.
- `Server.Verify(ctx, accessToken) (Grant, error)` resolves a live access token for the REST side.
  Malformed, unknown, expired or revoked tokens are `ez.ENOTAUTHENTICATED`. Every check reads the
  token row and then its family row, so a token of a revoked or expired family is refused on its
  next use. Store failures are `ez.EINTERNAL`. `accountsAPI` and `TestMCPWithAccessToken` in
  `components/mcp/oauth/mcp_test.go` show a REST handler that parses the bearer with
  `requests.ParseBearer` and calls `Verify`. An access token issued for `Resource` therefore
  authenticates the whole REST API, not only the tools the MCP server exposes, and any client
  the user approved acts as that user there. Limit what approved clients may do with the
  application's own authorization, for example on `Grant.Scope` set by the application.
- `Server.AuthFunc()` is the `mcp.Config.Auth` of the MCP server. For a live access token it
  returns `Authorization: Bearer <token>` unchanged. An unknown, expired or revoked access token
  fails with `ez.ENOTAUTHENTICATED`, so the client refreshes. Any other token goes to `Fallback`: an
  accepted token passes through the same way, a rejected one fails with `ez.ENOTAUTHENTICATED`, and
  a `Fallback` error is reported to `OnError` and returned as is. Without `Fallback` every other
  token fails with `ez.ENOTAUTHENTICATED`.
- `Fallback func(ctx context.Context, token string) (bool, error)` decides whether a token that is
  not shaped like an access token may reach the MCP endpoint: `true` accepts it, `false` rejects
  it with the 401 and challenge that start discovery, and an error means the check could not be
  made, which `Protect` reports to `OnError` and answers 500, so a store outage is never taken for
  a bad credential. It only answers at the MCP edge: the REST API authenticates the passed-through
  token again. It must reject tokens that are not its own. Tokens shaped like access tokens never
  reach it.

### Refresh rotation and family revocation

Refresh tokens rotate on every use. Each code exchange starts a token family. A refresh token
presented again after it was rotated revokes its family: the current access and refresh token stop
working on their next use. When both a thief and the client use a refresh token, the second use
ends the connection for both and the user logs in again. Access tokens issued before a rotation
stay valid until they expire, unless the family is revoked.

A refresh token presented twice also revokes its family, even when both requests come from the
client: a client whose rotation committed but whose response was lost retries with a rotated
token and must authorize again. A failure before the commit, a timeout included, leaves the
presented token untouched, and the retry rotates it. A client that is gone before the commit, its
own timeout fired while the Store was slow, gets no commit either: the swap is skipped, the rows
written so far are never published, and the retry rotates the presented token. Once the swap has
started it runs to the end.

The family row is the single source of truth. It holds the grant, the client, the expiry that
bounds the whole connection, the generation of the current refresh token and the id of that
token. Access and refresh rows only point at their family and carry the generation they were
issued in, and a token is live only while its family row is: only the refresh token the family
names rotates, and an access token of a later generation than the family's is refused. Rotation
writes the new access and refresh rows first, inert until published, then publishes them with one
conditional `Store.Swap` of the family row from the bytes it read to the next generation naming
the new refresh token. That swap is the commit and the last write before the response, so a
failure before it leaves the presented token untouched and the client simply retries, and a swap
that committed before running out of time is recognised by the family naming the new token and
answered as a success. A refresh token the family does not name is a rotated one presented again:
revoking is one idempotent delete of the family row, after which every token of the family is
refused. When that delete fails the answer is a retryable `server_error`, not `invalid_grant`, so
the client's retry repeats the detection instead of logging in anew while a thief's tokens live
on. Two requests presenting the same token at once both read the same generation; the one that
loses the swap re-reads the family, finds it moved on and revokes it. There are no tombstones,
marks or restore paths, and the family's persisted expiry bounds every token of the family: the
only setting read later is `AccessTokenTTL`, per access token and capped by that expiry. The Store
needs read-your-writes across instances (a single primary, no eventually consistent reads or read
replicas).

A rotated refresh row is never written again: it keeps the expiry it was issued with and is detected
as reuse until then, which is when the family expires under a finite `RefreshTokenTTL` and `IdleTTL`
after its issue under `Never`. Under `Never` the family has no fixed end: it is issued expiring
`IdleTTL` from then, and every rotation moves the family row and the new refresh row to `IdleTTL`
from the rotation, so the connection ends `IdleTTL` after the client last refreshed. `IdleTTL` is
kept with the family, so a change applies to new connections only. Every row therefore expires
within `RefreshTokenTTL` or `IdleTTL` of its last write, rows left by a failed rotation included,
and `oauth.Sweep` (see [the setup](#oauth-for-mcp)) removes them. The client renewal runs before the token rows are written, best effort:
its failure is reported and the exchange goes on, so the swap stays the last write before the
response.

### Config

A zero value takes the default. `New` rejects negative durations and a negative
`MaxLoginAttempts` with `ez.EINVALID`. `oauth.Never` is accepted only by `RefreshTokenTTL`; every other duration, `IdleTTL` included, rejects it.

| Field | Default | Notes |
| --- | --- | --- |
| `AccessTokenTTL` | 1h | |
| `RefreshTokenTTL` | 30 days | Bounds the whole connection; a rotation keeps the expiry of its family. `Never` puts no fixed end on it: the connection lives until the client goes `IdleTTL` without refreshing or reuses a rotated token. |
| `IdleTTL` | 30 days | Under `RefreshTokenTTL` `Never`, how long a connection lives after its last refresh; each rotation moves the family's expiry to `IdleTTL` from then. Kept with the family when issued, so a change applies to new connections only. Ignored under a finite `RefreshTokenTTL`. `Never` is rejected. |
| `RequestTTL` | 10 minutes | A pending login, from authorize to approval. |
| `CodeTTL` | 1 minute | An authorization code until it is exchanged. |
| `RegisteredClientTTL` | 365 days | Renewed on each authorize and on each code or refresh exchange, before its tokens are written, so a client that keeps refreshing never expires, under `RefreshTokenTTL` `Never` included. Only unused clients expire. |
| `MetadataCacheTTL` | 1h | Cached Client ID Metadata Documents, regardless of `Cache-Control`. |
| `RefreshTimeout` | 10s | The Store calls of a refresh, which run detached from the request: the reads, the client renewal, the token writes and the family swap. |
| `MaxLoginAttempts` | 5 | Failed `Challenge` or `Verify` calls that void a pending request on the built-in page. |

### Upgrading an existing OAuth deployment

Tokens issued before this version were stored in plain text. They cannot be found any more.
Connected MCP clients must reconnect once after the upgrade. There is no migration.

## MCP

`mcp.Config.Auth` defaults to `mcp.BearerPassthrough()`. It forwards the MCP client's token to the
REST API unchanged as `Authorization: Bearer <token>`. An empty token yields no headers.
`BearerPassthrough` does not check the token. The REST API does.

The MCP server parses the header with `requests.ParseBearer`. A malformed `Authorization` header
counts as no token.

`mcp.BearerToHeaders(keyHeader, secretHeader)` remains for REST APIs still on legacy headers. It
splits a `key:secret` bearer at the first `:` into two headers:

```go
server, err := mcp.New(mcp.Config{
	Spec:    spec,
	Handler: e,
	Auth:    mcp.BearerToHeaders("API-KEY", "API-SECRET"),
})
```

With `run_script` enabled, the `WhoAmI` call keeps anonymous callers out, because neither
`BearerPassthrough` nor `BearerToHeaders` checks the token. Name a function that fails without
valid credentials.

`mcp.Server.Call` takes the REST headers directly and never goes through `Auth`: a host that embeds its
own agent passes the headers of the account the agent acts for, for example `Authorization: Bearer
<token>`, and the REST API authenticates the call as it would any request. The headers are set after
the arguments, so an argument never overrides them.

## Generator

Declare the bearer scheme in the generator config:

```go
cfg.Security = []generator.SecurityScheme{
	{
		Name:         "BearerAuth",
		Type:         "http",
		Scheme:       "bearer",
		BearerFormat: "opaque",
		Description:  "An API key (acme_...), an OAuth access token (acmeat_...) or a session token (acmes_...).",
	},
}
```

It is emitted as:

```json
"BearerAuth": {"type": "http", "scheme": "bearer", "bearerFormat": "opaque", "description": "..."}
```

- `Type` is `"apiKey"` (the default when empty, header named by `Header`) or `"http"`.
- `Scheme` is required when `Type` is `"http"`. `BearerFormat` and `Description` are optional.
- Any other `Type` is `ez.EINVALID`.

The global `security` requirement puts every `apiKey` scheme in one requirement object, so they
must be sent together (the `API-KEY` and `API-SECRET` pair), and gives each `http` scheme its own
requirement object after it, so a bearer token is an alternative credential. During the migration
an app that lists `ApiKeyAuth`, `ApiSecretAuth` and `BearerAuth` publishes
`security: [{ApiKeyAuth: [], ApiSecretAuth: []}, {BearerAuth: []}]`. The reference page's curl
examples use the first requirement object, so they keep showing the key pair until it is removed.

## Migrating from API-KEY and API-SECRET

1. **Bump compose.**
2. **Keep legacy clients working for one release.** `Authenticate` never reads the pair. Where
   the application still accepts it, fall through to `LegacyCredentials` on
   `ez.ENOTAUTHENTICATED` only:

   ```go
   authReq, err := req.Authenticate()
   if ez.ErrorCode(err) == ez.ENOTAUTHENTICATED {
       key, secret, err := req.LegacyCredentials("API-KEY", "API-SECRET")
       if err != nil {
           return ez.Wrap(err)
       }
       // Legacy client: look the pair up as before and return from here.
       return a.resolveLegacy(ctx, key, secret)
   }
   if err != nil {
       return ez.Wrap(err)
   }
   // Bearer or cookie client: resolve authReq.Token.
   ```

   Falling through on that code alone keeps the rule that a malformed `Authorization` header,
   or a cookie that fails the CSRF rule, is refused rather than retried with the pair.
3. **Update error handling.** Missing credentials are now `ez.ENOTAUTHENTICATED` (401), not
   `ez.EINVALID` (400). A malformed `Authorization` header is `ez.EINVALID`. Only one of the two
   legacy headers is still `ez.EINVALID` from `LegacyCredentials`. Tests that expect 400 for a
   request without credentials must expect 401.
4. **Pass `requests.WithMethod`** to `requests.New` in every handler that may accept a cookie.
5. **Stop reading `API-KEY` directly.** A handler that takes the requester id or a rate-limit
   key from the raw header bypasses both extractors and breaks for bearer clients. Derive it from
   the credential the application resolved.
6. **Switch clients to `Authorization: Bearer <token>`.** Issue API keys with
   `token.Generate(prefix)` and resolve them by hash, as in [the example](#example). Switch the
   generator config to the [http bearer scheme](#generator).
7. **OAuth.** Set `oauth.Config.TokenPrefix`. Return `oauth.Grant{Subject: ...}` from
   `Login.Verify` or `Approve`; `Grant.Headers` is gone. Change `Fallback` to
   `func(ctx context.Context, token string) (bool, error)`. Resolve `<prefix>at_` tokens on the REST side
   with `Server.Verify`. Connected MCP clients reconnect once. A hand-configured MCP client that
   sent `key:secret` as its bearer through the old `Fallback` now reaches the REST API as
   `Authorization: Bearer key:secret`, which `Authenticate` takes as the token and the prefix
   switch rejects. Give those clients an API key issued with `token.Generate(prefix)`; only the
   token value in their configuration changes.
8. **MCP without OAuth.** If the REST API still accepts only legacy headers while MCP is
   upgraded, set `Auth: mcp.BearerToHeaders("API-KEY", "API-SECRET")` explicitly. The default is
   now `BearerPassthrough()`.
9. **Next release.** Delete the `LegacyCredentials` fallthrough.
