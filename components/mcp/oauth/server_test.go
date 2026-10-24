package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/auth/token"
	"github.com/vanclief/ez"
)

// testRedirect is the redirect URI of the test clients.
const testRedirect = "https://client.example/callback"

// testGrant is what the test login grants.
var testGrant = Grant{Subject: "user-1"}

// issuedGrant is what the server stores for testGrant: the scope of
// authorizeParams is never copied into it.
var issuedGrant = testGrant

// testPrefix is the TokenPrefix of the test servers.
const testPrefix = "test"

// shapedToken returns a token with the shape of an access token of the test
// servers that no server issued.
func shapedToken(t *testing.T) string {
	t.Helper()
	generated, err := token.Generate(testPrefix + "at")
	require.NoError(t, err)
	return generated.Value
}

// shapedRefreshToken returns a token with the shape of a refresh token of the
// test servers that no server issued.
func shapedRefreshToken(t *testing.T) string {
	t.Helper()
	generated, err := token.Generate(testPrefix + "rt")
	require.NoError(t, err)
	return generated.Value
}

// verifyLogin accepts ana@acme.com with "pw"; "boom" fails as a server error.
func verifyLogin(ctx context.Context, identifier, credential string) (Grant, error) {
	if identifier == "boom" {
		return Grant{}, ez.New(ez.EINTERNAL, "database is down", nil)
	}
	if identifier != "ana@acme.com" || credential != "pw" {
		return Grant{}, ez.New(ez.ENOTAUTHENTICATED, "Wrong email or password.", nil)
	}
	return testGrant, nil
}

// testEnv is a server mounted on an Echo app served over HTTP.
type testEnv struct {
	s    *Server
	url  string
	http *http.Client
}

// newEnv builds a server with a public client "public" and a confidential
// client "private" (secret "shh"). mutate adjusts the config; handler, when
// set, builds the MCP handler, else the MCP endpoint reports the token user.
func newEnv(t *testing.T, mutate func(*Config), handler func(*Server) http.Handler) *testEnv {
	t.Helper()
	e := echo.New()
	ts := httptest.NewUnstartedServer(e)
	base := "http://" + ts.Listener.Addr().String()
	cfg := Config{
		Issuer:      base,
		Resource:    base + "/mcp",
		TokenPrefix: testPrefix,
		Store:       NewMemoryStore(),
		Login:       Login{Title: "Acme", Verify: verifyLogin},
		Clients: []Client{
			{ID: "public", Name: "Public App", RedirectURIs: []string{testRedirect}},
			{ID: "private", Name: "Private App", Secret: "shh", RedirectURIs: []string{testRedirect, "https://client.example/other"}},
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	require.NoError(t, err)

	mcpHandler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "user="+auth.TokenInfoFromContext(r.Context()).UserID)
	}))
	if handler != nil {
		mcpHandler = handler(s)
	}
	s.Mount(e, mcpHandler)
	ts.Start()
	t.Cleanup(ts.Close)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	return &testEnv{s: s, url: base, http: client}
}

// response is a finished HTTP exchange.
type response struct {
	status int
	header http.Header
	body   string
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.body), &out), r.body)
	return out
}

func (r response) location(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse(r.header.Get("Location"))
	require.NoError(t, err)
	return u
}

func (env *testEnv) do(t *testing.T, req *http.Request) response {
	t.Helper()
	resp, err := env.http.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return response{status: resp.StatusCode, header: resp.Header, body: string(body)}
}

func (env *testEnv) get(t *testing.T, path string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, env.url+path, nil)
	require.NoError(t, err)
	return env.do(t, req)
}

func (env *testEnv) postForm(t *testing.T, path string, form url.Values) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, env.url+path, strings.NewReader(form.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return env.do(t, req)
}

// bearer calls the MCP endpoint with token.
func (env *testEnv) bearer(t *testing.T, token string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, env.url+"/mcp", nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return env.do(t, req)
}

// pkce returns a verifier and its S256 challenge.
func pkce(seed string) (string, string) {
	verifier := strings.Repeat(seed, 43)[:43]
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorizeParams are the parameters of a valid authorization request.
func authorizeParams(clientID, challenge string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {testRedirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"st-1"},
		"scope":                 {"mcp"},
	}
}

func (env *testEnv) authorize(t *testing.T, params url.Values) response {
	t.Helper()
	return env.get(t, "/oauth/authorize?"+params.Encode())
}

// startLogin runs the authorize step and returns the pending request id.
func (env *testEnv) startLogin(t *testing.T, params url.Values) string {
	t.Helper()
	res := env.authorize(t, params)
	require.Equal(t, http.StatusFound, res.status, res.body)
	loc := res.location(t)
	require.Equal(t, "/oauth/login", loc.Path)
	id := loc.Query().Get("request")
	require.NotEmpty(t, id)
	return id
}

// login submits the login form.
func (env *testEnv) login(t *testing.T, id, identifier, credential string) response {
	t.Helper()
	return env.postForm(t, "/oauth/login", url.Values{"request": {id}, "identifier": {identifier}, "credential": {credential}})
}

// codeFrom returns the code of a redirect to the client, checking the state.
func codeFrom(t *testing.T, res response) string {
	t.Helper()
	require.Contains(t, []int{http.StatusFound, http.StatusSeeOther}, res.status, res.body)
	loc := res.location(t)
	require.Equal(t, testRedirect, loc.Scheme+"://"+loc.Host+loc.Path)
	assert.Equal(t, "st-1", loc.Query().Get("state"))
	code := loc.Query().Get("code")
	require.NotEmpty(t, code)
	return code
}

// codeFor runs authorize and the password login for clientID.
func (env *testEnv) codeFor(t *testing.T, clientID, challenge string) string {
	t.Helper()
	id := env.startLogin(t, authorizeParams(clientID, challenge))
	return codeFrom(t, env.login(t, id, "ana@acme.com", "pw"))
}

// exchange redeems a code for the public client.
func (env *testEnv) exchange(t *testing.T, code, verifier string) response {
	t.Helper()
	return env.postForm(t, "/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {"public"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {testRedirect},
		"resource":      {env.s.cfg.Resource},
	})
}

// tokens runs the whole flow for the public client and returns the access
// and refresh tokens.
func (env *testEnv) tokens(t *testing.T) (string, string) {
	t.Helper()
	verifier, challenge := pkce("a")
	res := env.exchange(t, env.codeFor(t, "public", challenge), verifier)
	require.Equal(t, http.StatusOK, res.status, res.body)
	body := res.json(t)
	return body["access_token"].(string), body["refresh_token"].(string)
}

func oauthErrorCode(t *testing.T, res response) string {
	t.Helper()
	code, _ := res.json(t)["error"].(string)
	return code
}

func TestNewValidation(t *testing.T) {
	valid := func() Config {
		return Config{
			Issuer:      "https://api.acme.com",
			Resource:    "https://api.acme.com/mcp",
			TokenPrefix: "acme",
			Store:       NewMemoryStore(),
			Login:       Login{Verify: verifyLogin},
		}
	}
	cases := map[string]func(*Config){
		"issuer missing":        func(c *Config) { c.Issuer = "" },
		"issuer relative":       func(c *Config) { c.Issuer = "/auth" },
		"issuer http":           func(c *Config) { c.Issuer = "http://api.acme.com" },
		"issuer trailing slash": func(c *Config) { c.Issuer = "https://api.acme.com/" },
		"issuer query":          func(c *Config) { c.Issuer = "https://api.acme.com?x=1" },
		"issuer fragment":       func(c *Config) { c.Issuer = "https://api.acme.com#x" },
		"issuer not a URL":      func(c *Config) { c.Issuer = "https://api.acme.com/%zz" },
		"issuer user info":      func(c *Config) { c.Issuer = "https://me@api.acme.com" },
		"resource http":         func(c *Config) { c.Resource = "http://api.acme.com/mcp" },
		"resource escaped /":    func(c *Config) { c.Resource = "https://api.acme.com/a%2Fmcp" },
		"issuer escaped a":      func(c *Config) { c.Issuer = "https://api.acme.com/%61uth" },
		"issuer dot segment":    func(c *Config) { c.Issuer = "https://api.acme.com/a/../b" },
		"issuer double slash":   func(c *Config) { c.Issuer = "https://api.acme.com/a//b" },
		"resource dot segment":  func(c *Config) { c.Resource = "https://api.acme.com/a/../b" },
		"resource double slash": func(c *Config) { c.Resource = "https://api.acme.com/a//b" },
		"issuer open brace":     func(c *Config) { c.Issuer = "https://api.acme.com/%7Bid" },
		"resource open brace":   func(c *Config) { c.Resource = "https://api.acme.com/%7Bid" },
		"resource wildcard":     func(c *Config) { c.Resource = "https://api.acme.com/%7Bid%7D" },
		"issuer escaped %":      func(c *Config) { c.Issuer = "https://api.acme.com/a%2525b" },
		"resource colon":        func(c *Config) { c.Resource = "https://api.acme.com/:id" },
		"resource star":         func(c *Config) { c.Resource = "https://api.acme.com/a%2A" },
		"resource is issuer authorize": func(c *Config) {
			c.Issuer = "https://api.acme.com/.well-known/oauth-protected-resource"
			c.Resource = "https://api.acme.com/oauth/authorize"
		},
		"resource is issuer register": func(c *Config) {
			c.Issuer = "https://api.acme.com/.well-known/oauth-protected-resource"
			c.Resource = "https://api.acme.com/oauth/register"
		},
		"resource metadata is issuer metadata": func(c *Config) {
			c.Issuer = "https://api.acme.com/.well-known/oauth-protected-resource"
			c.Resource = "https://api.acme.com/.well-known/oauth-authorization-server"
		},
		"resource is token endpoint": func(c *Config) {
			c.Issuer = "https://api.acme.com/x"
			c.Resource = "https://api.acme.com/x/oauth/token"
		},
		"resource is authorize endpoint": func(c *Config) {
			c.Issuer = "https://api.acme.com/x"
			c.Resource = "https://api.acme.com/x/oauth/authorize"
		},
		"resource is login endpoint":    func(c *Config) { c.Resource = "https://api.acme.com/oauth/login" },
		"resource is server metadata":   func(c *Config) { c.Resource = "https://api.acme.com/.well-known/oauth-authorization-server" },
		"resource is resource metadata": func(c *Config) { c.Resource = "https://api.acme.com/.well-known/oauth-protected-resource" },
		"store missing":                 func(c *Config) { c.Store = nil },
		"no login":                      func(c *Config) { c.Login = Login{} },
		"both logins":                   func(c *Config) { c.LoginURL = "https://app.acme.com/login" },
		"login URL invalid": func(c *Config) {
			c.Login = Login{}
			c.LoginURL = "https://app.acme.com/%zz"
		},
		"client without name": func(c *Config) { c.Clients = []Client{{ID: "a", RedirectURIs: []string{testRedirect}}} },
		"client without uris": func(c *Config) { c.Clients = []Client{{ID: "a", Name: "A"}} },
		"client with http uri": func(c *Config) {
			c.Clients = []Client{{ID: "a", Name: "A", RedirectURIs: []string{"http://client.example/cb"}}}
		},
		"client with fragment": func(c *Config) {
			c.Clients = []Client{{ID: "a", Name: "A", RedirectURIs: []string{"https://client.example/cb#x"}}}
		},
		"client with custom uri":  func(c *Config) { c.Clients = []Client{{ID: "a", Name: "A", RedirectURIs: []string{"myapp://cb"}}} },
		"token prefix missing":    func(c *Config) { c.TokenPrefix = "" },
		"token prefix short":      func(c *Config) { c.TokenPrefix = "a" },
		"token prefix uppercase":  func(c *Config) { c.TokenPrefix = "Acme" },
		"token prefix with _":     func(c *Config) { c.TokenPrefix = "ac_me" },
		"token prefix too long":   func(c *Config) { c.TokenPrefix = "abcdefghijk" },
		"access ttl never":        func(c *Config) { c.AccessTokenTTL = Never },
		"access ttl negative":     func(c *Config) { c.AccessTokenTTL = -time.Hour },
		"refresh ttl negative":    func(c *Config) { c.RefreshTokenTTL = -time.Hour },
		"idle ttl never":          func(c *Config) { c.IdleTTL = Never },
		"idle ttl negative":       func(c *Config) { c.IdleTTL = -time.Hour },
		"request ttl never":       func(c *Config) { c.RequestTTL = Never },
		"code ttl never":          func(c *Config) { c.CodeTTL = Never },
		"code ttl negative":       func(c *Config) { c.CodeTTL = -2 },
		"client ttl never":        func(c *Config) { c.RegisteredClientTTL = Never },
		"metadata cache never":    func(c *Config) { c.MetadataCacheTTL = Never },
		"refresh timeout never":   func(c *Config) { c.RefreshTimeout = Never },
		"login attempts negative": func(c *Config) { c.MaxLoginAttempts = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			mutate(&cfg)
			_, err := New(cfg)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		})
	}

	cfg := valid()
	cfg.Resource = "https://api.acme.com/a%2Fmcp"
	_, err := New(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config.Resource", "the error names the field")

	cfg = valid()
	cfg.Issuer = "https://api.acme.com/a//b"
	_, err = New(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config.Issuer", "the error names the field")

	cfg = valid()
	cfg.Resource = "https://api.acme.com/%7Bid%7D"
	_, err = New(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config.Resource", "the error names the field")

	cfg = valid()
	cfg.Issuer = "https://api.acme.com/%7Bid"
	_, err = New(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config.Issuer", "the error names the field")

	cfg = valid()
	cfg.Issuer = "https://api.acme.com/x"
	cfg.Resource = "https://api.acme.com/x/oauth/token"
	_, err = New(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config.Resource", "the error names the field")
	assert.Contains(t, err.Error(), "/x/oauth/token", "the error names the path")

	// The syntax check runs on the decoded path first, so a path with a
	// syntax character gets that error and not a canonical-encoding one.
	for _, escaped := range []string{"/a%3Ab", "/a*", "/a}"} {
		cfg = valid()
		cfg.Resource = "https://api.acme.com" + escaped
		_, err = New(cfg)
		require.Error(t, err, escaped)
		assert.Contains(t, err.Error(), "cannot contain", escaped)
	}

	cfg = valid()
	cfg.Resource = "https://api.acme.com/a!b"
	_, err = New(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "for the decoded path p", "the message says what p is")
	assert.Contains(t, err.Error(), "%21", "the message names the escape Go writes")

	cfg = valid()
	cfg.AccessTokenTTL = Never
	_, err = New(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config.AccessTokenTTL cannot be Never")

	cfg = valid()
	cfg.RefreshTokenTTL = Never
	cfg.IdleTTL = Never
	_, err = New(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Config.IdleTTL cannot be Never")

	cfg = valid()
	cfg.TokenPrefix = "abcdefghij"
	_, err = New(cfg)
	assert.NoError(t, err, "a 10 character prefix leaves room for at and rt")

	cfg = valid()
	cfg.RefreshTokenTTL = Never
	s, err := New(cfg)
	require.NoError(t, err, "refresh tokens may never expire")
	assert.Equal(t, Never, s.cfg.RefreshTokenTTL)

	s, err = New(valid())
	require.NoError(t, err)
	assert.Equal(t, "/mcp", s.resourcePath)
	assert.Equal(t, "", s.issuerPath)
	assert.Equal(t, time.Hour, s.cfg.AccessTokenTTL)
	assert.Equal(t, 30*24*time.Hour, s.cfg.RefreshTokenTTL)
	assert.Equal(t, 30*24*time.Hour, s.cfg.IdleTTL)
	assert.Equal(t, 10*time.Minute, s.cfg.RequestTTL)
	assert.Equal(t, time.Minute, s.cfg.CodeTTL)
	assert.Equal(t, 365*24*time.Hour, s.cfg.RegisteredClientTTL)
	assert.Equal(t, time.Hour, s.cfg.MetadataCacheTTL)
	assert.Equal(t, 10*time.Second, s.cfg.RefreshTimeout)
	assert.Equal(t, 5, s.cfg.MaxLoginAttempts)
	assert.Equal(t, 10*time.Second, s.client.Timeout)
	require.IsType(t, &http.Transport{}, s.client.Transport, "the default client dials through the address guard")
	assert.True(t, s.client.Transport.(*http.Transport).DisableKeepAlives, "the default client keeps no idle connections")
	assert.Equal(t, "Email", s.cfg.Login.IdentifierLabel)
	assert.Equal(t, "Password", s.cfg.Login.CredentialLabel)

	cfg = valid()
	cfg.Issuer = "http://localhost:8080/auth"
	cfg.Resource = "http://127.0.0.1:8080"
	cfg.Login.Challenge = func(context.Context, string) error { return nil }
	cfg.HTTPClient = &http.Client{}
	s, err = New(cfg)
	require.NoError(t, err)
	assert.Equal(t, "/auth", s.issuerPath)
	assert.Equal(t, "", s.resourcePath)
	assert.Equal(t, "Code", s.cfg.Login.CredentialLabel)
	assert.Nil(t, s.client.Transport, "a caller's client is used as is")
	assert.Nil(t, cfg.HTTPClient.CheckRedirect, "the caller's client is not modified")
}

func TestMetadata(t *testing.T) {
	env := newEnv(t, nil, nil)

	res := env.get(t, "/.well-known/oauth-authorization-server")
	require.Equal(t, http.StatusOK, res.status)
	assert.Equal(t, "*", res.header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "public, max-age=3600", res.header.Get("Cache-Control"))
	assert.Equal(t, "application/json", res.header.Get("Content-Type"))
	assert.JSONEq(t, `{
		"issuer": "`+env.url+`",
		"authorization_endpoint": "`+env.url+`/oauth/authorize",
		"token_endpoint": "`+env.url+`/oauth/token",
		"registration_endpoint": "`+env.url+`/oauth/register",
		"response_types_supported": ["code"],
		"grant_types_supported": ["authorization_code", "refresh_token"],
		"code_challenge_methods_supported": ["S256"],
		"token_endpoint_auth_methods_supported": ["none", "client_secret_basic", "client_secret_post"],
		"client_id_metadata_document_supported": true
	}`, res.body)

	want := `{"resource": "` + env.url + `/mcp", "authorization_servers": ["` + env.url + `"], "bearer_methods_supported": ["header"]}`
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		res = env.get(t, path)
		require.Equal(t, http.StatusOK, res.status, path)
		assert.JSONEq(t, want, res.body)
		assert.Equal(t, "*", res.header.Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "public, max-age=3600", res.header.Get("Cache-Control"))
	}

	req, err := http.NewRequest(http.MethodOptions, env.url+"/.well-known/oauth-protected-resource/mcp", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://claude.ai")
	req.Header.Set("Access-Control-Request-Method", "GET")
	res = env.do(t, req)
	assert.Equal(t, http.StatusNoContent, res.status)
	assert.Equal(t, "*", res.header.Get("Access-Control-Allow-Origin"))
	assert.Contains(t, res.header.Get("Access-Control-Allow-Methods"), "GET")
	assert.Equal(t, "Authorization, Content-Type", res.header.Get("Access-Control-Allow-Headers"))
}

func TestMetadataIssuerPath(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.Issuer += "/auth"
	}, nil)
	for _, path := range []string{"/auth/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/auth"} {
		res := env.get(t, path)
		require.Equal(t, http.StatusOK, res.status, path)
		body := res.json(t)
		assert.Equal(t, env.url+"/auth", body["issuer"])
		assert.Equal(t, env.url+"/auth/oauth/token", body["token_endpoint"])
	}
	res := env.get(t, "/.well-known/oauth-protected-resource/mcp")
	assert.Equal(t, []any{env.url + "/auth"}, res.json(t)["authorization_servers"])

	verifier, challenge := pkce("p")
	res = env.get(t, "/auth/oauth/authorize?"+authorizeParams("public", challenge).Encode())
	require.Equal(t, http.StatusFound, res.status, res.body)
	loc := res.location(t)
	assert.Equal(t, "/auth/oauth/login", loc.Path)
	res = env.postForm(t, "/auth/oauth/login", url.Values{"request": {loc.Query().Get("request")}, "identifier": {"ana@acme.com"}, "credential": {"pw"}})
	code := codeFrom(t, res)
	res = env.postForm(t, "/auth/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"public"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {testRedirect}})
	assert.Equal(t, http.StatusOK, res.status, res.body)
}

func TestHandler(t *testing.T) {
	s, err := New(Config{Issuer: "https://api.acme.com", Resource: "https://api.acme.com/mcp", TokenPrefix: "acme", Store: NewMemoryStore(), LoginURL: "https://app.acme.com/login"})
	require.NoError(t, err)
	h := s.Handler()

	for path, status := range map[string]int{
		"/.well-known/oauth-authorization-server":   http.StatusOK,
		"/.well-known/oauth-protected-resource/mcp": http.StatusOK,
		"/.well-known/oauth-protected-resource":     http.StatusOK,
		"/oauth/login":                              http.StatusNotFound,
		"/oauth/authorize":                          http.StatusBadRequest,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, status, rec.Code, path)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader("grant_type=password")))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestHandlerAcceptedPaths: Issuer and Resource paths New accepts make valid
// ServeMux and Echo routes that serve the metadata documents and the token
// endpoint, and on Echo the MCP endpoint answers with a challenge whose URL
// serves the resource metadata.
func TestHandlerAcceptedPaths(t *testing.T) {
	pairs := [][2]string{
		{"", ""},
		{"/mcp", "/mcp"},
		{"/m%20cp", "/m%20cp"},
		{"/m%C3%A9", "/m%C3%A9"},
		{"/a%20/b", "/a%20/b"},
		{"/a%3Fb", "/a%3Fb"},
		{"/a%23b", "/a%23b"},
		{"/v1/mcp", "/v1/mcp"},
		{"/a%21b", "/a%21b"},
		// Close to the collisions TestNewValidation rejects.
		{"/.well-known/oauth-protected-resource", "/mcp"},
		{"/x", "/x/oauth"},
	}
	for _, pair := range pairs {
		issuer := pair[0]
		resource := pair[1]
		t.Run(issuer+" "+resource, func(t *testing.T) {
			s, err := New(Config{Issuer: "https://x" + issuer, Resource: "https://x" + resource, TokenPrefix: "acme", Store: NewMemoryStore(), LoginURL: "https://app.acme.com/login"})
			require.NoError(t, err)
			var mux http.Handler
			require.NotPanics(t, func() {
				mux = s.Handler()
			})
			e := echo.New()
			s.Mount(e, http.NotFoundHandler())

			for name, h := range map[string]http.Handler{"ServeMux": mux, "Echo": e} {
				for _, path := range []string{
					issuer + "/.well-known/oauth-authorization-server",
					"/.well-known/oauth-authorization-server" + issuer,
					"/.well-known/oauth-protected-resource" + resource,
					"/.well-known/oauth-protected-resource",
				} {
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
					assert.Equal(t, http.StatusOK, rec.Code, name+" "+path)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, issuer+"/oauth/token", strings.NewReader("grant_type=password")))
				assert.Equal(t, http.StatusBadRequest, rec.Code, name+" token")
			}

			mcpPath := resource
			if mcpPath == "" {
				mcpPath = "/"
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, mcpPath, nil))
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			metadataURL := "https://x/.well-known/oauth-protected-resource" + resource
			assert.Equal(t, `Bearer resource_metadata="`+metadataURL+`"`, rec.Header().Get("WWW-Authenticate"))
			rec = httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, metadataURL, nil))
			assert.Equal(t, http.StatusOK, rec.Code, metadataURL)
		})
	}
}

func TestAuthorizeErrors(t *testing.T) {
	env := newEnv(t, nil, nil)
	_, challenge := pkce("a")

	params := authorizeParams("nobody", challenge)
	res := env.authorize(t, params)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Unknown application")

	params = authorizeParams("public", challenge)
	params.Set("redirect_uri", "https://evil.example/callback")
	res = env.authorize(t, params)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "redirect URI is not registered")
	assert.Empty(t, res.header.Get("Location"))

	// Without redirect_uri a client with several URIs cannot be served...
	params = authorizeParams("private", challenge)
	params.Del("redirect_uri")
	res = env.authorize(t, params)
	assert.Equal(t, http.StatusBadRequest, res.status)

	// ...and a client with one uses it.
	params = authorizeParams("public", challenge)
	params.Del("redirect_uri")
	res = env.authorize(t, params)
	assert.Equal(t, http.StatusFound, res.status, res.body)

	redirectError := func(params url.Values) url.Values {
		t.Helper()
		res := env.authorize(t, params)
		require.Equal(t, http.StatusFound, res.status, res.body)
		loc := res.location(t)
		require.Equal(t, "client.example", loc.Host)
		return loc.Query()
	}

	params = authorizeParams("public", challenge)
	params.Del("code_challenge")
	q := redirectError(params)
	assert.Equal(t, "invalid_request", q.Get("error"))
	assert.NotEmpty(t, q.Get("error_description"))
	assert.Equal(t, "st-1", q.Get("state"))

	params = authorizeParams("public", challenge)
	params.Del("code_challenge_method")
	params.Del("state")
	q = redirectError(params)
	assert.Equal(t, "invalid_request", q.Get("error"))
	assert.False(t, q.Has("state"))

	params = authorizeParams("public", challenge)
	params.Set("code_challenge_method", "plain")
	assert.Equal(t, "invalid_request", redirectError(params).Get("error"))

	params = authorizeParams("public", challenge)
	params.Set("response_type", "token")
	assert.Equal(t, "unsupported_response_type", redirectError(params).Get("error"))

	params = authorizeParams("public", challenge)
	params.Set("resource", "https://other.example/mcp")
	assert.Equal(t, "invalid_target", redirectError(params).Get("error"))

	params = authorizeParams("public", challenge)
	params.Set("resource", env.url+"/mcp/")
	res = env.authorize(t, params)
	assert.Equal(t, "/oauth/login", res.location(t).Path)

	// A registered client gets the error page, never a redirect: registration
	// is open, so redirecting would make authorize an open redirector.
	registered := env.postJSON(t, "/oauth/register", `{"redirect_uris": ["https://evil.example/landing"]}`).json(t)["client_id"].(string)
	params = authorizeParams(registered, challenge)
	params.Set("redirect_uri", "https://evil.example/landing")
	params.Set("response_type", "token")
	res = env.authorize(t, params)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Empty(t, res.header.Get("Location"))
	assert.Contains(t, res.body, "response_type must be code")
}

func TestSameResource(t *testing.T) {
	s, err := New(Config{Issuer: "https://api.acme.com", Resource: "https://api.acme.com/mcp", TokenPrefix: "acme", Store: NewMemoryStore(), LoginURL: "https://app.acme.com/login"})
	require.NoError(t, err)
	cases := map[string]bool{
		"":                              true,
		"https://api.acme.com/mcp":      true,
		"https://api.acme.com/mcp/":     true,
		"HTTPS://API.Acme.com/mcp":      true,
		"https://api.acme.com/MCP":      false,
		"https://api.acme.com/mcp//":    false,
		"http://api.acme.com/mcp":       false,
		"https://api.acme.com:8443/mcp": false,
		"https://other.example/mcp":     false,
		"https://api.acme.com/mcp?x=1":  false,
		"https://api.acme.com/mcp#x":    false,
		"https://me@api.acme.com/mcp":   false,
		"https://api.acme.com/%zz":      false,
	}
	for resource, want := range cases {
		assert.Equal(t, want, s.sameResource(resource), resource)
	}
}

func TestTokenFlow(t *testing.T) {
	env := newEnv(t, nil, nil)
	verifier, challenge := pkce("a")
	code := env.codeFor(t, "public", challenge)

	res := env.exchange(t, code, verifier)
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.Equal(t, "no-store", res.header.Get("Cache-Control"))
	body := res.json(t)
	assert.Equal(t, "Bearer", body["token_type"])
	assert.Equal(t, float64(3600), body["expires_in"])
	assert.Equal(t, "mcp", body["scope"])
	assert.True(t, token.Check("testat", body["access_token"].(string)), body["access_token"])
	assert.True(t, token.Check("testrt", body["refresh_token"].(string)), body["refresh_token"])

	res = env.bearer(t, body["access_token"].(string))
	assert.Equal(t, http.StatusOK, res.status)
	assert.Equal(t, "user=user-1", res.body)

	// Codes are single use.
	res = env.exchange(t, code, verifier)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))
	assert.Equal(t, "no-store", res.header.Get("Cache-Control"))

	// A wrong verifier burns the code.
	code = env.codeFor(t, "public", challenge)
	res = env.exchange(t, code, "wrong-verifier-wrong-verifier-wrong-verifier")
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))
	res = env.exchange(t, code, verifier)
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))

	// The code is bound to its client and, when sent, its redirect URI.
	code = env.codeFor(t, "public", challenge)
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"private"}, "client_secret": {"shh"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {testRedirect}})
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))
	code = env.codeFor(t, "public", challenge)
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"public"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {"https://client.example/other"}})
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))
}

// TestTokenRedirectURIOptional: a client with one redirect URI may omit it at
// both steps; PKCE binds the code.
func TestTokenRedirectURIOptional(t *testing.T) {
	env := newEnv(t, nil, nil)
	verifier, challenge := pkce("o")
	params := authorizeParams("public", challenge)
	params.Del("redirect_uri")
	code := codeFrom(t, env.login(t, env.startLogin(t, params), "ana@acme.com", "pw"))
	res := env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {"public"}, "code": {code}, "code_verifier": {verifier}})
	assert.Equal(t, http.StatusOK, res.status, res.body)
}

func TestTokenErrors(t *testing.T) {
	env := newEnv(t, nil, nil)
	cases := []struct {
		name   string
		form   url.Values
		status int
		code   string
	}{
		{"no grant type", url.Values{"client_id": {"public"}}, http.StatusBadRequest, "invalid_request"},
		{"password grant", url.Values{"grant_type": {"password"}, "client_id": {"public"}}, http.StatusBadRequest, "unsupported_grant_type"},
		{"other resource", url.Values{"grant_type": {"authorization_code"}, "client_id": {"public"}, "resource": {"https://other.example/mcp"}}, http.StatusBadRequest, "invalid_target"},
		{"no client", url.Values{"grant_type": {"authorization_code"}, "code": {"x"}}, http.StatusUnauthorized, "invalid_client"},
		{"unknown client", url.Values{"grant_type": {"authorization_code"}, "client_id": {"nobody"}}, http.StatusUnauthorized, "invalid_client"},
		{"missing secret", url.Values{"grant_type": {"authorization_code"}, "client_id": {"private"}}, http.StatusUnauthorized, "invalid_client"},
		{"wrong secret", url.Values{"grant_type": {"authorization_code"}, "client_id": {"private"}, "client_secret": {"nope"}}, http.StatusUnauthorized, "invalid_client"},
		{"no code", url.Values{"grant_type": {"authorization_code"}, "client_id": {"public"}, "code_verifier": {"v"}}, http.StatusBadRequest, "invalid_request"},
		{"no verifier", url.Values{"grant_type": {"authorization_code"}, "client_id": {"public"}, "code": {"c"}}, http.StatusBadRequest, "invalid_request"},
		{"unknown code", url.Values{"grant_type": {"authorization_code"}, "client_id": {"public"}, "code": {"c"}, "code_verifier": {"v"}}, http.StatusBadRequest, "invalid_grant"},
		{"no refresh token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}}, http.StatusBadRequest, "invalid_request"},
		{"unknown refresh token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {"r"}}, http.StatusBadRequest, "invalid_grant"},
	}
	for _, c := range cases {
		res := env.postForm(t, "/oauth/token", c.form)
		assert.Equal(t, c.status, res.status, c.name)
		assert.Equal(t, c.code, oauthErrorCode(t, res), c.name)
		assert.Empty(t, res.header.Get("WWW-Authenticate"), c.name)
	}

	req, err := http.NewRequest(http.MethodPost, env.url+"/oauth/token", strings.NewReader("grant_type=refresh_token&refresh_token=r"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("private", "nope")
	res := env.do(t, req)
	assert.Equal(t, http.StatusUnauthorized, res.status)
	assert.Equal(t, "invalid_client", oauthErrorCode(t, res))
	assert.Equal(t, `Basic realm="oauth"`, res.header.Get("WWW-Authenticate"))

	req, err = http.NewRequest(http.MethodPost, env.url+"/oauth/token", strings.NewReader("grant_type=refresh_token&refresh_token=r&client_id=public"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("private", "shh")
	res = env.do(t, req)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Equal(t, "invalid_request", oauthErrorCode(t, res))

	req, err = http.NewRequest(http.MethodPost, env.url+"/oauth/token", strings.NewReader("grant_type=refresh_token&refresh_token=r"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("%zz", "shh")
	res = env.do(t, req)
	assert.Equal(t, "invalid_client", oauthErrorCode(t, res))
}

func TestConfidentialClient(t *testing.T) {
	env := newEnv(t, nil, nil)
	verifier, challenge := pkce("c")

	code := env.codeFor(t, "private", challenge)
	req, err := http.NewRequest(http.MethodPost, env.url+"/oauth/token", strings.NewReader(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {testRedirect},
	}.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth("private", "shh")
	res := env.do(t, req)
	require.Equal(t, http.StatusOK, res.status, res.body)

	code = env.codeFor(t, "private", challenge)
	res = env.postForm(t, "/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "client_id": {"private"}, "client_secret": {"shh"}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {testRedirect},
	})
	require.Equal(t, http.StatusOK, res.status, res.body)
}

func TestRefreshRotation(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t, nil, nil)
	access, refresh := env.tokens(t)
	var first tokenRecord
	require.NoError(t, env.s.getRecord(ctx, KindRefresh, hashID(refresh), false, &first))
	require.NotEmpty(t, first.Family)
	before, _, err := env.s.getFamily(ctx, first.Family)
	require.NoError(t, err)

	refreshWith := func(clientID, token string) response {
		return env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {token}})
	}
	res := refreshWith("public", refresh)
	require.Equal(t, http.StatusOK, res.status, res.body)
	body := res.json(t)
	newAccess := body["access_token"].(string)
	newRefresh := body["refresh_token"].(string)
	assert.NotEqual(t, access, newAccess)
	assert.NotEqual(t, refresh, newRefresh)
	assert.Equal(t, "mcp", body["scope"])

	// The new refresh token stays in the family, which keeps its expiry.
	var second tokenRecord
	require.NoError(t, env.s.getRecord(ctx, KindRefresh, hashID(newRefresh), false, &second))
	assert.Equal(t, first.Family, second.Family, "the family is kept through the rotation")
	after, _, err := env.s.getFamily(ctx, second.Family)
	require.NoError(t, err)
	assert.True(t, before.ExpiresAt.Equal(after.ExpiresAt))
	assert.Equal(t, issuedGrant, after.Grant)
	assert.Equal(t, before.Generation+1, second.Generation)

	assert.Equal(t, http.StatusOK, env.bearer(t, newAccess).status)
	assert.Equal(t, http.StatusOK, env.bearer(t, access).status, "the old access token lives until it expires")

	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"private"}, "client_secret": {"shh"}, "refresh_token": {newRefresh}})
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))
	assert.Equal(t, http.StatusOK, refreshWith("public", newRefresh).status, "a refresh token presented by another client is not revoked")
}

func TestAccessTokenExpires(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.AccessTokenTTL = time.Millisecond
	}, nil)
	access, refresh := env.tokens(t)
	time.Sleep(5 * time.Millisecond)
	assert.Equal(t, http.StatusUnauthorized, env.bearer(t, access).status)

	res := env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {refresh}})
	assert.Equal(t, http.StatusOK, res.status, res.body)
}

func TestLoginURLMode(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.Login = Login{}
		c.LoginURL = "https://app.acme.com/connect?from=mcp"
	}, nil)
	verifier, challenge := pkce("l")

	res := env.authorize(t, authorizeParams("public", challenge))
	require.Equal(t, http.StatusFound, res.status, res.body)
	loc := res.location(t)
	assert.Equal(t, "app.acme.com", loc.Host)
	assert.Equal(t, "/connect", loc.Path)
	assert.Equal(t, "mcp", loc.Query().Get("from"))
	id := loc.Query().Get("request")
	require.NotEmpty(t, id)

	assert.Equal(t, http.StatusNotFound, env.get(t, "/oauth/login?request="+id).status, "the built-in page is not mounted")

	pending, err := env.s.Pending(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, Pending{ClientID: "public", ClientName: "Public App", RedirectHost: "client.example", Scope: "mcp"}, pending)

	_, err = env.s.Approve(context.Background(), id, Grant{})
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err), "a grant without subject is refused")
	res = env.authorize(t, authorizeParams("public", challenge))
	require.Equal(t, http.StatusFound, res.status, res.body)
	id = res.location(t).Query().Get("request")

	redirect, err := env.s.Approve(context.Background(), id, testGrant)
	require.NoError(t, err)
	code := codeFrom(t, response{status: http.StatusFound, header: http.Header{"Location": {redirect}}})
	res = env.exchange(t, code, verifier)
	require.Equal(t, http.StatusOK, res.status, res.body)

	_, err = env.s.Approve(context.Background(), id, testGrant)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	_, err = env.s.Approve(context.Background(), "", testGrant)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	_, err = env.s.Pending(context.Background(), id)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
}

// TestApproveScope: Grant.Scope is what the application set, empty included.
// The token response carries the granted scope, or the requested one when the
// application granted none, and keeps it through a refresh.
func TestApproveScope(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.Login = Login{}
		c.LoginURL = "https://app.acme.com/connect"
	}, nil)
	ctx := context.Background()

	for _, tc := range []struct {
		name         string
		grant        Grant
		wantResponse string
	}{
		{name: "application scope is granted", grant: Grant{Subject: "u", Scope: "read"}, wantResponse: "read"},
		{name: "empty scope grants nothing", grant: Grant{Subject: "u"}, wantResponse: "write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier, challenge := pkce("s")
			params := authorizeParams("public", challenge)
			params.Set("scope", "write")
			res := env.authorize(t, params)
			require.Equal(t, http.StatusFound, res.status, res.body)
			id := res.location(t).Query().Get("request")
			require.NotEmpty(t, id)

			pending, err := env.s.Pending(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, "write", pending.Scope)

			redirect, err := env.s.Approve(ctx, id, tc.grant)
			require.NoError(t, err)
			code := codeFrom(t, response{status: http.StatusFound, header: http.Header{"Location": {redirect}}})
			res = env.exchange(t, code, verifier)
			require.Equal(t, http.StatusOK, res.status, res.body)
			body := res.json(t)
			assert.Equal(t, tc.wantResponse, body["scope"])

			grant, err := env.s.Verify(ctx, body["access_token"].(string))
			require.NoError(t, err)
			assert.Equal(t, tc.grant, grant, "Verify returns the grant as the application set it")

			form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {body["refresh_token"].(string)}, "client_id": {"public"}}
			res = env.postForm(t, "/oauth/token", form)
			require.Equal(t, http.StatusOK, res.status, res.body)
			body = res.json(t)
			assert.Equal(t, tc.wantResponse, body["scope"], "the response scope survives a refresh")
			grant, err = env.s.Verify(ctx, body["access_token"].(string))
			require.NoError(t, err)
			assert.Equal(t, tc.grant, grant)
		})
	}
}

func TestProtect(t *testing.T) {
	env := newEnv(t, nil, nil)
	challenge := `Bearer resource_metadata="` + env.url + `/.well-known/oauth-protected-resource/mcp"`

	res := env.bearer(t, "")
	assert.Equal(t, http.StatusUnauthorized, res.status)
	assert.Equal(t, challenge, res.header.Get("WWW-Authenticate"))

	res = env.bearer(t, "key:secret")
	assert.Equal(t, http.StatusUnauthorized, res.status)
	assert.Equal(t, challenge, res.header.Get("WWW-Authenticate"))
	assert.Equal(t, "unknown or expired access token: invalid token\n", res.body)
	assert.NotContains(t, res.body, "oauth.Server")
	assert.NotContains(t, res.body, "verifyToken")

	access, _ := env.tokens(t)
	res = env.bearer(t, access)
	assert.Equal(t, http.StatusOK, res.status)

	res = env.bearer(t, shapedToken(t))
	assert.Equal(t, http.StatusUnauthorized, res.status)
	assert.Equal(t, "unknown or expired access token: invalid token\n", res.body)

	var fallbackCalls atomic.Int32
	fallback := newEnv(t, func(c *Config) {
		c.Fallback = func(ctx context.Context, token string) (bool, error) {
			fallbackCalls.Add(1)
			return token == "key:secret", nil
		}
	}, nil)
	res = fallback.bearer(t, "key:secret")
	assert.Equal(t, http.StatusOK, res.status)
	assert.Equal(t, "user=", res.body)
	res = fallback.bearer(t, "other")
	assert.Equal(t, http.StatusUnauthorized, res.status)
	assert.Equal(t, "the bearer token is not accepted: invalid token\n", res.body)
	assert.NotContains(t, res.body, "oauth.Server")
	assert.NotContains(t, res.body, "verifyToken")
	assert.Equal(t, int32(2), fallbackCalls.Load())
	res = fallback.bearer(t, shapedToken(t))
	assert.Equal(t, http.StatusUnauthorized, res.status, "an unknown access token never reaches Fallback")
	assert.Equal(t, int32(2), fallbackCalls.Load())

	var fallbackReported []error
	failing := newEnv(t, func(c *Config) {
		c.Fallback = func(ctx context.Context, token string) (bool, error) {
			return false, errors.New("key store is down")
		}
		c.OnError = func(ctx context.Context, err error) {
			fallbackReported = append(fallbackReported, err)
		}
	}, nil)
	res = failing.bearer(t, "key:secret")
	assert.Equal(t, http.StatusInternalServerError, res.status, "a failed Fallback is not a refused token")
	assert.Equal(t, "the bearer token could not be checked\n", res.body)
	assert.Empty(t, res.header.Get("WWW-Authenticate"), "a failed check does not start discovery")
	assert.NotContains(t, res.body, "key store is down")
	require.Len(t, fallbackReported, 1, "the Fallback error reaches OnError")
	assert.EqualError(t, fallbackReported[0], "key store is down")

	store := &brokenStore{Store: NewMemoryStore(), broken: map[string]bool{KindAccess: true}}
	var reported []error
	broken := newEnv(t, func(c *Config) {
		c.Store = store
		c.OnError = func(ctx context.Context, err error) {
			reported = append(reported, err)
		}
	}, nil)
	res = broken.bearer(t, "any")
	assert.Equal(t, http.StatusUnauthorized, res.status, "a token of another shape is not looked up")
	assert.Empty(t, reported)
	res = broken.bearer(t, shapedToken(t))
	assert.Equal(t, http.StatusInternalServerError, res.status)
	assert.Equal(t, "the access token could not be checked\n", res.body)
	assert.NotContains(t, res.body, "oauth.Server")
	assert.NotContains(t, res.body, "verifyToken")
	assert.NotContains(t, res.body, "store is down")
	require.Len(t, reported, 1, "the Store error reaches OnError")
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(reported[0]))
	assert.Contains(t, reported[0].Error(), "store is down")
}

// TestResourceMetadataURLEscaped: a percent-encoded Resource path stays
// encoded in the challenge, and the URL it names serves the metadata.
func TestResourceMetadataURLEscaped(t *testing.T) {
	env := newEnv(t, func(c *Config) {
		c.Resource = c.Issuer + "/m%20cp"
	}, nil)
	metadataURL := env.url + "/.well-known/oauth-protected-resource/m%20cp"
	assert.Equal(t, metadataURL, env.s.resourceMetadataURL())

	req, err := http.NewRequest(http.MethodPost, env.url+"/m%20cp", nil)
	require.NoError(t, err)
	res := env.do(t, req)
	assert.Equal(t, http.StatusUnauthorized, res.status)
	assert.Equal(t, `Bearer resource_metadata="`+metadataURL+`"`, res.header.Get("WWW-Authenticate"))

	req, err = http.NewRequest(http.MethodGet, metadataURL, nil)
	require.NoError(t, err)
	res = env.do(t, req)
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.Equal(t, env.url+"/m%20cp", res.json(t)["resource"])
}

func TestAuthFunc(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t, func(c *Config) {
		c.AccessTokenTTL = 50 * time.Millisecond
	}, nil)
	authFunc := env.s.AuthFunc()

	header, err := authFunc(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, header)

	access, _ := env.tokens(t)
	header, err = authFunc(ctx, access)
	require.NoError(t, err)
	assert.Equal(t, http.Header{"Authorization": {"Bearer " + access}}, header, "the access token is passed through")

	_, err = authFunc(ctx, shapedToken(t))
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	_, err = authFunc(ctx, "key:secret")
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "without Fallback other tokens fail")

	time.Sleep(100 * time.Millisecond)
	_, err = authFunc(ctx, access)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "an expired token fails so the client refreshes")

	var seen []string
	fallback := newEnv(t, func(c *Config) {
		c.Fallback = func(ctx context.Context, token string) (bool, error) {
			seen = append(seen, token)
			return token == "key:secret", nil
		}
	}, nil)
	header, err = fallback.s.AuthFunc()(ctx, "key:secret")
	require.NoError(t, err)
	assert.Equal(t, http.Header{"Authorization": {"Bearer key:secret"}}, header, "an accepted token is passed through")
	_, err = fallback.s.AuthFunc()(ctx, "other")
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "a refused token fails as a bad credential")
	_, err = fallback.s.AuthFunc()(ctx, shapedToken(t))
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	assert.Equal(t, []string{"key:secret", "other"}, seen, "access-token shaped tokens never reach Fallback")

	var reported []error
	failing := newEnv(t, func(c *Config) {
		c.Fallback = func(ctx context.Context, token string) (bool, error) {
			return false, errors.New("key store is down")
		}
		c.OnError = func(ctx context.Context, err error) {
			reported = append(reported, err)
		}
	}, nil)
	_, err = failing.s.AuthFunc()(ctx, "key:secret")
	require.Error(t, err)
	assert.NotEqual(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "a failed Fallback is not a bad credential")
	assert.Contains(t, err.Error(), "key store is down")
	require.Len(t, reported, 1, "the Fallback error reaches OnError")
	assert.EqualError(t, reported[0], "key store is down")
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t, func(c *Config) {
		c.AccessTokenTTL = 50 * time.Millisecond
	}, nil)
	access, refresh := env.tokens(t)

	grant, err := env.s.Verify(ctx, access)
	require.NoError(t, err)
	assert.Equal(t, issuedGrant, grant, "the requested scope is not granted")

	for name, value := range map[string]string{
		"unknown":       shapedToken(t),
		"malformed":     "key:secret",
		"empty":         "",
		"refresh token": refresh,
		"other prefix":  "acmeat_" + access[len("testat_"):],
	} {
		_, err = env.s.Verify(ctx, value)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), name)
	}

	time.Sleep(100 * time.Millisecond)
	_, err = env.s.Verify(ctx, access)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "expired")

	store := &brokenStore{Store: NewMemoryStore(), broken: map[string]bool{}}
	broken := newEnv(t, func(c *Config) {
		c.Store = store
	}, nil)
	access, _ = broken.tokens(t)
	store.set(KindAccess, true)
	_, err = broken.s.Verify(ctx, access)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	_, err = broken.s.Verify(ctx, "key:secret")
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "a malformed token is refused without a Store read")
}

// TestStoreIDsAreHashed: no secret is ever a Store id. Codes, access and
// refresh tokens, rotated ones included, are stored under their hex SHA-256.
func TestStoreIDsAreHashed(t *testing.T) {
	store := NewMemoryStore().(*memoryStore)
	env := newEnv(t, func(c *Config) {
		c.Store = store
	}, nil)

	// Keep a code unexchanged so it is in the Store too.
	_, challenge := pkce("h")
	code := env.codeFor(t, "public", challenge)
	access, refresh := env.tokens(t)
	res := env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {refresh}})
	require.Equal(t, http.StatusOK, res.status, res.body)
	newAccess := res.json(t)["access_token"].(string)
	newRefresh := res.json(t)["refresh_token"].(string)
	secrets := []string{code, access, refresh, newAccess, newRefresh}

	store.mu.Lock()
	defer store.mu.Unlock()
	seen := map[string]int{}
	for key, record := range store.records {
		kind, id, found := strings.Cut(key, "\x00")
		require.True(t, found)
		for _, secret := range secrets {
			assert.NotEqual(t, secret, id, kind)
			assert.NotContains(t, string(record.value), secret, kind)
		}
		switch kind {
		case KindAccess, KindRefresh, KindCode:
			seen[kind]++
			assert.Len(t, id, 64, kind)
			_, err := hex.DecodeString(id)
			assert.NoError(t, err, kind)
		}
	}
	assert.Equal(t, map[string]int{KindAccess: 2, KindRefresh: 2, KindCode: 1}, seen)
	_, ok := store.records[memoryKey(KindCode, hashID(code))]
	assert.True(t, ok, "the code is stored under its hash")
	_, ok = store.records[memoryKey(KindRefresh, hashID(refresh))]
	assert.True(t, ok, "the rotated refresh token is kept under its hash for reuse detection")
}

// TestRefreshTokenTTLNever: a family issued with Never expires IdleTTL after
// its issue, as does its refresh token, remembers IdleTTL, and keeps rotating.
func TestRefreshTokenTTLNever(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore().(*memoryStore)
	env := newEnv(t, func(c *Config) {
		c.Store = store
		c.RefreshTokenTTL = Never
		c.IdleTTL = 2 * time.Hour
	}, nil)
	expiry := func(kind, id string) time.Time {
		store.mu.Lock()
		defer store.mu.Unlock()
		return store.records[memoryKey(kind, id)].expiresAt
	}
	_, refresh := env.tokens(t)
	var record tokenRecord
	require.NoError(t, env.s.getRecord(ctx, KindRefresh, hashID(refresh), false, &record))
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), expiry(KindRefresh, hashID(refresh)), 5*time.Second)
	family, _, err := env.s.getFamily(ctx, record.Family)
	require.NoError(t, err)
	assert.Equal(t, 2*time.Hour, family.Idle)
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), family.ExpiresAt, 5*time.Second)
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), expiry(KindFamily, record.Family), 5*time.Second)

	res := env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {refresh}})
	require.Equal(t, http.StatusOK, res.status, res.body)
	newRefresh := res.json(t)["refresh_token"].(string)
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), expiry(KindRefresh, hashID(newRefresh)), 5*time.Second)
	assert.WithinDuration(t, time.Now().Add(2*time.Hour), expiry(KindFamily, record.Family), 5*time.Second)
}

// brokenStore fails every call of the kinds set broken, after sleeping
// delay.
type brokenStore struct {
	Store
	mu     sync.Mutex
	broken map[string]bool
	delay  time.Duration
}

func (b *brokenStore) set(kind string, broken bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.broken[kind] = broken
}

func (b *brokenStore) setDelay(delay time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.delay = delay
}

func (b *brokenStore) fail(kind string) error {
	b.mu.Lock()
	broken := b.broken[kind]
	delay := b.delay
	b.mu.Unlock()
	if !broken {
		return nil
	}
	time.Sleep(delay)
	return ez.New(ez.EUNAVAILABLE, "store is down", nil)
}

func (b *brokenStore) Put(ctx context.Context, kind, id string, value []byte, expiresAt time.Time) error {
	err := b.fail(kind)
	if err != nil {
		return err
	}
	return b.Store.Put(ctx, kind, id, value, expiresAt)
}

func (b *brokenStore) Get(ctx context.Context, kind, id string) ([]byte, error) {
	err := b.fail(kind)
	if err != nil {
		return nil, err
	}
	return b.Store.Get(ctx, kind, id)
}

func (b *brokenStore) Take(ctx context.Context, kind, id string) ([]byte, error) {
	err := b.fail(kind)
	if err != nil {
		return nil, err
	}
	return b.Store.Take(ctx, kind, id)
}

// cancelStore fails every call whose context is done, as database drivers
// do. An armed Put runs cancel before that check, so a test can cancel a
// request in the middle of a refresh.
type cancelStore struct {
	Store
	mu     sync.Mutex
	kind   string
	cancel context.CancelFunc
}

// arm makes the next Put of kind call cancel.
func (c *cancelStore) arm(kind string, cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kind = kind
	c.cancel = cancel
}

// fire calls the armed cancel when kind matches it.
func (c *cancelStore) fire(kind string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil && kind == c.kind {
		c.cancel()
		c.cancel = nil
	}
}

func (c *cancelStore) done(ctx context.Context) error {
	if ctx.Err() != nil {
		return ez.New(ez.EUNAVAILABLE, "store: the context is done", ctx.Err())
	}
	return nil
}

func (c *cancelStore) Put(ctx context.Context, kind, id string, value []byte, expiresAt time.Time) error {
	c.fire(kind)
	err := c.done(ctx)
	if err != nil {
		return err
	}
	return c.Store.Put(ctx, kind, id, value, expiresAt)
}

func (c *cancelStore) Get(ctx context.Context, kind, id string) ([]byte, error) {
	err := c.done(ctx)
	if err != nil {
		return nil, err
	}
	return c.Store.Get(ctx, kind, id)
}

func (c *cancelStore) Swap(ctx context.Context, kind, id string, old, value []byte, expiresAt time.Time) error {
	err := c.done(ctx)
	if err != nil {
		return err
	}
	return c.Store.Swap(ctx, kind, id, old, value, expiresAt)
}

// TestRefreshSurvivesDisconnect: a refresh runs detached from its request,
// so a cancelled request fails no Store call, and a request gone before the
// Swap commits nothing and gets no answer; a refresh whose Store fails or
// runs out of RefreshTimeout before the Swap keeps the refresh token too, so
// the retry is never answered invalid_grant.
func TestRefreshSurvivesDisconnect(t *testing.T) {
	cancelling := &cancelStore{Store: NewMemoryStore()}
	store := &brokenStore{Store: cancelling, broken: map[string]bool{}}
	var mu sync.Mutex
	var reportedCtxErrs []error
	env := newEnv(t, func(c *Config) {
		c.Store = store
		c.RefreshTimeout = 100 * time.Millisecond
		c.OnError = func(ctx context.Context, err error) {
			mu.Lock()
			defer mu.Unlock()
			reportedCtxErrs = append(reportedCtxErrs, ctx.Err())
		}
	}, nil)
	refreshTimeout := env.s.cfg.RefreshTimeout
	refreshWith := func(ctx context.Context, token string) response {
		form := url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {token}}
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		env.s.Handler().ServeHTTP(rec, req)
		return response{status: rec.Code, header: rec.Header(), body: rec.Body.String()}
	}
	retry := func(t *testing.T, token string) {
		t.Helper()
		res := refreshWith(context.Background(), token)
		assert.Equal(t, http.StatusOK, res.status, "the refresh token survived: "+res.body)
	}
	// uncommitted checks that a request gone before the Swap was answered
	// nothing, and that the presented token still rotates, which a
	// committed rotation would answer as reuse.
	uncommitted := func(t *testing.T, res response, token string) {
		t.Helper()
		assert.Empty(t, res.body, "no response, and no token, is written for a client that is gone")
		retry(t, token)
	}

	t.Run("cancelled mid-refresh", func(t *testing.T) {
		_, refresh := env.tokens(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cancelling.arm(KindAccess, cancel)
		uncommitted(t, refreshWith(ctx, refresh), refresh)
	})

	t.Run("cancelled before the request", func(t *testing.T) {
		_, refresh := env.tokens(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		uncommitted(t, refreshWith(ctx, refresh), refresh)
	})

	t.Run("write outlasts refreshTimeout", func(t *testing.T) {
		_, refresh := env.tokens(t)
		store.set(KindAccess, true)
		store.setDelay(2 * refreshTimeout)
		res := refreshWith(context.Background(), refresh)
		store.set(KindAccess, false)
		store.setDelay(0)
		assert.Equal(t, "server_error", oauthErrorCode(t, res))
		retry(t, refresh)
	})

	t.Run("read outlasts refreshTimeout", func(t *testing.T) {
		_, refresh := env.tokens(t)
		store.set(KindRefresh, true)
		store.setDelay(2 * refreshTimeout)
		mu.Lock()
		before := len(reportedCtxErrs)
		mu.Unlock()
		res := refreshWith(context.Background(), refresh)
		store.set(KindRefresh, false)
		store.setDelay(0)
		assert.Equal(t, "server_error", oauthErrorCode(t, res))
		mu.Lock()
		reported := reportedCtxErrs[before:]
		mu.Unlock()
		require.Len(t, reported, 1, "the failed read reaches OnError")
		assert.NoError(t, reported[0], "OnError gets a live context after the refresh deadline")
		retry(t, refresh)
	})

	t.Run("failing Store", func(t *testing.T) {
		_, refresh := env.tokens(t)
		for _, kind := range []string{KindRefresh, KindFamily, KindAccess} {
			store.set(kind, true)
			res := refreshWith(context.Background(), refresh)
			store.set(kind, false)
			assert.Equal(t, http.StatusInternalServerError, res.status, kind)
			assert.Equal(t, "server_error", oauthErrorCode(t, res), kind)
		}
		retry(t, refresh)
	})

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, reportedCtxErrs)
	for _, err := range reportedCtxErrs {
		assert.NoError(t, err, "OnError gets a live context after a disconnect")
	}
}

func TestStoreFailures(t *testing.T) {
	ctx := context.Background()
	store := &brokenStore{Store: NewMemoryStore(), broken: map[string]bool{}}
	var reported atomic.Int32
	env := newEnv(t, func(c *Config) {
		c.Store = store
		c.OnError = func(ctx context.Context, err error) {
			assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
			assert.Contains(t, err.Error(), "store is down")
			reported.Add(1)
		}
	}, nil)
	_, challenge := pkce("s")

	registered := env.postJSON(t, "/oauth/register", `{"redirect_uris": ["`+testRedirect+`"]}`).json(t)["client_id"].(string)
	store.set(KindRequest, true)
	q := env.authorize(t, authorizeParams("public", challenge)).location(t).Query()
	assert.Equal(t, "server_error", q.Get("error"))
	res := env.authorize(t, authorizeParams(registered, challenge))
	assert.Equal(t, http.StatusInternalServerError, res.status, "a registered client gets the error page")
	assert.Empty(t, res.header.Get("Location"))
	res = env.get(t, "/oauth/login?request=abc")
	assert.Equal(t, http.StatusInternalServerError, res.status)
	assert.Contains(t, res.body, "Something went wrong")
	store.set(KindRequest, false)

	store.set(KindClient, true)
	res = env.authorize(t, authorizeParams("dynamic", challenge))
	assert.Equal(t, http.StatusInternalServerError, res.status)
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"dynamic"}, "refresh_token": {"r"}})
	assert.Equal(t, http.StatusInternalServerError, res.status)
	assert.Equal(t, "server_error", oauthErrorCode(t, res))
	res = env.postJSON(t, "/oauth/register", `{"redirect_uris": ["`+testRedirect+`"]}`)
	assert.Equal(t, http.StatusInternalServerError, res.status)
	store.set(KindClient, false)

	id := env.startLogin(t, authorizeParams("public", challenge))
	store.set(KindCode, true)
	res = env.login(t, id, "ana@acme.com", "pw")
	assert.Equal(t, http.StatusInternalServerError, res.status)
	store.set(KindCode, false)

	verifier, challenge := pkce("t")
	code := env.codeFor(t, "public", challenge)
	store.set(KindAccess, true)
	res = env.exchange(t, code, verifier)
	assert.Equal(t, http.StatusInternalServerError, res.status)
	assert.Equal(t, "server_error", oauthErrorCode(t, res))
	assert.Equal(t, http.StatusInternalServerError, env.bearer(t, shapedToken(t)).status)
	_, err := env.s.AuthFunc()(ctx, shapedToken(t))
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	_, err = env.s.Verify(ctx, shapedToken(t))
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	store.set(KindAccess, false)

	code = env.codeFor(t, "public", challenge)
	store.set(KindRefresh, true)
	res = env.exchange(t, code, verifier)
	assert.Equal(t, http.StatusInternalServerError, res.status)
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {shapedRefreshToken(t)}})
	assert.Equal(t, http.StatusInternalServerError, res.status, "a Store outage is not invalid_grant")
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {"r"}})
	assert.Equal(t, http.StatusBadRequest, res.status, "a malformed refresh token is answered without the Store")
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))
	store.set(KindRefresh, false)

	store.set(KindCode, true)
	res = env.exchange(t, "c", verifier)
	assert.Equal(t, http.StatusInternalServerError, res.status)
	assert.Equal(t, "server_error", oauthErrorCode(t, res))
	store.set(KindCode, false)

	// A refresh whose token write failed keeps the presented token.
	_, refresh := env.tokens(t)
	store.set(KindAccess, true)
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {refresh}})
	assert.Equal(t, "server_error", oauthErrorCode(t, res))
	store.set(KindAccess, false)
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {refresh}})
	assert.Equal(t, http.StatusOK, res.status, "the refresh token survived the failed attempt: "+res.body)
	assert.Greater(t, reported.Load(), int32(8), "every hidden Store failure reaches OnError")

	_, err = env.s.Approve(ctx, "missing", testGrant)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	require.NoError(t, env.s.putRecord(ctx, KindRequest, "garbled", "not an object", time.Now().Add(time.Minute)))
	_, err = env.s.Approve(ctx, "garbled", testGrant)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
}
