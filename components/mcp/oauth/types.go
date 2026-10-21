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
//		Issuer:   "https://api.acme.com",
//		Resource: "https://api.acme.com/mcp",
//		Store:    oauth.NewBunStore(db.DB, "oauth_records"),
//		Login: oauth.Login{
//			Title:           "Acme",
//			IdentifierLabel: "Email or phone",
//			Challenge:       app.SendLoginCode,
//			Verify:          app.VerifyLoginCode,
//		},
//		Fallback: mcp.BearerToHeaders("API-KEY", "API-SECRET"),
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
// Verify returns the Grant for the user: the REST headers the MCP server
// sends on their behalf, for example API-KEY and API-SECRET. The grant is
// stored with the tokens, so the Store holds credentials and must be treated
// like a session table. Applications with their own login page set LoginURL
// instead of Login, show the client name and redirect host that Pending
// returns, and call Approve once the user has authenticated and confirmed.
//
// Known limits: a reused refresh token is rejected but does not revoke the
// tokens issued from it; cached Client ID Metadata Documents are kept for 1h
// regardless of their Cache-Control; registration is open and not
// throttled; the MCP endpoint sends no CORS headers; Protect and AuthFunc
// each read the Store once per request.
package oauth

import (
	"context"
	"net/http"
	"time"
)

// Config configures a server. Issuer, Resource, Store and one of Login or
// LoginURL are required.
type Config struct {
	// Issuer is the https base URL of this server, without a trailing slash,
	// for example "https://api.acme.com". All endpoints are served under it
	// and it is the issuer reported in the metadata.
	Issuer string
	// Resource is the canonical URL of the MCP endpoint, for example
	// "https://api.acme.com/mcp". Tokens are issued for it alone, and the
	// resource parameter of the code flow must match it.
	Resource string
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
	// Fallback, when set, accepts bearer tokens that are not OAuth tokens,
	// for example the "key:secret" form that mcp.BearerToHeaders maps, so
	// clients configured by hand keep working next to OAuth clients. It must
	// fail for tokens that are not its own: an expired OAuth token it
	// accepted would pass Protect, and the client would never get the 401
	// that makes it refresh.
	Fallback func(ctx context.Context, token string) (http.Header, error)
	// AccessTokenTTL bounds access tokens. Defaults to 1h.
	AccessTokenTTL time.Duration
	// RefreshTokenTTL bounds refresh tokens and so the whole connection.
	// Defaults to 30 days.
	RefreshTokenTTL time.Duration
	// OnError, when set, receives the failures users see only as a generic
	// page or a server_error response: Store failures, Challenge and Verify
	// errors that are not shown, and the like. Optional.
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
// five void the pending request. The limit is per pending request: throttling
// per identifier and per IP belongs to the application, inside Challenge and
// Verify.
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
	// for the user. Errors with code ez.ENOTAUTHENTICATED, ez.ENOTFOUND or
	// ez.EINVALID show their message; any other error shows a generic text
	// and goes to OnError.
	Verify func(ctx context.Context, identifier, credential string) (Grant, error)
}

// Grant is what a token stands for: the REST headers the MCP server sends
// for the user, and a subject for logs.
type Grant struct {
	// Subject identifies the user, for example an account id. It is reported
	// to the MCP server as the token user id and never shown to clients.
	Subject string `json:"subject"`
	// Headers are the REST headers that authenticate the user.
	Headers http.Header `json:"headers"`
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
// page: the client asking for access and the host the browser returns to.
type Pending struct {
	ClientID     string
	ClientName   string
	RedirectHost string
}

// Store persists the server records: clients, pending requests, codes and
// tokens. Implementations must be safe for concurrent use. Values are opaque
// JSON; ids are random strings or client ids.
type Store interface {
	// Put saves value under kind and id until expiresAt, replacing any
	// previous value.
	Put(ctx context.Context, kind, id string, value []byte, expiresAt time.Time) error
	// Get returns the value, or an error with code ez.ENOTFOUND when it is
	// absent or expired.
	Get(ctx context.Context, kind, id string) ([]byte, error)
	// Take returns the value and deletes it in one step, so a code or a
	// refresh token is redeemed at most once. Absent or expired values yield
	// an error with code ez.ENOTFOUND.
	Take(ctx context.Context, kind, id string) ([]byte, error)
	// Delete removes the value. A missing value is not an error.
	Delete(ctx context.Context, kind, id string) error
}

// Record kinds the server stores.
const (
	KindClient  = "client"
	KindRequest = "request"
	KindCode    = "code"
	KindAccess  = "access"
	KindRefresh = "refresh"
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
}

// codeRecord is an authorization code waiting to be exchanged.
type codeRecord struct {
	ClientID      string `json:"client_id"`
	RedirectURI   string `json:"redirect_uri"`
	Scope         string `json:"scope"`
	CodeChallenge string `json:"code_challenge"`
	Grant         Grant  `json:"grant"`
}

// tokenRecord is an access or refresh token.
type tokenRecord struct {
	ClientID  string    `json:"client_id"`
	Scope     string    `json:"scope"`
	Grant     Grant     `json:"grant"`
	ExpiresAt time.Time `json:"expires_at"`
}
