package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func (env *testEnv) postJSON(t *testing.T, path, body string) response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, env.url+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	return env.do(t, req)
}

func TestRegisterPublicClient(t *testing.T) {
	env := newEnv(t, nil, nil)
	res := env.postJSON(t, "/oauth/register", `{"client_name": "Claude", "redirect_uris": ["`+testRedirect+`", "http://127.0.0.1:3000/cb"], "grant_types": ["authorization_code"]}`)
	require.Equal(t, http.StatusCreated, res.status, res.body)
	assert.Equal(t, "*", res.header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "no-store", res.header.Get("Cache-Control"))
	body := res.json(t)
	clientID := body["client_id"].(string)
	assert.Len(t, clientID, 43)
	assert.NotContains(t, body, "client_secret")
	assert.NotContains(t, body, "client_secret_expires_at")
	assert.Equal(t, "Claude", body["client_name"])
	assert.Equal(t, "none", body["token_endpoint_auth_method"])
	assert.Equal(t, []any{testRedirect, "http://127.0.0.1:3000/cb"}, body["redirect_uris"])
	assert.Equal(t, []any{"authorization_code", "refresh_token"}, body["grant_types"])
	assert.Equal(t, []any{"code"}, body["response_types"])
	assert.NotZero(t, body["client_id_issued_at"])

	// The registered client runs the whole flow.
	verifier, challenge := pkce("d")
	code := env.codeFor(t, clientID, challenge)
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {testRedirect}})
	assert.Equal(t, http.StatusOK, res.status, res.body)
}

func TestRegisterConfidentialClient(t *testing.T) {
	env := newEnv(t, nil, nil)
	for _, method := range []string{"client_secret_basic", "client_secret_post"} {
		res := env.postJSON(t, "/oauth/register", `{"redirect_uris": ["`+testRedirect+`"], "token_endpoint_auth_method": "`+method+`"}`)
		require.Equal(t, http.StatusCreated, res.status, res.body)
		body := res.json(t)
		assert.Equal(t, method, body["token_endpoint_auth_method"])
		secret, _ := body["client_secret"].(string)
		assert.Len(t, secret, 43)
		require.Contains(t, body, "client_secret_expires_at")
		assert.Equal(t, float64(0), body["client_secret_expires_at"], "0: the secret never expires")
		clientID := body["client_id"].(string)

		verifier, challenge := pkce("e")
		code := env.codeFor(t, clientID, challenge)
		form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}, "redirect_uri": {testRedirect}}
		res = env.postForm(t, "/oauth/token", form)
		assert.Equal(t, "invalid_client", oauthErrorCode(t, res), "a confidential client needs its secret")

		code = env.codeFor(t, clientID, challenge)
		form.Set("code", code)
		form.Set("client_secret", secret)
		res = env.postForm(t, "/oauth/token", form)
		assert.Equal(t, http.StatusOK, res.status, res.body)
	}
}

func TestRegisterErrors(t *testing.T) {
	env := newEnv(t, nil, nil)
	cases := map[string]struct {
		body string
		code string
	}{
		"not JSON":         {`redirect_uris=x`, "invalid_client_metadata"},
		"no redirect":      {`{"client_name": "x"}`, "invalid_redirect_uri"},
		"http redirect":    {`{"redirect_uris": ["http://client.example/cb"]}`, "invalid_redirect_uri"},
		"fragment":         {`{"redirect_uris": ["https://client.example/cb#x"]}`, "invalid_redirect_uri"},
		"custom scheme":    {`{"redirect_uris": ["myapp:/cb"]}`, "invalid_redirect_uri"},
		"relative":         {`{"redirect_uris": ["/cb"]}`, "invalid_redirect_uri"},
		"private_key_jwt":  {`{"redirect_uris": ["` + testRedirect + `"], "token_endpoint_auth_method": "private_key_jwt"}`, "invalid_client_metadata"},
		"redirect not URL": {`{"redirect_uris": ["https://client.example/%zz"]}`, "invalid_redirect_uri"},
		"unicode host":     {`{"redirect_uris": ["https://clаude.ai/cb"]}`, "invalid_redirect_uri"},
	}
	for name, c := range cases {
		res := env.postJSON(t, "/oauth/register", c.body)
		assert.Equal(t, http.StatusBadRequest, res.status, name)
		assert.Equal(t, c.code, oauthErrorCode(t, res), name)
	}
	for _, uri := range []string{"http://localhost/cb", "http://127.0.0.1:9/cb", "http://[::1]:9/cb", "https://client.example/cb?x=1"} {
		res := env.postJSON(t, "/oauth/register", `{"redirect_uris": ["`+uri+`"]}`)
		assert.Equal(t, http.StatusCreated, res.status, uri)
	}
}

func TestAllowedRedirectHosts(t *testing.T) {
	host := newDocumentHost(t)
	env := newEnv(t, func(c *Config) {
		c.AllowedRedirectHosts = []string{"claude.ai", "127.0.0.1"}
		c.HTTPClient = host.server.Client()
	}, nil)

	res := env.postJSON(t, "/oauth/register", `{"redirect_uris": ["https://claude.ai/api/mcp/auth_callback", "http://127.0.0.1:3000/cb"]}`)
	assert.Equal(t, http.StatusCreated, res.status, res.body)
	res = env.postJSON(t, "/oauth/register", `{"redirect_uris": ["https://claude.ai/cb", "`+testRedirect+`"]}`)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Equal(t, "invalid_redirect_uri", oauthErrorCode(t, res))
	res = env.postJSON(t, "/oauth/register", `{"redirect_uris": ["https://claude.ai.evil.example/cb"]}`)
	assert.Equal(t, http.StatusBadRequest, res.status)

	_, challenge := pkce("h")
	allowed := host.add("/allowed.json", map[string]any{"client_name": "Claude", "redirect_uris": []string{"https://claude.ai/cb"}})
	params := authorizeParams(allowed, challenge)
	params.Set("redirect_uri", "https://claude.ai/cb")
	res = env.authorize(t, params)
	assert.Equal(t, http.StatusFound, res.status, res.body)

	other := host.add("/other.json", map[string]any{"client_name": "Other", "redirect_uris": []string{testRedirect}})
	res = env.authorize(t, authorizeParams(other, challenge))
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "host this server does not accept")

	// A client stored before the restriction was configured is held to it.
	stale := Client{ID: "stale", Name: "Stale", RedirectURIs: []string{testRedirect}}
	require.NoError(t, env.s.putRecord(context.Background(), KindClient, "stale", stale, time.Now().Add(time.Hour)))
	res = env.authorize(t, authorizeParams("stale", challenge))
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "host this server does not accept")

	// Config.Clients are not restricted.
	env.codeFor(t, "public", challenge)
}

// TestRegisteredClientLifetime: a registration lives a year from its last
// authorize; cached documents keep their 1h lifetime.
func TestRegisteredClientLifetime(t *testing.T) {
	host := newDocumentHost(t)
	env := newEnv(t, func(c *Config) {
		c.HTTPClient = host.server.Client()
	}, nil)
	store := env.s.cfg.Store.(*memoryStore)
	expiry := func(id string) time.Duration {
		store.mu.Lock()
		defer store.mu.Unlock()
		return time.Until(store.records[memoryKey(KindClient, id)].expiresAt)
	}
	year := 365 * 24 * time.Hour

	clientID := env.postJSON(t, "/oauth/register", `{"redirect_uris": ["`+testRedirect+`"]}`).json(t)["client_id"].(string)
	assert.InDelta(t, year.Seconds(), expiry(clientID).Seconds(), 60)

	var client Client
	require.NoError(t, env.s.getRecord(context.Background(), KindClient, clientID, false, &client))
	require.NoError(t, env.s.putRecord(context.Background(), KindClient, clientID, client, time.Now().Add(time.Hour)))
	_, challenge := pkce("l")
	env.startLogin(t, authorizeParams(clientID, challenge))
	assert.InDelta(t, year.Seconds(), expiry(clientID).Seconds(), 60, "authorize renews the registration")

	documentID := host.add("/client.json", map[string]any{"client_name": "Hosted", "redirect_uris": []string{testRedirect}})
	env.startLogin(t, authorizeParams(documentID, challenge))
	assert.LessOrEqual(t, expiry(documentID), time.Hour, "documents stay cached for 1h only")

	env.startLogin(t, authorizeParams("public", challenge))
	store.mu.Lock()
	_, stored := store.records[memoryKey(KindClient, "public")]
	store.mu.Unlock()
	assert.False(t, stored, "configured clients are not stored")
}

// documentHost serves Client ID Metadata Documents over TLS and counts the
// fetches.
type documentHost struct {
	server  *httptest.Server
	fetches atomic.Int32
	mu      sync.Mutex
	docs    map[string]string
}

func newDocumentHost(t *testing.T) *documentHost {
	t.Helper()
	host := &documentHost{docs: map[string]string{}}
	host.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host.fetches.Add(1)
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		switch r.URL.Path {
		case "/moved.json":
			http.Redirect(w, r, "/client.json", http.StatusFound)
			return
		case "/huge.json":
			w.Write([]byte(`{"client_id": "` + strings.Repeat("x", 70<<10) + `"}`))
			return
		}
		host.mu.Lock()
		doc, ok := host.docs[r.URL.Path]
		host.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(doc))
	}))
	t.Cleanup(host.server.Close)
	return host
}

// add publishes a document at path and returns its client_id.
func (h *documentHost) add(path string, doc map[string]any) string {
	clientID := h.server.URL + path
	_, ok := doc["client_id"]
	if !ok {
		doc["client_id"] = clientID
	}
	data, _ := json.Marshal(doc)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.docs[path] = string(data)
	return clientID
}

func TestClientMetadataDocument(t *testing.T) {
	host := newDocumentHost(t)
	clientID := host.add("/client.json", map[string]any{"client_name": "Hosted Client", "redirect_uris": []string{testRedirect}})
	env := newEnv(t, func(c *Config) {
		c.HTTPClient = host.server.Client()
	}, nil)

	_, challenge := pkce("m")
	id := env.startLogin(t, authorizeParams(clientID, challenge))
	page := env.get(t, "/oauth/login?request="+id)
	assert.Contains(t, page.body, "Hosted Client")
	assert.Equal(t, int32(1), host.fetches.Load())

	// The document is cached.
	env.startLogin(t, authorizeParams(clientID, challenge))
	assert.Equal(t, int32(1), host.fetches.Load())

	params := authorizeParams(clientID, challenge)
	params.Set("redirect_uri", "https://client.example/elsewhere")
	res := env.authorize(t, params)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "redirect URI is not registered")
}

func TestClientMetadataDocumentRejected(t *testing.T) {
	host := newDocumentHost(t)
	env := newEnv(t, func(c *Config) {
		c.HTTPClient = host.server.Client()
	}, nil)
	_, challenge := pkce("m")

	cases := map[string]string{
		"other client_id": host.add("/other.json", map[string]any{"client_id": "https://evil.example/client.json", "client_name": "Evil", "redirect_uris": []string{testRedirect}}),
		"no name":         host.add("/noname.json", map[string]any{"redirect_uris": []string{testRedirect}}),
		"no redirects":    host.add("/noredirect.json", map[string]any{"client_name": "X"}),
		"bad redirect":    host.add("/badredirect.json", map[string]any{"client_name": "X", "redirect_uris": []string{"http://client.example/cb"}}),
		"unicode host":    host.add("/unicode.json", map[string]any{"client_name": "X", "redirect_uris": []string{"https://clаude.ai/cb"}}),
		"missing":         host.server.URL + "/missing.json",
		"redirected":      host.server.URL + "/moved.json",
		"too large":       host.server.URL + "/huge.json",
	}
	host.mu.Lock()
	host.docs["/notjson.json"] = "<html>"
	host.mu.Unlock()
	cases["not JSON"] = host.server.URL + "/notjson.json"
	for name, clientID := range cases {
		res := env.authorize(t, authorizeParams(clientID, challenge))
		assert.Equal(t, http.StatusBadRequest, res.status, name)
		assert.Contains(t, res.body, "Invalid application", name)
		assert.NotContains(t, res.body, "404", name)
		assert.NotContains(t, res.body, "status", name)
	}
	res := env.authorize(t, authorizeParams(cases["missing"], challenge))
	assert.Contains(t, res.body, "the client metadata document could not be fetched")

	// A host that cannot be reached is not the client's fault.
	res = env.authorize(t, authorizeParams("https://127.0.0.1:1/client.json", challenge))
	assert.Equal(t, http.StatusServiceUnavailable, res.status)
	assert.Contains(t, res.body, "Application unavailable")

	// The token endpoint never fetches a document: an unknown document
	// client is a public client without grants.
	fetches := host.fetches.Load()
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {cases["missing"]}, "refresh_token": {"r"}})
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))
	assert.Equal(t, fetches, host.fetches.Load())
}

// TestClientMetadataDocumentRefreshWithoutHost: an outage at the document
// host does not disconnect the users of that client.
func TestClientMetadataDocumentRefreshWithoutHost(t *testing.T) {
	host := newDocumentHost(t)
	clientID := host.add("/client.json", map[string]any{"client_name": "Hosted Client", "redirect_uris": []string{testRedirect}})
	env := newEnv(t, func(c *Config) {
		c.HTTPClient = host.server.Client()
	}, nil)

	verifier, challenge := pkce("r")
	code := env.codeFor(t, clientID, challenge)
	res := env.postForm(t, "/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"code_verifier": {verifier},
	})
	require.Equal(t, http.StatusOK, res.status, res.body)
	refresh := res.json(t)["refresh_token"].(string)

	// The cached document is gone and the host is down.
	require.NoError(t, env.s.cfg.Store.Delete(context.Background(), KindClient, clientID))
	host.server.Close()
	fetches := host.fetches.Load()

	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refresh}})
	assert.Equal(t, http.StatusOK, res.status, res.body)
	assert.Equal(t, fetches, host.fetches.Load())

	// Another client cannot use the refresh token with the document id.
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {res.json(t)["refresh_token"].(string)}})
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))
}

// TestClientMetadataDocumentHostGuard runs the default client: the guard
// refuses the resolved addresses before connecting.
func TestClientMetadataDocumentHostGuard(t *testing.T) {
	s, err := New(Config{Issuer: "https://api.acme.com", Resource: "https://api.acme.com/mcp", Store: NewMemoryStore(), LoginURL: "https://app.acme.com/login"})
	require.NoError(t, err)
	for _, clientID := range []string{"https://127.0.0.1/client.json", "https://localhost/client.json", "https://[::1]/client.json", "https://10.0.0.1/client.json", "https://169.254.169.254/client.json"} {
		_, err = s.lookupClient(context.Background(), clientID)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), clientID)
		assert.Equal(t, "oauth: client_id resolves to an address this server does not fetch", ez.ErrorMessage(err), clientID)
	}
}

// TestClientMetadataDocumentHostGuardInvalid: a document served from a
// loopback address is an invalid client, not one to try again later, and is
// never fetched.
func TestClientMetadataDocumentHostGuardInvalid(t *testing.T) {
	var fetches atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
	}))
	t.Cleanup(host.Close)
	env := newEnv(t, nil, nil)
	_, challenge := pkce("g")

	res := env.authorize(t, authorizeParams(host.URL+"/client.json", challenge))
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Invalid application")
	assert.Contains(t, res.body, "client_id resolves to an address this server does not fetch")
	assert.NotContains(t, res.body, "127.0.0.1")
	assert.Equal(t, int32(0), fetches.Load())
}

func TestPublicAddressOnly(t *testing.T) {
	refused := map[string]string{
		"loopback v4":        "127.0.0.1:443",
		"loopback v6":        "[::1]:443",
		"loopback v4 in v6":  "[::ffff:127.0.0.1]:443",
		"private 10/8":       "10.1.2.3:443",
		"private 172.16/12":  "172.16.0.1:443",
		"private 192.168/16": "192.168.1.1:443",
		"link-local v4":      "169.254.169.254:443",
		"link-local v6":      "[fe80::1]:443",
		"unspecified v4":     "0.0.0.0:443",
		"unspecified v6":     "[::]:443",
		"multicast v4":       "224.0.0.1:443",
		"multicast v6":       "[ff02::1]:443",
		"unique-local v6":    "[fd00::1]:443",
		"this network":       "0.1.2.3:443",
		"carrier-grade NAT":  "100.64.0.1:443",
		"cgnat metadata":     "100.100.100.200:443",
		"protocol assign.":   "192.0.0.9:443",
		"benchmarking":       "198.18.0.1:443",
		"nat64 link-local":   "[64:ff9b::a9fe:a9fe]:443",
		"nat64 local-use":    "[64:ff9b:1::a9fe:a9fe]:443",
		"class E":            "240.0.0.1:443",
		"teredo":             "[2001:0:c0a8:101::1]:443",
		"6to4":               "[2002:a9fe:a9fe::1]:443",
		"private v4 in v6":   "[::ffff:10.0.0.1]:443",
	}
	for name, address := range refused {
		err := publicAddressOnly("tcp", address, nil)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), name)
	}
	for _, address := range []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443", "8.8.8.8:80"} {
		assert.NoError(t, publicAddressOnly("tcp", address, nil), address)
	}
	for _, address := range []string{"no-port", "example.com:443"} {
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(publicAddressOnly("tcp", address, nil)), address)
	}
}

func TestIsDocumentURL(t *testing.T) {
	cases := map[string]bool{
		"https://client.example/client.json": true,
		"https://client.example/":            false,
		"https://client.example":             false,
		"http://client.example/client.json":  false,
		"https://me@client.example/c.json":   false,
		"https://client.example/c.json#x":    false,
		"client-123":                         false,
		"https://client.example/%zz":         false,
		"https://client.example/c.json?v=1":  false,
		"https://client.example/c.json?":     false,
		"https://client.example/./c.json":    false,
		"https://client.example/a/../c.json": false,
		"https://client.example/a/%2e%2e/c":  false,
		"https://client.example/a..b/c.json": true,
	}
	for clientID, want := range cases {
		assert.Equal(t, want, isDocumentURL(clientID), clientID)
	}
}
