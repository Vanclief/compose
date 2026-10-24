// Package oauth lets hosted MCP clients such as claude.ai and ChatGPT connect
// to a compose MCP server. Those clients cannot be configured with an API
// key: they discover an OAuth 2.1 authorization server, send the user to log
// in, and then call the MCP endpoint with the access token they received.
//
// The package hosts that authorization server and protects the MCP endpoint
// as the matching resource server. The application keeps its users and its
// REST credentials; the package keeps the protocol:
//
//   - discovery: RFC 8414 authorization server metadata and RFC 9728
//     protected resource metadata, both under /.well-known;
//   - client registration: Client ID Metadata Documents (an https URL as
//     client_id), RFC 7591 dynamic registration, and clients listed in Config;
//   - the authorization code flow with PKCE (S256 only), the RFC 8707 resource
//     parameter, exact redirect URI matching, and single-use codes;
//   - opaque access and refresh tokens kept in the Store, refresh rotation;
//   - the bearer check in front of the MCP endpoint, answering 401 with the
//     WWW-Authenticate challenge that starts discovery.
//
// Every endpoint lives under Issuer:
//
//	/.well-known/oauth-authorization-server   metadata
//	/.well-known/oauth-protected-resource/mcp metadata of the MCP endpoint (path mirrors Resource)
//	/oauth/register                            dynamic client registration
//	/oauth/authorize                           start of the code flow
//	/oauth/login                               built-in login page (unless LoginURL is set)
//	/oauth/token                               code exchange and refresh
//
// Mounting next to the MCP server on an Echo app:
//
//	auth, err := oauth.New(oauth.Config{
//		Issuer:      "https://api.acme.com",
//		Resource:    "https://api.acme.com/mcp",
//		TokenPrefix: "acme",
//		Store:       oauth.NewBunStore(db.DB, "oauth_records"),
//		Login: oauth.Login{
//			Title:           "Acme",
//			IdentifierLabel: "Email or phone",
//			Challenge:       app.SendLoginCode,
//			// Verify returns oauth.Grant{Subject: account.ID}.
//			Verify: app.VerifyLoginCode,
//		},
//		// Accepts the application's own API keys at the MCP endpoint too.
//		Fallback: func(ctx context.Context, bearer string) (bool, error) {
//			return app.IsAPIKey(ctx, bearer)
//		},
//	})
//	if err != nil {
//		return ez.Wrap(err)
//	}
//	server, err := mcp.New(mcp.Config{Spec: spec, Handler: e, Auth: auth.AuthFunc()})
//	if err != nil {
//		return ez.Wrap(err)
//	}
//	auth.Mount(e, server.Handler())
//
// Verify returns the Grant for the user: a Subject such as the account id.
// The MCP server forwards each call to the REST API with the header
// "Authorization: Bearer <access token>" unchanged, and the REST handler
// resolves the token with Server.Verify, so no REST credential is ever
// stored. Applications with their own login page set LoginURL instead of
// Login, show the client name and redirect host that Pending returns, and
// call Approve once the user has authenticated and confirmed.
//
// Access tokens look like "<TokenPrefix>at_<43 base62>" and refresh tokens
// "<TokenPrefix>rt_<43 base62>" (see components/auth/token). The Store keeps
// only the hex SHA-256 of tokens and authorization codes as record ids, so a
// leaked table holds no usable credential. Tokens issued before this scheme
// were stored in plain text and cannot be found any more: connected MCP
// clients must reconnect once after the upgrade. There is no migration.
//
// Refresh tokens rotate. Each code exchange starts a token family, whose row
// in the Store is the single source of truth: an access or refresh token
// points at its family and the generation it was issued in, and is live only
// while the family row is. The family names its current refresh token, and
// a refresh row it does not name never rotates; an access row of a later
// generation than the family's is refused too. A rotation writes the new
// tokens first, where they are inert, and commits with one conditional Swap
// of the family row that moves it to the next generation and names the new
// refresh token. The rows of a rotation that fails before its Swap stay
// behind: the refresh row never rotates, and the access row, never sent to
// a client, is accepted once a retry commits its generation. A rotated
// refresh token presented again, after its rotation or at the same time as
// it, is reuse: the family row is deleted, and every token of the family is
// refused on its next check. When both a thief and the client use a refresh
// token, the second use ends the connection for both and the user logs in
// again.
//
// Known limits: cached Client ID Metadata Documents are kept for
// MetadataCacheTTL regardless of their Cache-Control; registration is open
// and not throttled; the MCP endpoint sends no CORS headers; a tool call
// checks its token three times, in Protect, in AuthFunc and in the
// application's Verify, two Store reads each, which is accepted as defense in
// depth; access tokens issued before a rotation stay valid until they expire.
//
// A refresh token presented twice revokes its family even when both requests
// come from the client, so a client that retries a refresh with the same
// token after the first attempt committed must authorize again. A client
// that loses the response of a refresh that committed, by disconnecting, is
// in the same place: its retry is reuse and the user logs in again. That is
// preferred to a reuse grace period, because a rotated token stays dead. A
// refresh that fails before the Swap leaves the presented token usable, so
// its retry rotates it, and a Swap that committed but ran out of time
// before returning is recognised by the family naming the new token, and
// answered as a success.
package oauth

import (
	"context"
	"net/http"
	"time"
)

// Never disables an expiry. A zero duration in Config selects the documented
// default; Never is accepted only by the fields that document it, and New
// rejects it, like any other negative duration, everywhere else.
const Never = time.Duration(-1)

// Config configures a server. Issuer, Resource, TokenPrefix, Store and one of
// Login or LoginURL are required.
type Config struct {
	// Issuer is the https base URL of this server, without a trailing slash,
	// for example "https://api.acme.com". All endpoints are served under it
	// and it is the issuer reported in the metadata.
	Issuer string
	// Resource is the canonical URL of the MCP endpoint, for example
	// "https://api.acme.com/mcp". Tokens are issued for it alone, and the
	// resource parameter of the code flow must match it.
	Resource string
	// TokenPrefix names the tokens of this server, for example "acme": access
	// tokens are "acmeat_...", refresh tokens "acmert_...". It must match
	// ^[a-z0-9]{2,10}$ so both prefixes fit the 12 characters
	// components/auth/token allows. Pick one that no other token of the
	// application starts with, so AuthFunc, Protect and Verify tell OAuth
	// tokens apart by their shape alone.
	TokenPrefix string
	// Store keeps clients, pending logins, codes and tokens.
	Store Store
	// Login configures the built-in login page.
	Login Login
	// LoginURL, when set, replaces the built-in page: the browser is sent
	// there with the pending request id in the "request" query parameter.
	// The page must show the ClientName and RedirectHost that Pending
	// returns and get an explicit click from the user before the application
	// calls Approve, so a user is never signed in to a client they did not
	// see.
	LoginURL string
	// Clients are pre-registered clients, for installations that hand out a
	// client id instead of accepting registration from any client.
	Clients []Client
	// AllowedRedirectHosts, when set, restricts dynamic registration and
	// Client ID Metadata Documents to clients whose every redirect URI host
	// (exact match, without port) is listed, for example claude.ai and
	// chatgpt.com, and refuses authorization requests from previously
	// stored clients whose redirect URI is not; empty accepts any host.
	// Config.Clients are not restricted.
	AllowedRedirectHosts []string
	// Fallback, when set, decides whether a bearer token that is not shaped
	// like an access token of this server may reach the MCP endpoint, for
	// example an API key of the application, so clients configured by hand
	// keep working next to OAuth clients. It returns true to accept the
	// token and false to refuse it; an error means the token could not be
	// checked, such as a Store outage, and is reported to OnError: Protect
	// answers 500 and AuthFunc returns the error. AuthFunc passes an
	// accepted token to the REST API unchanged, which authenticates it
	// again: Fallback only answers at the MCP edge. It must refuse tokens
	// that are not its own: any token it accepted passes Protect, and a
	// client holding a bad token would never get the 401 that starts
	// discovery. Tokens shaped like access tokens never reach it.
	Fallback func(ctx context.Context, token string) (bool, error)
	// AccessTokenTTL bounds access tokens. Defaults to 1h.
	AccessTokenTTL time.Duration
	// RefreshTokenTTL bounds refresh tokens and so the whole connection,
	// from the code exchange on: a rotation keeps the expiry of its family.
	// Defaults to 30 days. Never puts no fixed end on the connection: it
	// lives until the client goes IdleTTL without refreshing or reuses a
	// rotated token, each rotation moving the family's expiry to IdleTTL
	// from then.
	RefreshTokenTTL time.Duration
	// IdleTTL is how long a connection under RefreshTokenTTL Never lives
	// after its last refresh. It is kept with the family when issued, so a
	// later change applies to new connections only. Ignored under a finite
	// RefreshTokenTTL, which bounds the connection by itself. Defaults to
	// 30 days. Never is rejected.
	IdleTTL time.Duration
	// RequestTTL bounds a pending login, from authorize to approval.
	// Defaults to 10 minutes.
	RequestTTL time.Duration
	// CodeTTL bounds an authorization code until it is exchanged. Defaults
	// to 1 minute.
	CodeTTL time.Duration
	// RegisteredClientTTL keeps a dynamically registered client. Each
	// authorize, and each code or refresh exchange before its tokens are
	// written, stores the client again with a fresh expiry, so only clients
	// unused for this long expire, even under a longer RefreshTokenTTL.
	// Defaults to 365 days.
	RegisteredClientTTL time.Duration
	// MetadataCacheTTL is how long a fetched Client ID Metadata Document is
	// cached, regardless of its Cache-Control. Defaults to 1h.
	MetadataCacheTTL time.Duration
	// RefreshTimeout bounds the Store calls of a refresh, which run detached
	// from the request so a client that disconnects cannot stop them halfway:
	// reading the token and its family, renewing the client, writing the new
	// tokens, the Swap or the revocation. Defaults to 10s.
	RefreshTimeout time.Duration
	// MaxLoginAttempts is how many failed Challenge or Verify calls void a
	// pending request on the built-in login page. Defaults to 5.
	MaxLoginAttempts int
	// OnError, when set, receives the failures users see only as a generic
	// page or a server_error response: Store failures, Challenge and Verify
	// errors that are not shown, and the like. Optional. The context carries
	// the request's values but is never cancelled and has no deadline, and
	// OnError runs synchronously inside the handler, so it must bound its own
	// work.
	OnError func(ctx context.Context, err error)
	// HTTPClient fetches Client ID Metadata Documents. Defaults to a client
	// with a 10s timeout and no proxy that checks every resolved address and
	// refuses loopback, private, link-local, unspecified, multicast,
	// unique-local, carrier-grade NAT, "this network", IETF protocol
	// assignment, benchmarking, class E, NAT64, Teredo and 6to4 addresses; a
	// client set here is trusted to reach any host. Redirects are never
	// followed.
	HTTPClient *http.Client
}

// Login configures the built-in login page. The page asks for an identifier,
// runs Challenge when it is set, then asks for the credential and calls
// Verify. Every error from Challenge or Verify counts as a failed attempt and
// Config.MaxLoginAttempts of them void the pending request. The limit is per
// pending request: throttling per identifier and per IP belongs to the
// application, inside Challenge and Verify.
type Login struct {
	// Title names the application on the page.
	Title string
	// IdentifierLabel labels the first field. Defaults to "Email".
	IdentifierLabel string
	// CredentialLabel labels the second field. Defaults to "Password", or
	// "Code" when Challenge is set.
	CredentialLabel string
	// Challenge runs after the identifier is submitted and before the
	// credential is asked, typically to send a one-time code. Errors with
	// code ez.EINVALID, ez.ENOTFOUND or ez.ERESOURCEEXHAUSTED show their
	// message; any other error shows a generic text and goes to OnError.
	Challenge func(ctx context.Context, identifier string) error
	// Verify checks the identifier and the credential and returns the grant
	// for the user, with Subject set and Scope as granted, empty for none.
	// Errors with code ez.ENOTAUTHENTICATED, ez.ENOTFOUND or ez.EINVALID
	// show their message; any other error shows a generic text and goes to
	// OnError.
	Verify func(ctx context.Context, identifier, credential string) (Grant, error)
}

// Grant is what a token stands for. It holds no credential: the REST API
// resolves the access token it receives back to the Grant with Verify.
// Subject is required: a grant without it, returned by Login.Verify or
// passed to Approve, fails the login with an internal error (ez.EINTERNAL).
type Grant struct {
	// Subject identifies the user, for example an account id. Required: an
	// empty Subject fails the login with an internal error.
	Subject string `json:"subject"`
	// Scope is the granted scope string, space separated, exactly as the
	// application returned it from Verify or passed it to Approve, empty when
	// it granted none. The scope the client requested is never copied in; it
	// only echoes in the token response.
	Scope string `json:"scope,omitempty"`
}

// Client is a registered OAuth client.
type Client struct {
	// ID is the client_id. For Client ID Metadata Documents it is the https
	// URL of the document.
	ID string `json:"id"`
	// Secret is the client_secret of a confidential client; empty for public
	// clients, which authenticate with PKCE alone.
	Secret string `json:"secret,omitempty"`
	// Name is shown on the login page.
	Name string `json:"name"`
	// RedirectURIs are the exact redirect URIs the client may use.
	RedirectURIs []string `json:"redirect_uris"`
	// configured marks a client from Config.Clients, the only clients that
	// authorize errors are redirected to.
	configured bool
}

// Pending describes a pending authorization request for a custom login
// page: the client asking for access, the host the browser returns to and
// the scope it asked for.
type Pending struct {
	ClientID     string
	ClientName   string
	RedirectHost string
	// Scope is the raw space-separated scope the client requested, so the
	// login page can grant, narrow or refuse it in the Grant.Scope it passes
	// to Approve. It is never granted on its own.
	Scope string
}

// Store persists the server records: clients, pending requests, codes,
// tokens and token families. Implementations must be safe for concurrent use.
// Values are opaque JSON; ids are random strings, client ids, or the hex
// SHA-256 of a token or code.
//
// The refresh protocol needs read-your-writes across every instance of the
// server: each Get must observe every Put, Swap or Delete that returned
// before it started. A single primary database gives that; eventually
// consistent reads and read replicas are not supported, since a stale read
// of a family row could rotate a token of a revoked family or miss a reuse.
type Store interface {
	// Put saves value under kind and id until expiresAt, replacing any
	// previous value.
	Put(ctx context.Context, kind, id string, value []byte, expiresAt time.Time) error
	// Get returns the value, or an error with code ez.ENOTFOUND when it is
	// absent or expired.
	Get(ctx context.Context, kind, id string) ([]byte, error)
	// Take returns the value and deletes it in one step, so a code is
	// redeemed at most once. Absent or expired values yield an error with
	// code ez.ENOTFOUND.
	Take(ctx context.Context, kind, id string) ([]byte, error)
	// Swap replaces the live value under kind and id with value, until
	// expiresAt, only when the stored value equals old, byte for byte: old
	// is always a value Get returned, never re-encoded, so a store that
	// normalises its values must compare the same way. It returns an error
	// with code ez.ENOTFOUND when the record is absent, expired or holds a
	// different value, so the caller re-reads and decides again.
	Swap(ctx context.Context, kind, id string, old, value []byte, expiresAt time.Time) error
	// Delete removes the value. A missing value is not an error.
	Delete(ctx context.Context, kind, id string) error
}

// Record kinds the server stores. Codes, access tokens and refresh tokens are
// stored under the hex SHA-256 of the secret, never the secret itself.
const (
	KindClient  = "client"
	KindRequest = "request"
	KindCode    = "code"
	KindAccess  = "access"
	KindRefresh = "refresh"
	// KindFamily is a token family under its random id: the grant, the
	// client, the expiry and the current generation every access and
	// refresh token of the family is checked against.
	KindFamily = "family"
)

// Server is the authorization server and the guard of the MCP endpoint.
type Server struct {
	cfg Config
	// resourcePath is the path of cfg.Resource, "/mcp" for the example.
	resourcePath string
	// issuerPath is the path of cfg.Issuer, usually empty.
	issuerPath string
	client     *http.Client
}

// authRequest is a pending authorization request, stored from the authorize
// endpoint until Approve redeems it.
type authRequest struct {
	ClientID      string `json:"client_id"`
	ClientName    string `json:"client_name"`
	RedirectURI   string `json:"redirect_uri"`
	State         string `json:"state"`
	Scope         string `json:"scope"`
	CodeChallenge string `json:"code_challenge"`
	// Identifier is the first login field once submitted on the built-in page.
	Identifier string `json:"identifier,omitempty"`
	// Attempts counts failed credential checks on the built-in page.
	Attempts int `json:"attempts,omitempty"`
	// ExpiresAt is set once when the request is created; putting the request
	// back keeps it.
	ExpiresAt time.Time `json:"expires_at"`
}

// codeRecord is an authorization code waiting to be exchanged. The granted
// scope travels in Grant.Scope; RequestedScope is the client's scope
// parameter, kept only for the token response.
type codeRecord struct {
	ClientID       string `json:"client_id"`
	RedirectURI    string `json:"redirect_uri"`
	CodeChallenge  string `json:"code_challenge"`
	Grant          Grant  `json:"grant"`
	RequestedScope string `json:"requested_scope,omitempty"`
}

// familyRecord is a token family: the grant, the client, the expiry that
// bounds the whole connection, and the generation and id of its current
// refresh token. It is the single source of truth: an access or refresh
// token is live only while its family row is, and only the refresh token
// the family names rotates. Revoking a family is deleting this row.
type familyRecord struct {
	ClientID string `json:"client_id"`
	Grant    Grant  `json:"grant"`
	// RequestedScope is the scope the client asked for at authorization,
	// kept for the token response only.
	RequestedScope string    `json:"requested_scope,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
	// Idle is set for a family issued under RefreshTokenTTL Never: each
	// rotation moves ExpiresAt to Idle from then, so the connection ends
	// Idle after its last refresh. Zero for a family with a fixed expiry.
	Idle       time.Duration `json:"idle,omitempty"`
	Generation int64         `json:"generation"`
	// Refresh is the store id of the current refresh token, the only one
	// that rotates. A refresh row the family does not name is a rotated
	// token, or one a failed rotation left behind, and is refused; it keeps
	// the expiry it was issued with, and is detected as reuse until then.
	Refresh string `json:"refresh"`
}

// responseScope is the scope field of the token response (RFC 6749 section
// 5.1): the granted scope when the application set one, else the requested
// scope, which tells the client it got what it asked for. It is never what
// Verify returns.
func (f familyRecord) responseScope() string {
	if f.Grant.Scope != "" {
		return f.Grant.Scope
	}
	return f.RequestedScope
}

// tokenRecord is an access or refresh token: a pointer at its family and
// the generation it was issued in.
type tokenRecord struct {
	Family     string `json:"family"`
	Generation int64  `json:"generation"`
}
