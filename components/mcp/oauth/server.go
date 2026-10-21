package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
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

	return &Server{
		cfg:          cfg,
		resourcePath: resourcePath,
		issuerPath:   issuerPath,
		client:       &client,
	}, nil
}

// baseURLPath checks that raw is an absolute https URL (http is allowed for
// localhost) without query, fragment or trailing slash, and returns its path.
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
	return u.Path, nil
}

// isLocalHost reports whether host is a loopback name allowed over http.
func isLocalHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Mount registers every endpoint on e and serves mcpHandler at the path of
// Resource behind Protect.
func (s *Server) Mount(e *echo.Echo, mcpHandler http.Handler) {
	for _, r := range s.routes() {
		e.Add(r.method, r.path, echo.WrapHandler(r.handler))
	}
	mcpPath := s.resourcePath
	if mcpPath == "" {
		mcpPath = "/"
	}
	e.Any(mcpPath, echo.WrapHandler(s.Protect(mcpHandler)))
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

	metadataPaths := map[string]http.HandlerFunc{
		s.issuerPath + "/.well-known/oauth-authorization-server": asMetadata,
		"/.well-known/oauth-authorization-server" + s.issuerPath: asMetadata,
		"/.well-known/oauth-protected-resource" + s.resourcePath: resourceMetadata,
		"/.well-known/oauth-protected-resource":                  resourceMetadata,
	}
	var routes []route
	for path, handler := range metadataPaths {
		routes = append(routes, route{http.MethodGet, path, handler}, route{http.MethodOptions, path, handler})
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
		s.cfg.OnError(ctx, err)
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
