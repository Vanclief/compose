package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/vanclief/ez"
)

const (
	defaultAccessTokenTTL  = time.Hour
	defaultRefreshTokenTTL = 30 * 24 * time.Hour
	defaultHTTPTimeout     = 10 * time.Second
	// requestTTL bounds a pending login, codeTTL an unexchanged code.
	requestTTL = 10 * time.Minute
	codeTTL    = time.Minute
	// metadataMaxAge is how long clients may cache the metadata documents.
	metadataMaxAge = "public, max-age=3600"
)

// route is one endpoint of the authorization server.
type route struct {
	method  string
	path    string
	handler http.HandlerFunc
}

// New validates cfg, applies defaults and returns the server.
func New(cfg Config) (*Server, error) {
	issuerPath, err := baseURLPath("Issuer", cfg.Issuer)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	resourcePath, err := baseURLPath("Resource", cfg.Resource)
	if err != nil {
		return nil, ez.Wrap(err)
	}
	if cfg.Store == nil {
		return nil, ez.New(ez.EINVALID, "oauth: Config.Store is required", nil)
	}
	if (cfg.Login.Verify == nil) == (cfg.LoginURL == "") {
		return nil, ez.New(ez.EINVALID, "oauth: set exactly one of Config.Login.Verify and Config.LoginURL", nil)
	}
	if cfg.LoginURL != "" {
		_, err = url.Parse(cfg.LoginURL)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "oauth: Config.LoginURL is not a URL", err)
		}
	}
	for _, client := range cfg.Clients {
		err = checkClient(client)
		if err != nil {
			return nil, ez.Wrap(err)
		}
	}

	if cfg.AccessTokenTTL <= 0 {
		cfg.AccessTokenTTL = defaultAccessTokenTTL
	}
	if cfg.RefreshTokenTTL <= 0 {
		cfg.RefreshTokenTTL = defaultRefreshTokenTTL
	}
	if cfg.Login.IdentifierLabel == "" {
		cfg.Login.IdentifierLabel = "Email"
	}
	if cfg.Login.CredentialLabel == "" {
		cfg.Login.CredentialLabel = "Password"
		if cfg.Login.Challenge != nil {
			cfg.Login.CredentialLabel = "Code"
		}
	}
	// The default client checks every address it connects to, after DNS
	// resolution, and uses no proxy, so a metadata document URL cannot reach
	// the internal network. A caller that supplies HTTPClient owns where it may connect
	// (tests serve documents from 127.0.0.1).
	if cfg.HTTPClient == nil {
		dialer := &net.Dialer{Timeout: defaultHTTPTimeout, Control: publicAddressOnly}
		cfg.HTTPClient = &http.Client{
			Timeout:   defaultHTTPTimeout,
			Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: defaultHTTPTimeout},
		}
	}
	// Metadata documents are fetched without following redirects, so a
	// document cannot bounce the fetch elsewhere.
	client := *cfg.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	s := &Server{
		cfg:          cfg,
		resourcePath: resourcePath,
		issuerPath:   issuerPath,
		client:       &client,
	}
	err = s.checkRoutes()
	if err != nil {
		return nil, ez.Wrap(err)
	}
	return s, nil
}

// checkRoutes rejects an Issuer and Resource whose paths put two endpoints,
// or an endpoint and the MCP endpoint of Mount, at the same method and path:
// Handler would panic and Mount would let one replace the other.
func (s *Server) checkRoutes() error {
	seen := map[string]bool{}
	for _, r := range s.routes() {
		key := r.method + " " + r.path
		if seen[key] || r.path == s.mcpPath() {
			return ez.New(ez.EINVALID, "oauth: Config.Issuer and Config.Resource put two endpoints at "+r.path+", change one of them", nil)
		}
		seen[key] = true
	}
	return nil
}

// baseURLPath checks that raw is a URL whose path the routers can serve, and
// returns that path decoded. raw must be absolute, without user info, use
// https (http is allowed for localhost), have no query or fragment, not end
// with a slash, decode to a path without { } % : or *, percent-encode its path
// canonically and have a clean path.
func baseURLPath(name, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ez.New(ez.EINVALID, "oauth: Config."+name+" is not a URL", err)
	}
	if u.Host == "" || u.User != nil {
		return "", ez.New(ez.EINVALID, "oauth: Config."+name+" must be an absolute URL such as https://api.acme.com", nil)
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLocalHost(u.Hostname())) {
		return "", ez.New(ez.EINVALID, "oauth: Config."+name+" must use https", nil)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", ez.New(ez.EINVALID, "oauth: Config."+name+" must not have a query or fragment", nil)
	}
	if strings.HasSuffix(raw, "/") {
		return "", ez.New(ez.EINVALID, "oauth: Config."+name+" must not end with a slash", nil)
	}
	// Handler and Mount use the path in route patterns, where these
	// characters are wildcard or parameter syntax, or are unescaped again.
	if strings.ContainsAny(u.Path, "{}%:*") {
		return "", ez.New(ez.EINVALID, "oauth: Config."+name+" path cannot contain { } % : or *, which the routers treat as syntax", nil)
	}
	// Routes use the decoded path and the challenge the escaped one, so they
	// only agree when the path is escaped the canonical way.
	if u.RawPath != "" {
		return "", ez.New(ez.EINVALID, "oauth: Config."+name+" must percent-encode its path canonically, as (&url.URL{Path: p}).EscapedPath() writes it for the decoded path p, with uppercase hex and no needless escapes, such as /m%C3%A9. Go also escapes ! ' ( ) [ ], so write them as %21 %27 %28 %29 %5B %5D", nil)
	}
	// Handler panics on a route pattern with an unclean path.
	if u.Path != "" && path.Clean(u.Path) != u.Path {
		return "", ez.New(ez.EINVALID, "oauth: Config."+name+" must have a clean path, without . or .. segments or repeated slashes", nil)
	}
	return u.Path, nil
}

// isLocalHost reports whether host is a loopback name allowed over http.
func isLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Mount registers every endpoint on e and serves mcpHandler at the path of
// Resource behind Protect. Echo matches the raw request path, so clients must
// request the published URLs byte for byte: a re-encoded form such as %c3%a9
// for %C3%A9 is not found.
func (s *Server) Mount(e *echo.Echo, mcpHandler http.Handler) {
	for _, r := range s.routes() {
		e.Add(r.method, r.path, echo.WrapHandler(r.handler))
	}
	e.Any(s.mcpPath(), echo.WrapHandler(s.Protect(mcpHandler)))
}

// mcpPath is the path Mount serves the MCP endpoint at.
func (s *Server) mcpPath() string {
	if s.resourcePath == "" {
		return "/"
	}
	return s.resourcePath
}

// Handler serves the authorization server endpoints (metadata, register,
// authorize, login, token) for installations that do not use Echo. Paths are
// matched as in Mount.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.routes() {
		mux.HandleFunc(r.method+" "+r.path, r.handler)
	}
	return mux
}

// routes lists the endpoints with their full paths.
func (s *Server) routes() []route {
	asMetadata := cors(s.serveAuthServerMetadata)
	resourceMetadata := cors(s.serveResourceMetadata)
	register := cors(s.serveRegister)
	token := cors(s.serveToken)

	var routes []route
	metadata := func(p string, handler http.HandlerFunc) {
		routes = append(routes, route{http.MethodGet, p, handler}, route{http.MethodOptions, p, handler})
	}
	// Each document is served at two paths, which are one when the path of
	// Issuer or Resource is empty. Other repeats are left for checkRoutes.
	metadata(s.issuerPath+"/.well-known/oauth-authorization-server", asMetadata)
	if s.issuerPath != "" {
		metadata("/.well-known/oauth-authorization-server"+s.issuerPath, asMetadata)
	}
	metadata("/.well-known/oauth-protected-resource", resourceMetadata)
	if s.resourcePath != "" {
		metadata("/.well-known/oauth-protected-resource"+s.resourcePath, resourceMetadata)
	}
	routes = append(routes,
		route{http.MethodPost, s.issuerPath + "/oauth/register", register},
		route{http.MethodOptions, s.issuerPath + "/oauth/register", register},
		route{http.MethodPost, s.issuerPath + "/oauth/token", token},
		route{http.MethodOptions, s.issuerPath + "/oauth/token", token},
		route{http.MethodGet, s.issuerPath + "/oauth/authorize", s.serveAuthorize},
	)
	if s.cfg.LoginURL == "" {
		routes = append(routes,
			route{http.MethodGet, s.issuerPath + "/oauth/login", s.serveLoginPage},
			route{http.MethodPost, s.issuerPath + "/oauth/login", s.serveLoginSubmit},
		)
	}
	return routes
}

// cors lets browser-based clients call the endpoint from any origin and
// answers the preflight. None of these endpoints use cookies.
func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// authServerMetadata is the RFC 8414 document. oauthex.AuthServerMeta
// always encodes jwks_uri, which this server does not have, so the document
// is its own struct with the same field names.
type authServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported"`
}

func (s *Server) serveAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", metadataMaxAge)
	writeJSON(w, http.StatusOK, authServerMetadata{
		Issuer:                            s.cfg.Issuer,
		AuthorizationEndpoint:             s.cfg.Issuer + "/oauth/authorize",
		TokenEndpoint:                     s.cfg.Issuer + "/oauth/token",
		RegistrationEndpoint:              s.cfg.Issuer + "/oauth/register",
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		TokenEndpointAuthMethodsSupported: []string{"none", "client_secret_basic", "client_secret_post"},
		ClientIDMetadataDocumentSupported: true,
	})
}

func (s *Server) serveResourceMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", metadataMaxAge)
	writeJSON(w, http.StatusOK, oauthex.ProtectedResourceMetadata{
		Resource:               s.cfg.Resource,
		AuthorizationServers:   []string{s.cfg.Issuer},
		BearerMethodsSupported: []string{"header"},
	})
}

// resourceMetadataURL is the RFC 9728 metadata URL of the MCP endpoint: the
// well-known path inserted between the origin and the path of Resource, kept
// percent-encoded as Resource has it.
func (s *Server) resourceMetadataURL() string {
	u, err := url.Parse(s.cfg.Resource)
	if err != nil {
		// Unreachable: New has parsed Resource.
		return ""
	}
	return u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource" + u.EscapedPath()
}

// writeJSON encodes v as the response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "oauth: response encoding failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

// randomID returns 32 random bytes, base64url encoded without padding.
func randomID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// putRecord stores v as JSON under kind and id until expiresAt.
func (s *Server) putRecord(ctx context.Context, kind, id string, v any, expiresAt time.Time) error {
	data, err := json.Marshal(v)
	if err != nil {
		return ez.New(ez.EINTERNAL, "oauth: encoding a "+kind+" record failed", err)
	}
	err = s.cfg.Store.Put(ctx, kind, id, data, expiresAt)
	if err != nil {
		// Whatever code the Store uses, a failing Store is an internal error
		// here, distinct from the codes the handlers show to users.
		return ez.New(ez.EINTERNAL, "oauth: saving the "+kind+" record failed", err)
	}
	return nil
}

// report hands err to Config.OnError when it is set.
func (s *Server) report(ctx context.Context, err error) {
	if s.cfg.OnError != nil {
		// Detached: the failure may be that ctx ran out, and OnError still needs a live context.
		s.cfg.OnError(context.WithoutCancel(ctx), err)
	}
}

// serverError reports err and answers an OAuth server_error carrying
// description.
func (s *Server) serverError(ctx context.Context, w http.ResponseWriter, err error, description string) {
	s.report(ctx, err)
	writeOAuthError(w, http.StatusInternalServerError, "server_error", description)
}

// getRecord loads the record under kind and id into v. With take set the
// record is deleted in the same step. Absent records yield ez.ENOTFOUND, a
// failing Store ez.EINTERNAL.
func (s *Server) getRecord(ctx context.Context, kind, id string, take bool, v any) error {
	if id == "" {
		return ez.New(ez.ENOTFOUND, "oauth: no "+kind+" id", nil)
	}
	var data []byte
	var err error
	if take {
		data, err = s.cfg.Store.Take(ctx, kind, id)
	} else {
		data, err = s.cfg.Store.Get(ctx, kind, id)
	}
	if err != nil && ez.ErrorCode(err) != ez.ENOTFOUND {
		// Whatever code the Store uses, a failing Store is an internal error
		// here, distinct from the codes the handlers show to users.
		return ez.New(ez.EINTERNAL, "oauth: reading the "+kind+" record failed", err)
	}
	if err != nil {
		return ez.Wrap(err)
	}
	err = json.Unmarshal(data, v)
	if err != nil {
		return ez.New(ez.EINTERNAL, "oauth: decoding a "+kind+" record failed", err)
	}
	return nil
}
