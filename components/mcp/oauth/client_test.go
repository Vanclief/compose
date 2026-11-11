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
	"unicode/utf8"

	"github.com/labstack/echo/v5"
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

func TestRedirectRegistered(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registered string
		presented  string
		want       bool
	}{
		{name: "exact https", registered: "https://claude.ai/api/mcp/auth_callback", presented: "https://claude.ai/api/mcp/auth_callback", want: true},
		{name: "exact loopback with port", registered: "http://127.0.0.1:3000/cb", presented: "http://127.0.0.1:3000/cb", want: true},
		{name: "localhost port added", registered: "http://localhost/callback", presented: "http://localhost:3118/callback", want: true},
		{name: "localhost port changed", registered: "http://localhost:3000/callback", presented: "http://localhost:3118/callback", want: true},
		{name: "localhost port dropped", registered: "http://localhost:3000/callback", presented: "http://localhost/callback", want: true},
		{name: "ipv4 loopback port added", registered: "http://127.0.0.1/callback", presented: "http://127.0.0.1:3118/callback", want: true},
		{name: "ipv6 loopback port added", registered: "http://[::1]/callback", presented: "http://[::1]:3118/callback", want: true},
		{name: "loopback same query", registered: "http://localhost/callback?app=1", presented: "http://localhost:3118/callback?app=1", want: true},
		{name: "loopback other path", registered: "http://localhost/callback", presented: "http://localhost:3118/other", want: false},
		{name: "loopback other query", registered: "http://localhost/callback?app=1", presented: "http://localhost:3118/callback?app=2", want: false},
		{name: "loopback other host", registered: "http://127.0.0.1/callback", presented: "http://localhost:3118/callback", want: false},
		{name: "loopback https", registered: "https://localhost/callback", presented: "https://localhost:3118/callback", want: false},
		{name: "loopback scheme changed", registered: "http://localhost/callback", presented: "https://localhost:3118/callback", want: false},
		{name: "loopback user info", registered: "http://localhost/callback", presented: "http://evil@localhost:3118/callback", want: false},
		{name: "loopback fragment", registered: "http://localhost/callback", presented: "http://localhost:3118/callback#x", want: false},
		{name: "loopback encoded path", registered: "http://localhost/callback", presented: "http://localhost:3118/%63allback", want: false},
		{name: "loopback same encoded slash", registered: "http://localhost/a%2Fb", presented: "http://localhost:3118/a%2Fb", want: true},
		{name: "loopback encoded slash", registered: "http://localhost/a/b", presented: "http://localhost:3118/a%2Fb", want: false},
		{name: "https port variant", registered: "https://claude.ai/api/mcp/auth_callback", presented: "https://claude.ai:8443/api/mcp/auth_callback", want: false},
		{name: "unparsable", registered: "http://localhost/callback", presented: "http://localhost:port/callback", want: false},
		{name: "empty", registered: "http://localhost/callback", presented: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := Client{ID: "c", RedirectURIs: []string{tc.registered}}
			assert.Equal(t, tc.want, redirectRegistered(client, tc.presented))
		})
	}
}

// TestRegisteredClientLifetime: a registration lives UnusedClientTTL, which
// each authorize extends to UnusedClientTTL and no further, until its first
// code exchange gives it a year; from then each authorize renews it for a
// year, as it does a client stored before the pending mark existed. Cached
// documents keep their 1h lifetime.
func TestRegisteredClientLifetime(t *testing.T) {
	ctx := context.Background()
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
	week := 7 * 24 * time.Hour
	year := 365 * 24 * time.Hour

	clientID := env.postJSON(t, "/oauth/register", `{"redirect_uris": ["`+testRedirect+`"]}`).json(t)["client_id"].(string)
	assert.InDelta(t, week.Seconds(), expiry(clientID).Seconds(), 60, "a registration nobody approved lives a week")

	// The record is moved an hour out, as it stands an hour before it expires.
	raw, err := store.Get(ctx, KindClient, clientID)
	require.NoError(t, err)
	require.NoError(t, store.Put(ctx, KindClient, clientID, raw, time.Now().Add(time.Hour)))
	verifier, challenge := pkce("l")
	env.startLogin(t, authorizeParams(clientID, challenge))
	assert.InDelta(t, week.Seconds(), expiry(clientID).Seconds(), 60, "authorize extends a pending client by a week only")

	code := env.codeFor(t, clientID, challenge)
	res := env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}})
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.InDelta(t, year.Seconds(), expiry(clientID).Seconds(), 60, "the code exchange gives the approved client a year")

	// An approved client an hour from its expiry is renewed by authorize.
	raw, err = store.Get(ctx, KindClient, clientID)
	require.NoError(t, err)
	require.NoError(t, store.Put(ctx, KindClient, clientID, raw, time.Now().Add(time.Hour)))
	env.startLogin(t, authorizeParams(clientID, challenge))
	assert.InDelta(t, year.Seconds(), expiry(clientID).Seconds(), 60, "authorize renews an approved client for a year")

	// A client stored before the pending mark existed is treated as approved.
	var legacy Client
	require.NoError(t, env.s.getRecord(ctx, KindClient, clientID, false, &legacy))
	require.NoError(t, env.s.putRecord(ctx, KindClient, clientID, legacy, time.Now().Add(time.Hour)))
	env.startLogin(t, authorizeParams(clientID, challenge))
	assert.InDelta(t, year.Seconds(), expiry(clientID).Seconds(), 60, "authorize renews a client stored before the upgrade for a year")

	documentID := host.add("/client.json", map[string]any{"client_name": "Hosted", "redirect_uris": []string{testRedirect}})
	env.startLogin(t, authorizeParams(documentID, challenge))
	assert.LessOrEqual(t, expiry(documentID), time.Hour, "documents stay cached for 1h only")

	env.startLogin(t, authorizeParams("public", challenge))
	store.mu.Lock()
	_, stored := store.records[memoryKey(KindClient, "public")]
	store.mu.Unlock()
	assert.False(t, stored, "configured clients are not stored")
}

// register registers a public client with testRedirect and returns its id.
func (env *testEnv) register(t *testing.T) string {
	t.Helper()
	res := env.postJSON(t, "/oauth/register", validRegistration)
	require.Equal(t, http.StatusCreated, res.status, res.body)
	return res.json(t)["client_id"].(string)
}

// storedCount is the number of records of kind in the memory store of env,
// expired ones included.
func (env *testEnv) storedCount(kind string) int {
	store := env.s.cfg.Store.(*memoryStore)
	store.mu.Lock()
	defer store.mu.Unlock()
	count := 0
	for key := range store.records {
		if strings.HasPrefix(key, kind+"\x00") {
			count++
		}
	}
	return count
}

// TestUnusedClientExpires: a registration no user approves is gone after
// UnusedClientTTL, while one approved within it lives on. Instead of
// sleeping, the client records are moved UnusedClientTTL and a minute into
// the past in the memory store, as if that much time had gone by.
func TestUnusedClientExpires(t *testing.T) {
	env := newEnv(t, nil, nil)
	unused := env.register(t)
	approved := env.register(t)

	verifier, challenge := pkce("u")
	code := env.codeFor(t, approved, challenge)
	res := env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {approved}, "code": {code}, "code_verifier": {verifier}})
	require.Equal(t, http.StatusOK, res.status, res.body)
	refresh := res.json(t)["refresh_token"].(string)

	store := env.s.cfg.Store.(*memoryStore)
	store.mu.Lock()
	for _, id := range []string{unused, approved} {
		key := memoryKey(KindClient, id)
		record := store.records[key]
		record.expiresAt = record.expiresAt.Add(-env.s.cfg.UnusedClientTTL - time.Minute)
		store.records[key] = record
	}
	store.mu.Unlock()

	res = env.authorize(t, authorizeParams(unused, challenge))
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Unknown application", "the unapproved registration expired")

	env.startLogin(t, authorizeParams(approved, challenge))
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {approved}, "refresh_token": {refresh}})
	assert.Equal(t, http.StatusOK, res.status, "the approved client outlives UnusedClientTTL: "+res.body)
}

// TestKeepClientAfterApproval: an authorize that read the client while it was
// pending does not undo a code exchange that approved it meanwhile.
func TestKeepClientAfterApproval(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t, nil, nil)
	clientID := env.register(t)
	pending, err := env.s.knownClient(ctx, clientID)
	require.NoError(t, err)
	require.NotNil(t, pending.pendingRecord, "a new registration is pending")

	require.NoError(t, env.s.renewClient(ctx, pending))
	require.NoError(t, env.s.keepClient(ctx, pending))

	approved, err := env.s.knownClient(ctx, clientID)
	require.NoError(t, err)
	assert.Nil(t, approved.pendingRecord, "the client stays approved")
	store := env.s.cfg.Store.(*memoryStore)
	store.mu.Lock()
	expiresAt := store.records[memoryKey(KindClient, clientID)].expiresAt
	store.mu.Unlock()
	assert.Greater(t, time.Until(expiresAt), 364*24*time.Hour, "the client keeps RegisteredClientTTL")
}

// TestLegacyClientNameIsCut: a client registered before names were bounded
// keeps working, and the pending request stores its name cut to
// maxClientNameBytes at a UTF-8 boundary.
func TestLegacyClientNameIsCut(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t, nil, nil)
	// Byte 512 falls inside an é, so the cut must step back to 511.
	name := "a" + strings.Repeat("é", 30<<10)
	legacy := Client{ID: "legacy-client-registered-before-the-bound-xx", Name: name, RedirectURIs: []string{testRedirect}}
	require.NoError(t, env.s.putRecord(ctx, KindClient, legacy.ID, legacy, time.Now().Add(time.Hour)))

	_, challenge := pkce("n")
	id := env.startLogin(t, authorizeParams(legacy.ID, challenge))
	var request authRequest
	require.NoError(t, env.s.getRecord(ctx, KindRequest, id, false, &request))
	assert.LessOrEqual(t, len(request.ClientName), maxClientNameBytes)
	assert.Equal(t, name[:511], request.ClientName)
	assert.True(t, utf8.ValidString(request.ClientName))
}

// TestLegacyLongRedirectURI: a client registered before redirect URIs were
// bounded can hold one longer than maxURLBytes. Authorize refuses it as not
// registered, whether sent or defaulted from the client's only URI, stores no
// request and does not renew the client.
func TestLegacyLongRedirectURI(t *testing.T) {
	ctx := context.Background()
	env := newEnv(t, nil, nil)
	long := testRedirect + "?" + strings.Repeat("q", 60<<10)
	legacy := Client{ID: "legacy-client-with-a-long-redirect-uri-xxxx", Name: "Legacy", RedirectURIs: []string{long}}
	expiresAt := time.Now().Add(time.Hour)
	require.NoError(t, env.s.putRecord(ctx, KindClient, legacy.ID, legacy, expiresAt))

	_, challenge := pkce("r")
	sent := authorizeParams(legacy.ID, challenge)
	sent.Set("redirect_uri", long)
	defaulted := authorizeParams(legacy.ID, challenge)
	defaulted.Del("redirect_uri")
	for name, params := range map[string]url.Values{"sent": sent, "defaulted": defaulted} {
		res := env.authorize(t, params)
		assert.Equal(t, http.StatusBadRequest, res.status, name)
		assert.Contains(t, res.body, "redirect URI is not registered", name)
	}
	assert.Zero(t, env.storedCount(KindRequest))
	store := env.s.cfg.Store.(*memoryStore)
	store.mu.Lock()
	stored := store.records[memoryKey(KindClient, legacy.ID)].expiresAt
	store.mu.Unlock()
	assert.True(t, stored.Equal(expiresAt), "the client is not renewed")
}

// TestRegisterBounds: a client_name or redirect URI longer than the limits is
// refused and nothing is stored; values at the limits are accepted.
func TestRegisterBounds(t *testing.T) {
	env := newEnv(t, nil, nil)
	name := strings.Repeat("n", maxClientNameBytes)
	uri := testRedirect + "?" + strings.Repeat("q", maxURLBytes-len(testRedirect)-1)

	res := env.postJSON(t, "/oauth/register", `{"client_name": "`+name+`n", "redirect_uris": ["`+testRedirect+`"]}`)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Equal(t, "invalid_client_metadata", oauthErrorCode(t, res))
	res = env.postJSON(t, "/oauth/register", `{"redirect_uris": ["`+uri+`q"]}`)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Equal(t, "invalid_redirect_uri", oauthErrorCode(t, res))
	assert.Zero(t, env.storedCount(KindClient))

	uris := func(count int) string {
		list := make([]string, count)
		for i := range list {
			list[i] = `"` + testRedirect + `"`
		}
		return strings.Join(list, ", ")
	}
	res = env.postJSON(t, "/oauth/register", `{"redirect_uris": [`+uris(maxRedirectURIs+1)+`]}`)
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Equal(t, "invalid_redirect_uri", oauthErrorCode(t, res))
	assert.Zero(t, env.storedCount(KindClient))

	res = env.postJSON(t, "/oauth/register", `{"client_name": "`+name+`", "redirect_uris": ["`+uri+`"]}`)
	assert.Equal(t, http.StatusCreated, res.status, res.body)
	res = env.postJSON(t, "/oauth/register", `{"redirect_uris": [`+uris(maxRedirectURIs)+`]}`)
	assert.Equal(t, http.StatusCreated, res.status, res.body)
}

// TestClientMetadataDocumentBounds: a document URL longer than maxURLBytes is
// an unknown client and is not fetched; a document with a client_name longer
// than maxClientNameBytes or more than maxRedirectURIs redirect URIs is an
// invalid one.
func TestClientMetadataDocumentBounds(t *testing.T) {
	host := newDocumentHost(t)
	env := newEnv(t, func(c *Config) {
		c.HTTPClient = host.server.Client()
	}, nil)
	_, challenge := pkce("b")

	res := env.authorize(t, authorizeParams(host.server.URL+"/"+strings.Repeat("a", maxURLBytes)+".json", challenge))
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Unknown application")
	assert.Equal(t, int32(0), host.fetches.Load())

	longName := host.add("/longname.json", map[string]any{"client_name": strings.Repeat("n", maxClientNameBytes+1), "redirect_uris": []string{testRedirect}})
	res = env.authorize(t, authorizeParams(longName, challenge))
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Invalid application")

	redirects := make([]string, maxRedirectURIs+1)
	for i := range redirects {
		redirects[i] = testRedirect
	}
	manyRedirects := host.add("/many.json", map[string]any{"client_name": "Many", "redirect_uris": redirects})
	res = env.authorize(t, authorizeParams(manyRedirects, challenge))
	assert.Equal(t, http.StatusBadRequest, res.status)
	assert.Contains(t, res.body, "Invalid application")
	assert.Contains(t, res.body, "more than 10 redirect URIs")
	assert.Zero(t, env.storedCount(KindClient))
	assert.Zero(t, env.storedCount(KindRequest))
}

// TestLongCachedDocumentIsUnknown: a document an earlier version cached under
// an id longer than maxURLBytes is an unknown client, documents on or off: at
// authorize and at the token endpoint, without a fetch and without its
// cached expiry being extended.
func TestLongCachedDocumentIsUnknown(t *testing.T) {
	for name, disable := range map[string]bool{"documents on": false, "documents off": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			host := newDocumentHost(t)
			env := newEnv(t, func(c *Config) {
				c.HTTPClient = host.server.Client()
				c.DisableClientMetadataDocuments = disable
			}, nil)
			path := "/" + strings.Repeat("a", 2100-len(host.server.URL)-6) + ".json"
			clientID := host.add(path, map[string]any{"client_name": "Long", "redirect_uris": []string{testRedirect}})
			require.Greater(t, len(clientID), maxURLBytes)
			// The record as the earlier version cached it: a plain Client.
			cached := Client{ID: clientID, Name: "Long", RedirectURIs: []string{testRedirect}}
			expiresAt := time.Now().Add(time.Hour)
			require.NoError(t, env.s.putRecord(ctx, KindClient, clientID, cached, expiresAt))

			_, challenge := pkce("w")
			res := env.authorize(t, authorizeParams(clientID, challenge))
			assert.Equal(t, http.StatusBadRequest, res.status)
			assert.Contains(t, res.body, "Unknown application")

			res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {shapedRefreshToken(t)}})
			assert.Equal(t, "invalid_client", oauthErrorCode(t, res))

			assert.Equal(t, int32(0), host.fetches.Load())
			assert.Zero(t, env.storedCount(KindRequest))
			store := env.s.cfg.Store.(*memoryStore)
			store.mu.Lock()
			stored := store.records[memoryKey(KindClient, clientID)].expiresAt
			store.mu.Unlock()
			assert.True(t, stored.Equal(expiresAt), "the cached expiry is not extended")
		})
	}
}

// TestDisableClientMetadataDocuments: with the switch set the metadata says
// documents are not supported, and a document client_id is an unknown client
// at authorize and at the token endpoint, cached or not, without a fetch.
// A client listed in Config.Clients keeps working whatever its id.
func TestDisableClientMetadataDocuments(t *testing.T) {
	ctx := context.Background()
	host := newDocumentHost(t)
	clientID := host.add("/client.json", map[string]any{"client_name": "Hosted Client", "redirect_uris": []string{testRedirect}})
	cachedID := host.add("/cached.json", map[string]any{"client_name": "Cached Client", "redirect_uris": []string{testRedirect}})
	listedID := "https://client.example/listed.json"
	env := newEnv(t, func(c *Config) {
		c.HTTPClient = host.server.Client()
		c.DisableClientMetadataDocuments = true
		c.Clients = append(c.Clients, Client{ID: listedID, Name: "Listed", RedirectURIs: []string{testRedirect}})
	}, nil)
	// A document cached before the switch was set.
	cached := Client{ID: cachedID, Name: "Cached Client", RedirectURIs: []string{testRedirect}}
	require.NoError(t, env.s.putRecord(ctx, KindClient, cachedID, cached, time.Now().Add(time.Hour)))

	res := env.get(t, "/.well-known/oauth-authorization-server")
	require.Equal(t, http.StatusOK, res.status)
	assert.Equal(t, false, res.json(t)["client_id_metadata_document_supported"])

	verifier, challenge := pkce("d")
	for _, id := range []string{clientID, cachedID} {
		res = env.authorize(t, authorizeParams(id, challenge))
		assert.Equal(t, http.StatusBadRequest, res.status, id)
		assert.Contains(t, res.body, "Unknown application", id)

		res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {shapedRefreshToken(t)}})
		assert.Equal(t, "invalid_client", oauthErrorCode(t, res), id)
	}
	assert.Equal(t, int32(0), host.fetches.Load())
	assert.Zero(t, env.storedCount(KindRequest))

	code := env.codeFor(t, listedID, challenge)
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {listedID}, "code": {code}, "code_verifier": {verifier}})
	assert.Equal(t, http.StatusOK, res.status, res.body)
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
	s, err := New(Config{Issuer: "https://api.acme.com", Resource: "https://api.acme.com/mcp", TokenPrefix: "acme", Store: NewMemoryStore(), LoginURL: "https://app.acme.com/login"})
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
		// No length bound: the shape alone makes an id a document, so the
		// switch and the renewal skip hold for long ids too.
		"https://client.example/" + strings.Repeat("a", maxURLBytes): true,
	}
	for clientID, want := range cases {
		assert.Equal(t, want, isDocumentURL(clientID), clientID)
	}
}

// TestTokenRequestsRenewRegisteredClient: a successful token request renews
// a dynamically registered client, so a client that keeps refreshing under
// RefreshTokenTTL Never outlives RegisteredClientTTL; one that stops
// refreshing still expires.
func TestTokenRequestsRenewRegisteredClient(t *testing.T) {
	ctx := context.Background()
	ttl := 600 * time.Millisecond
	env := newEnv(t, func(c *Config) {
		c.RegisteredClientTTL = ttl
		c.RefreshTokenTTL = Never
	}, nil)
	clientID := env.postJSON(t, "/oauth/register", `{"client_name": "Claude", "redirect_uris": ["`+testRedirect+`"], "token_endpoint_auth_method": "client_secret_post"}`).json(t)["client_id"].(string)
	var registered Client
	require.NoError(t, env.s.getRecord(ctx, KindClient, clientID, false, &registered))
	refreshWith := func(refresh string) response {
		return env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "client_secret": {registered.Secret}, "refresh_token": {refresh}})
	}

	verifier, challenge := pkce("n")
	code := env.codeFor(t, clientID, challenge)
	res := env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "client_secret": {registered.Secret}, "code": {code}, "code_verifier": {verifier}})
	require.Equal(t, http.StatusOK, res.status, res.body)

	// Each refresh lands before the registration renewed by the previous
	// token request expires; the second lands after the expiry the code
	// exchange set.
	time.Sleep(ttl * 3 / 5)
	res = refreshWith(res.json(t)["refresh_token"].(string))
	require.Equal(t, http.StatusOK, res.status, res.body)
	time.Sleep(ttl * 3 / 5)
	res = refreshWith(res.json(t)["refresh_token"].(string))
	require.Equal(t, http.StatusOK, res.status, "the first refresh renewed the registration: "+res.body)

	var renewed Client
	require.NoError(t, env.s.getRecord(ctx, KindClient, clientID, false, &renewed))
	assert.Equal(t, registered, renewed, "the renewal stores the full client")

	time.Sleep(ttl + 100*time.Millisecond)
	res = refreshWith(res.json(t)["refresh_token"].(string))
	assert.Equal(t, "invalid_client", oauthErrorCode(t, res), "a client that stops refreshing expires")
}

// TestTokenRequestsRenewClientBeforeTokens: a successful code exchange and a
// successful refresh each renew a dynamically registered client, and a
// renewal that fails is reported while the token request still succeeds.
func TestTokenRequestsRenewClientBeforeTokens(t *testing.T) {
	ctx := context.Background()
	env := newHookEnv(t, nil)
	clientID := env.postJSON(t, "/oauth/register", `{"redirect_uris": ["`+testRedirect+`"]}`).json(t)["client_id"].(string)
	var client Client
	require.NoError(t, env.s.getRecord(ctx, KindClient, clientID, false, &client))
	year := 365 * 24 * time.Hour
	// shorten moves the client's expiry an hour out, and with failRenewal
	// fails the next write of the client.
	shorten := func(failRenewal bool) {
		t.Helper()
		require.NoError(t, env.s.putRecord(ctx, KindClient, clientID, client, time.Now().Add(time.Hour)))
		if failRenewal {
			env.hooks.before("Put "+KindClient, storeDown)
		}
	}
	renewed := func(t *testing.T) bool {
		t.Helper()
		return time.Until(env.expiry(t, KindClient, clientID)) > year-time.Minute
	}
	exchange := func(failRenewal bool) response {
		verifier, challenge := pkce("r")
		code := env.codeFor(t, clientID, challenge)
		// The authorize step renews or extends the client too, so the
		// client is shortened after it: only the exchange can renew it from
		// here.
		shorten(failRenewal)
		return env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "code": {code}, "code_verifier": {verifier}})
	}
	refreshWith := func(refresh string, failRenewal bool) response {
		shorten(failRenewal)
		return env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refresh}})
	}

	res := exchange(false)
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.True(t, renewed(t), "the code exchange renews the client")
	res = refreshWith(res.json(t)["refresh_token"].(string), false)
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.True(t, renewed(t), "the refresh renews the client")
	env.mu.Lock()
	assert.Empty(t, env.reported)
	env.mu.Unlock()

	res = exchange(true)
	require.Equal(t, http.StatusOK, res.status, "a failed renewal does not fail the exchange: "+res.body)
	assert.False(t, renewed(t))
	res = refreshWith(res.json(t)["refresh_token"].(string), true)
	require.Equal(t, http.StatusOK, res.status, "a failed renewal does not fail the refresh: "+res.body)
	assert.False(t, renewed(t))

	env.mu.Lock()
	defer env.mu.Unlock()
	require.Len(t, env.reported, 2, "each failed renewal reaches OnError")
	for _, err := range env.reported {
		assert.Contains(t, err.Error(), "store is down")
	}
}

// TestTokenRequestsDoNotStoreOtherClients: the renewal leaves clients from
// Config.Clients and Client ID Metadata Documents out of the Store.
func TestTokenRequestsDoNotStoreOtherClients(t *testing.T) {
	ctx := context.Background()
	host := newDocumentHost(t)
	documentID := host.add("/client.json", map[string]any{"client_name": "Hosted Client", "redirect_uris": []string{testRedirect}})
	env := newEnv(t, func(c *Config) {
		c.HTTPClient = host.server.Client()
	}, nil)
	stored := func(id string) bool {
		_, err := env.s.cfg.Store.Get(ctx, KindClient, id)
		return err == nil
	}
	refreshWith := func(clientID, refresh string) response {
		return env.postForm(t, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refresh}})
	}

	_, refresh := env.tokens(t)
	res := refreshWith("public", refresh)
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.False(t, stored("public"), "configured clients are not stored")

	verifier, challenge := pkce("o")
	code := env.codeFor(t, documentID, challenge)
	// The cached document is dropped, so any later write is the renewal's.
	require.NoError(t, env.s.cfg.Store.Delete(ctx, KindClient, documentID))
	res = env.postForm(t, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {documentID}, "code": {code}, "code_verifier": {verifier}})
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.False(t, stored(documentID), "the code exchange does not store a document client")
	res = refreshWith(documentID, res.json(t)["refresh_token"].(string))
	require.Equal(t, http.StatusOK, res.status, res.body)
	assert.False(t, stored(documentID), "the refresh does not store a document client")
}

// validRegistration is a registration body the server accepts.
const validRegistration = `{"redirect_uris": ["` + testRedirect + `"]}`

// newLimitServer builds a server for the registration limit tests on store,
// with mutate adjusting the config.
func newLimitServer(t *testing.T, store Store, mutate func(*Config)) *Server {
	t.Helper()
	cfg := Config{Issuer: "https://api.acme.com", Resource: "https://api.acme.com/mcp", TokenPrefix: "acme", Store: store, LoginURL: "https://app.acme.com/login"}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := New(cfg)
	require.NoError(t, err)
	return s
}

// mountLimitServer mounts s on a new Echo app and returns the app.
func mountLimitServer(s *Server) *echo.Echo {
	e := echo.New()
	s.Mount(e, http.NotFoundHandler())
	return e
}

// sendRegistration sends a method request with body to the registration
// endpoint of h from remoteAddr, with header X-Client set to client when it
// is not empty.
func sendRegistration(h http.Handler, method, remoteAddr, client, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/oauth/register", strings.NewReader(body))
	req.RemoteAddr = remoteAddr
	if client != "" {
		req.Header.Set("X-Client", client)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// register posts validRegistration to h from remoteAddr and returns the
// status.
func register(h http.Handler, remoteAddr, client string) int {
	return sendRegistration(h, http.MethodPost, remoteAddr, client, validRegistration).Code
}

// clientHeader is an IP extractor that reads the X-Client header.
func clientHeader(r *http.Request) string {
	return r.Header.Get("X-Client")
}

// TestRegistrationLimit: under the default limit the 21st registration from
// one address within the minute is refused with 429, Retry-After and the
// OAuth error body, and stores no client, while another address still
// registers.
func TestRegistrationLimit(t *testing.T) {
	store := newHookStore(NewMemoryStore())
	s := newLimitServer(t, store, nil)
	e := mountLimitServer(s)

	for i := 0; i < defaultRegistrationLimit; i++ {
		require.Equal(t, http.StatusCreated, register(e, "192.0.2.1:1000", ""), i)
	}
	stored := false
	store.before("Put client", func() error {
		stored = true
		return nil
	})
	rec := sendRegistration(e, http.MethodPost, "192.0.2.1:1000", "", validRegistration)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "60", rec.Header().Get("Retry-After"))
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var body oauthError
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	assert.Equal(t, "temporarily_unavailable", body.Error)
	assert.NotEmpty(t, body.Description)
	assert.False(t, stored, "a refused registration stored a client")

	assert.Equal(t, http.StatusCreated, register(e, "198.51.100.7:1000", ""))
	assert.True(t, stored)
}

// TestRegistrationLimitIPv6Prefix: IPv6 addresses in one /64 share a count,
// another /64 and an IPv4-mapped address do not.
func TestRegistrationLimitIPv6Prefix(t *testing.T) {
	s := newLimitServer(t, NewMemoryStore(), func(c *Config) { c.RegistrationLimit = 2 })
	h := s.Handler()
	require.Equal(t, http.StatusCreated, register(h, "[2001:db8:1:2::1]:1000", ""))
	require.Equal(t, http.StatusCreated, register(h, "[2001:db8:1:2:ffff::9]:1000", ""))
	assert.Equal(t, http.StatusTooManyRequests, register(h, "[2001:db8:1:2:1234::1]:1000", ""))
	assert.Equal(t, http.StatusCreated, register(h, "[2001:db8:1:3::1]:1000", ""))
	assert.Equal(t, http.StatusCreated, register(h, "[::ffff:192.0.2.1]:1000", ""))
}

func TestRegistrationKey(t *testing.T) {
	tests := map[string]string{
		"2001:db8:1:2:3:4:5:6": "2001:db8:1:2::/64",
		"2001:db8::1":          "2001:db8::/64",
		"::ffff:192.0.2.1":     "::ffff:192.0.2.1",
		"192.0.2.1":            "192.0.2.1",
		"not an address":       "not an address",
		"":                     "",
	}
	for address, want := range tests {
		assert.Equal(t, want, registrationKey(address), address)
	}
}

// TestRegistrationLimitChargesMalformed: a registration with a malformed body
// counts against the limit.
func TestRegistrationLimitChargesMalformed(t *testing.T) {
	s := newLimitServer(t, NewMemoryStore(), func(c *Config) { c.RegistrationLimit = 2 })
	h := s.Handler()
	for i := 0; i < 2; i++ {
		rec := sendRegistration(h, http.MethodPost, "192.0.2.1:1000", "", "not json")
		require.Equal(t, http.StatusBadRequest, rec.Code)
	}
	assert.Equal(t, http.StatusTooManyRequests, register(h, "192.0.2.1:1000", ""))
}

// TestRegistrationLimitSkipsPreflight: the CORS preflight of the registration
// endpoint does not count against the limit.
func TestRegistrationLimitSkipsPreflight(t *testing.T) {
	s := newLimitServer(t, NewMemoryStore(), func(c *Config) { c.RegistrationLimit = 2 })
	h := s.Handler()
	for i := 0; i < 5; i++ {
		rec := sendRegistration(h, http.MethodOptions, "192.0.2.1:1000", "", "")
		require.Equal(t, http.StatusNoContent, rec.Code)
	}
	assert.Equal(t, http.StatusCreated, register(h, "192.0.2.1:1000", ""))
	assert.Equal(t, http.StatusCreated, register(h, "192.0.2.1:1000", ""))
	assert.Equal(t, http.StatusTooManyRequests, register(h, "192.0.2.1:1000", ""))
}

// TestRegistrationLimitOff: a negative RegistrationLimit lets every
// registration through.
func TestRegistrationLimitOff(t *testing.T) {
	s := newLimitServer(t, NewMemoryStore(), func(c *Config) { c.RegistrationLimit = -1 })
	h := s.Handler()
	for i := 0; i < 2*defaultRegistrationLimit; i++ {
		require.Equal(t, http.StatusCreated, register(h, "192.0.2.1:1000", ""), i)
	}
}

// TestRegistrationLimitClientIP: Config.ClientIP decides the address a
// registration is counted against, under Handler and under Mount, where it
// takes precedence over the Echo IPExtractor.
func TestRegistrationLimitClientIP(t *testing.T) {
	builds := map[string]func(*Server) http.Handler{
		"Handler": func(s *Server) http.Handler { return s.Handler() },
		"Mount": func(s *Server) http.Handler {
			e := mountLimitServer(s)
			e.IPExtractor = echo.ExtractIPDirect()
			return e
		},
	}
	for name, build := range builds {
		t.Run(name, func(t *testing.T) {
			s := newLimitServer(t, NewMemoryStore(), func(c *Config) {
				c.RegistrationLimit = 1
				c.ClientIP = clientHeader
			})
			h := build(s)
			assert.Equal(t, http.StatusCreated, register(h, "192.0.2.1:1000", "a"))
			assert.Equal(t, http.StatusTooManyRequests, register(h, "198.51.100.7:1000", "a"))
			assert.Equal(t, http.StatusCreated, register(h, "192.0.2.1:1000", "b"))
		})
	}
}

// TestRegistrationLimitMountKey: under Mount without Config.ClientIP the Echo
// IPExtractor decides the address, also when it is set after Mount, and
// without one the host of RemoteAddr does.
func TestRegistrationLimitMountKey(t *testing.T) {
	t.Run("IPExtractor", func(t *testing.T) {
		s := newLimitServer(t, NewMemoryStore(), func(c *Config) { c.RegistrationLimit = 1 })
		e := mountLimitServer(s)
		e.IPExtractor = clientHeader
		assert.Equal(t, http.StatusCreated, register(e, "192.0.2.1:1000", "a"))
		assert.Equal(t, http.StatusTooManyRequests, register(e, "198.51.100.7:1000", "a"))
		assert.Equal(t, http.StatusCreated, register(e, "192.0.2.1:1000", "b"))
	})
	t.Run("RemoteAddr", func(t *testing.T) {
		s := newLimitServer(t, NewMemoryStore(), func(c *Config) { c.RegistrationLimit = 1 })
		e := mountLimitServer(s)
		assert.Equal(t, http.StatusCreated, register(e, "192.0.2.1:1000", "a"))
		assert.Equal(t, http.StatusTooManyRequests, register(e, "192.0.2.1:2000", "b"))
		assert.Equal(t, http.StatusCreated, register(e, "198.51.100.7:1000", "a"))
	})
}

// TestRegistrationLimitHandlerKey: through Handler without Config.ClientIP
// the host of RemoteAddr decides the address, whatever the port, and a
// RemoteAddr without a port counts whole.
func TestRegistrationLimitHandlerKey(t *testing.T) {
	s := newLimitServer(t, NewMemoryStore(), func(c *Config) { c.RegistrationLimit = 1 })
	h := s.Handler()
	assert.Equal(t, http.StatusCreated, register(h, "192.0.2.1:1000", "a"))
	assert.Equal(t, http.StatusTooManyRequests, register(h, "192.0.2.1:2000", "b"))
	assert.Equal(t, http.StatusCreated, register(h, "198.51.100.7:1000", ""))
	assert.Equal(t, http.StatusCreated, register(h, "203.0.113.5", ""))
	assert.Equal(t, http.StatusTooManyRequests, register(h, "203.0.113.5", ""))
}
