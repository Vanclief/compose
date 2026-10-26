package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

const (
	testKID     = "test-key"
	templateKID = "template-key" // issuer: the Microsoft {tenantid} template
	workKID     = "work-key"     // issuer: workTenant's issuer
	secondKID   = "second-key"   // no issuer, survives the rotation
	rotatedKID  = "rotated-key"  // otherKey, published by the rotation only
	workTenant  = "72f988bf-86f1-41af-91ab-2d7cd011db47"
	testNonce   = "nonce-123"
)

var (
	signingKey = mustRSAKey()
	otherKey   = mustRSAKey()
	// testJWKS publishes signingKey under three kids, two of them limited to
	// an issuer the way Microsoft's key documents do.
	testJWKS = map[string]any{"keys": []any{
		publicJWK(signingKey, testKID, ""),
		publicJWK(signingKey, templateKID, microsoftTemplate),
		publicJWK(signingKey, workKID, "https://login.microsoftonline.com/"+workTenant+"/v2.0"),
		publicJWK(signingKey, secondKID, ""),
	}}
	// rotatedJWKS is testJWKS after a rotation: testKID is gone and
	// rotatedKID, a different key, is new.
	rotatedJWKS = map[string]any{"keys": []any{
		publicJWK(signingKey, templateKID, microsoftTemplate),
		publicJWK(signingKey, workKID, "https://login.microsoftonline.com/"+workTenant+"/v2.0"),
		publicJWK(signingKey, secondKID, ""),
		publicJWK(otherKey, rotatedKID, ""),
	}}
	// oddJWKS is testJWKS plus two entries that cannot sign: an encryption
	// key and one whose modulus does not decode.
	oddJWKS = map[string]any{"keys": []any{
		publicJWK(signingKey, testKID, ""),
		encryptionJWK(),
		map[string]any{"kty": "RSA", "kid": "bad-n", "n": "!!!", "e": "AQAB"},
	}}
)

func encryptionJWK() map[string]any {
	jwk := publicJWK(signingKey, "enc-only", "")
	jwk["use"] = "enc"

	return jwk
}

// publicJWK returns key's public half as a JWKS entry, with an "issuer"
// field when issuer is not empty.
func publicJWK(key *rsa.PrivateKey, kid, issuer string) map[string]any {
	raw, err := json.Marshal(jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Algorithm: "RS256", Use: "sig"})
	if err != nil {
		panic(err)
	}
	var entry map[string]any
	err = json.Unmarshal(raw, &entry)
	if err != nil {
		panic(err)
	}
	if issuer != "" {
		entry["issuer"] = issuer
	}

	return entry
}

func mustRSAKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

// fakeIssuer serves a discovery document and a JWKS holding signingKey.
type fakeIssuer struct {
	server *httptest.Server
	// published is the issuer the document announces, the server URL when empty.
	published string
	// keysBase is where the document says the keys are, the server URL when empty.
	keysBase    string
	release     chan struct{} // closed to let blocked requests answer
	hits        atomic.Int32  // discovery requests
	keysHits    atomic.Int32  // key set requests
	down        atomic.Bool   // answer 503 to everything
	blocked     atomic.Bool   // hold discovery requests until release is closed
	keysBlocked atomic.Bool   // hold key set requests until release is closed
	stalled     atomic.Bool   // answer discovery with 200 and hold the body until release is closed
	keysStalled atomic.Bool   // answer key set requests with 200 and hold the body until release is closed
	noJWKS      atomic.Bool   // leave jwks_uri out of the document
	keysCode    atomic.Int32  // answer /keys with this status when not zero
	keysJunk    atomic.Bool   // answer /keys with a malformed key set
	keysEmpty   atomic.Bool   // answer /keys with a set holding no usable key
	keysRotated atomic.Bool   // answer /keys with rotatedJWKS
	keysOdd     atomic.Bool   // answer /keys with oddJWKS
	keysMoved   atomic.Bool   // publish the keys at /keys-moved and answer /keys with 404
}

// startIssuer starts a fake issuer on addr ("" picks a free port).
func startIssuer(t *testing.T, addr, published string) *fakeIssuer {
	t.Helper()

	f := &fakeIssuer{published: published}
	f.start(t, addr)

	return f
}

// startPublicIssuer starts a fake issuer that announces a public issuer URL
// and its keys under it. Reach it through clientTo.
func startPublicIssuer(t *testing.T, published string) *fakeIssuer {
	t.Helper()

	f := &fakeIssuer{published: published, keysBase: published}
	f.start(t, "")

	return f
}

func (f *fakeIssuer) start(t *testing.T, addr string) {
	t.Helper()

	if addr == "" {
		addr = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err)

	f.release = make(chan struct{})
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.server.Listener.Close()
	f.server.Listener = listener
	f.server.Start()
	t.Cleanup(f.server.Close)
}

// serve matches paths by suffix, so an issuer with a path such as
// Microsoft's "/<tenant>/v2.0" works too.
func (f *fakeIssuer) serve(w http.ResponseWriter, r *http.Request) {
	discovery := strings.HasSuffix(r.URL.Path, "/.well-known/openid-configuration")
	keys := strings.HasSuffix(r.URL.Path, "/keys")
	moved := strings.HasSuffix(r.URL.Path, "/keys-moved")
	if keys || moved {
		f.keysHits.Add(1)
	}
	if f.keysMoved.Load() {
		// Only the new path serves keys; the old one falls through to 404.
		keys = moved
	}
	blocked := false
	if discovery {
		f.hits.Add(1)
		blocked = f.blocked.Load()
	}
	if keys {
		blocked = f.keysBlocked.Load()
	}
	if blocked {
		select {
		case <-f.release:
		case <-r.Context().Done():
			return
		}
	}
	if f.down.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	var body any
	switch {
	case discovery:
		issuer := f.published
		if issuer == "" {
			issuer = f.server.URL
		}
		keysBase := f.keysBase
		if keysBase == "" {
			keysBase = f.server.URL
		}
		keysPath := "/keys"
		if f.keysMoved.Load() {
			keysPath = "/keys-moved"
		}
		document := map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                f.server.URL + "/authorize",
			"token_endpoint":                        f.server.URL + "/token",
			"jwks_uri":                              keysBase + keysPath,
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		if f.noJWKS.Load() {
			delete(document, "jwks_uri")
		}
		body = document
	case keys:
		code := f.keysCode.Load()
		if code != 0 {
			w.WriteHeader(int(code))
			return
		}
		if f.keysJunk.Load() {
			body = map[string]any{"keys": "nope"}
			break
		}
		if f.keysEmpty.Load() {
			body = map[string]any{"keys": []map[string]any{{"kty": "RSA", "use": "enc", "kid": "enc-only"}}}
			break
		}
		body = testJWKS
		if f.keysRotated.Load() {
			body = rotatedJWKS
		}
		if f.keysOdd.Load() {
			body = oddJWKS
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if (discovery && f.stalled.Load()) || (keys && f.keysStalled.Load()) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-f.release:
		case <-r.Context().Done():
			return
		}
	}
	err := json.NewEncoder(w).Encode(body)
	if err != nil {
		panic(err)
	}
}

// rewriteTransport sends every request to target, whatever its URL says.
type rewriteTransport struct {
	target *url.URL
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	clone.Host = ""

	return http.DefaultTransport.RoundTrip(clone)
}

// clientTo returns an HTTP client that reaches f whatever URL it is asked for.
func clientTo(t *testing.T, f *fakeIssuer) *http.Client {
	t.Helper()

	target, err := url.Parse(f.server.URL)
	require.NoError(t, err)

	return &http.Client{Transport: rewriteTransport{target: target}}
}

func mint(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()

	return mintWith(t, jose.RS256, key, kid, claims)
}

func mintWith(t *testing.T, alg jose.SignatureAlgorithm, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()

	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: alg,
		Key:       jose.JSONWebKey{Key: key, KeyID: kid},
	}, nil)
	require.NoError(t, err)

	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)

	return raw
}

// claimsFor returns valid claims for issuer and audience, overridden by extra.
func claimsFor(issuer string, audience any, extra map[string]any) map[string]any {
	now := time.Now()
	claims := map[string]any{
		"iss":   issuer,
		"aud":   audience,
		"sub":   "user-1",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"nonce": testNonce,
	}
	maps.Copy(claims, extra)

	return claims
}

func newVerifier(t *testing.T, provider Provider, cfg ProviderConfig) *Verifier {
	t.Helper()

	var config Config
	switch provider {
	case Google:
		config.Google = cfg
	case Apple:
		config.Apple = cfg
	case Microsoft:
		config.Microsoft = cfg
	}
	v, err := New(config)
	require.NoError(t, err)

	return v
}

func TestVerifyGoogle(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"ios", "web"}, Issuer: f.server.URL})

	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, []string{"other", "web"}, map[string]any{
		"email":          "ana@example.com",
		"email_verified": true,
		"hd":             "example.com",
	}))
	identity, err := v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, Identity{
		Provider:      Google,
		Subject:       "user-1",
		Email:         "ana@example.com",
		EmailVerified: true,
		HostedDomain:  "example.com",
		Issuer:        f.server.URL,
		Audience:      "web",
	}, identity)
}

func TestVerifyGoogleIssuerWithoutScheme(t *testing.T) {
	f := startPublicIssuer(t, googleIssuer)
	v, err := New(Config{
		Google:     ProviderConfig{ClientIDs: []string{"web"}},
		HTTPClient: clientTo(t, f),
	})
	require.NoError(t, err)

	for _, issuer := range []string{"https://accounts.google.com", "accounts.google.com"} {
		token := mint(t, signingKey, testKID, claimsFor(issuer, "web", nil))
		identity, err := v.Verify(context.Background(), Google, token, testNonce)
		require.NoError(t, err, issuer)
		assert.Equal(t, issuer, identity.Issuer)
	}

	for _, issuer := range []string{"https://accounts.example.com", "http://accounts.google.com", "accounts.google.com/"} {
		token := mint(t, signingKey, testKID, claimsFor(issuer, "web", nil))
		_, err := v.Verify(context.Background(), Google, token, testNonce)
		require.Error(t, err, issuer)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), issuer)
	}
}

func TestVerifyApple(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Apple, ProviderConfig{ClientIDs: []string{"com.example.app"}, Issuer: f.server.URL})

	cases := []struct {
		name         string
		verified     any
		private      any
		wantVerified bool
		wantPrivate  bool
	}{
		{"strings true", "true", "true", true, true},
		{"booleans true", true, true, true, true},
		{"strings false", "false", "false", false, false},
		{"booleans false", false, false, false, false},
		{"mixed", "true", false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "com.example.app", map[string]any{
				"email":            "relay@privaterelay.appleid.com",
				"email_verified":   c.verified,
				"is_private_email": c.private,
				"hd":               "ignored.example",
			}))
			identity, err := v.Verify(context.Background(), Apple, token, testNonce)
			require.NoError(t, err)
			assert.Equal(t, Identity{
				Provider:      Apple,
				Subject:       "user-1",
				Email:         "relay@privaterelay.appleid.com",
				EmailVerified: c.wantVerified,
				PrivateEmail:  c.wantPrivate,
				Issuer:        f.server.URL,
				Audience:      "com.example.app",
			}, identity)
		})
	}
}

// microsoftVerifier returns a Verifier for the multi-tenant Microsoft tenant
// whose discovery reaches a fake that publishes published as its issuer.
func microsoftVerifier(t *testing.T, tenant, published string) (*Verifier, *fakeIssuer) {
	t.Helper()

	f := startPublicIssuer(t, published)
	v, err := New(Config{
		Microsoft:  ProviderConfig{ClientIDs: []string{"ms-app"}, Tenant: tenant},
		HTTPClient: clientTo(t, f),
	})
	require.NoError(t, err)

	return v, f
}

func TestVerifyMicrosoftCommon(t *testing.T) {
	v, f := microsoftVerifier(t, "", microsoftTemplate)
	tenantIssuer := "https://login.microsoftonline.com/" + workTenant + "/v2.0"

	token := mint(t, signingKey, testKID, claimsFor(tenantIssuer, "ms-app", map[string]any{
		"tid":            workTenant,
		"email":          "ana@contoso.com",
		"email_verified": true,
	}))
	identity, err := v.Verify(context.Background(), Microsoft, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, Identity{
		Provider: Microsoft,
		Subject:  "user-1",
		Email:    "ana@contoso.com",
		Issuer:   tenantIssuer,
		Audience: "ms-app",
	}, identity)

	token = mint(t, signingKey, testKID, claimsFor(tenantIssuer, "ms-app", map[string]any{
		"tid":                workTenant,
		"preferred_username": "ana@contoso.com",
	}))
	identity, err = v.Verify(context.Background(), Microsoft, token, testNonce)
	require.NoError(t, err)
	assert.Empty(t, identity.Email)

	refused := []struct {
		name   string
		issuer string
		tid    string
	}{
		{"mismatched tid", tenantIssuer, "00000000-0000-0000-0000-000000000000"},
		{"missing tid", tenantIssuer, ""},
		{"wrong host", "https://login.example.com/" + workTenant + "/v2.0", workTenant},
		{"template issuer", microsoftTemplate, workTenant},
		{"discovery issuer", f.server.URL, workTenant},
	}
	for _, c := range refused {
		token := mint(t, signingKey, testKID, claimsFor(c.issuer, "ms-app", map[string]any{"tid": c.tid}))
		_, err := v.Verify(context.Background(), Microsoft, token, testNonce)
		require.Error(t, err, c.name)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), c.name)
	}
}

func TestVerifyMicrosoftAccountKinds(t *testing.T) {
	consumersIssuer := "https://login.microsoftonline.com/" + consumersTenant + "/v2.0"
	cases := []struct {
		name      string
		tenant    string
		published string
		tid       string
		wantCode  string
	}{
		{"consumers accepts personal", "consumers", consumersIssuer, consumersTenant, ""},
		{"consumers refuses work", "consumers", consumersIssuer, workTenant, ez.ENOTAUTHENTICATED},
		{"organizations accepts work", "organizations", microsoftTemplate, workTenant, ""},
		{"organizations refuses personal", "organizations", microsoftTemplate, consumersTenant, ez.ENOTAUTHENTICATED},
		{"organizations refuses uppercase personal", "organizations", microsoftTemplate, strings.ToUpper(consumersTenant), ez.ENOTAUTHENTICATED},
		{"common accepts personal", "common", microsoftTemplate, consumersTenant, ""},
	}
	for _, c := range cases {
		v, _ := microsoftVerifier(t, c.tenant, c.published)
		issuer := "https://login.microsoftonline.com/" + c.tid + "/v2.0"
		token := mint(t, signingKey, testKID, claimsFor(issuer, "ms-app", map[string]any{"tid": c.tid}))

		_, err := v.Verify(context.Background(), Microsoft, token, testNonce)
		if c.wantCode == "" {
			require.NoError(t, err, c.name)
			continue
		}
		require.Error(t, err, c.name)
		assert.Equal(t, c.wantCode, ez.ErrorCode(err), c.name)
	}
}

func TestVerifyMicrosoftRefusesAccessToken(t *testing.T) {
	f := startPublicIssuer(t, microsoftTemplate)
	v, err := New(Config{
		Microsoft:  ProviderConfig{ClientIDs: []string{"ms-app"}, AllowMissingNonce: true},
		HTTPClient: clientTo(t, f),
	})
	require.NoError(t, err)
	tenantIssuer := "https://login.microsoftonline.com/" + workTenant + "/v2.0"

	// An access token for an API the app exposes: same keys, issuer and
	// audience as an ID token, no nonce, scp present.
	access := mint(t, signingKey, testKID, claimsFor(tenantIssuer, "ms-app", map[string]any{
		"tid":   workTenant,
		"scp":   "access_as_user",
		"azp":   "third-party-app",
		"nonce": nil,
	}))
	_, err = v.Verify(context.Background(), Microsoft, access, "")
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))

	// A client-credentials token: no scp and no user, sub equals the service
	// principal's oid. idtyp is the explicit marker when configured.
	appOnly := mint(t, signingKey, testKID, claimsFor(tenantIssuer, "ms-app", map[string]any{
		"tid":   workTenant,
		"sub":   "sp-object-id",
		"oid":   "sp-object-id",
		"roles": []string{"Data.Read"},
		"nonce": nil,
	}))
	_, err = v.Verify(context.Background(), Microsoft, appOnly, "")
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))

	marked := mint(t, signingKey, testKID, claimsFor(tenantIssuer, "ms-app", map[string]any{
		"tid":   workTenant,
		"oid":   "sp-object-id",
		"idtyp": "app",
		"nonce": nil,
	}))
	_, err = v.Verify(context.Background(), Microsoft, marked, "")
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))

	// A user's ID token carries its own oid and may carry app roles.
	idToken := mint(t, signingKey, testKID, claimsFor(tenantIssuer, "ms-app", map[string]any{
		"tid":   workTenant,
		"oid":   "user-object-id",
		"roles": []string{"Admin"},
		"nonce": nil,
	}))
	_, err = v.Verify(context.Background(), Microsoft, idToken, "")
	require.NoError(t, err)
}

func TestVerifyMicrosoftKeyIssuer(t *testing.T) {
	v, _ := microsoftVerifier(t, "", microsoftTemplate)
	otherTenant := "11111111-2222-3333-4444-555555555555"

	cases := []struct {
		name     string
		kid      string
		tenant   string
		wantCode string
	}{
		{"template key, own tenant", templateKID, workTenant, ""},
		{"template key, other tenant", templateKID, otherTenant, ""},
		{"tenant key, its tenant", workKID, workTenant, ""},
		{"tenant key, other tenant", workKID, otherTenant, ez.ENOTAUTHENTICATED},
		{"key without issuer", testKID, otherTenant, ""},
	}
	for _, c := range cases {
		issuer := "https://login.microsoftonline.com/" + c.tenant + "/v2.0"
		token := mint(t, signingKey, c.kid, claimsFor(issuer, "ms-app", map[string]any{"tid": c.tenant}))

		_, err := v.Verify(context.Background(), Microsoft, token, testNonce)
		if c.wantCode == "" {
			require.NoError(t, err, c.name)
			continue
		}
		require.Error(t, err, c.name)
		assert.Equal(t, c.wantCode, ez.ErrorCode(err), c.name)
	}
}

func TestVerifyMicrosoftFixedTenant(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Microsoft, ProviderConfig{ClientIDs: []string{"ms-app"}, Issuer: f.server.URL, Tenant: workTenant})

	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "ms-app", map[string]any{"tid": workTenant}))
	identity, err := v.Verify(context.Background(), Microsoft, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, f.server.URL, identity.Issuer)

	tenantIssuer := "https://login.microsoftonline.com/" + workTenant + "/v2.0"
	token = mint(t, signingKey, testKID, claimsFor(tenantIssuer, "ms-app", map[string]any{"tid": workTenant}))
	_, err = v.Verify(context.Background(), Microsoft, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
}

func TestVerifyMicrosoftTenantIsCaseInsensitive(t *testing.T) {
	tenantIssuer := "https://login.microsoftonline.com/" + workTenant + "/v2.0"
	f := startPublicIssuer(t, tenantIssuer)
	v, err := New(Config{
		Microsoft:  ProviderConfig{ClientIDs: []string{"ms-app"}, Tenant: strings.ToUpper(workTenant)},
		HTTPClient: clientTo(t, f),
	})
	require.NoError(t, err)

	token := mint(t, signingKey, testKID, claimsFor(tenantIssuer, "ms-app", map[string]any{"tid": workTenant}))
	identity, err := v.Verify(context.Background(), Microsoft, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, tenantIssuer, identity.Issuer)
}

func TestVerifyRejectsBadTokens(t *testing.T) {
	f := startIssuer(t, "", "")
	v, err := New(Config{
		Google: ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL},
		Apple:  ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL},
	})
	require.NoError(t, err)
	now := time.Now()

	cases := []struct {
		name  string
		key   *rsa.PrivateKey
		kid   string
		extra map[string]any
	}{
		{"wrong audience", signingKey, testKID, map[string]any{"aud": "other"}},
		{"expired", signingKey, testKID, map[string]any{"exp": now.Add(-time.Minute).Unix()}},
		{"no expiry", signingKey, testKID, map[string]any{"exp": nil}},
		{"not yet valid", signingKey, testKID, map[string]any{"nbf": now.Add(time.Hour).Unix()}},
		{"not yet valid beyond leeway", signingKey, testKID, map[string]any{"nbf": now.Add(10 * time.Minute).Unix()}},
		{"bad signature", otherKey, testKID, nil},
		{"unknown kid", signingKey, "unknown", nil},
		{"wrong issuer", signingKey, testKID, map[string]any{"iss": "https://evil.example.com"}},
		{"no subject", signingKey, testKID, map[string]any{"sub": ""}},
	}
	for _, provider := range []Provider{Google, Apple} {
		for _, c := range cases {
			token := mint(t, c.key, c.kid, claimsFor(f.server.URL, "web", c.extra))
			_, err := v.Verify(context.Background(), provider, token, testNonce)
			require.Error(t, err, "%s %s", provider, c.name)
			assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "%s %s", provider, c.name)
		}
	}
}

func TestVerifyRejectsOtherAlgorithms(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})

	// Same key and kid as a valid token, so only the algorithm differs.
	for _, alg := range []jose.SignatureAlgorithm{jose.RS512, jose.PS256} {
		token := mintWith(t, alg, signingKey, testKID, claimsFor(f.server.URL, "web", nil))
		_, err := v.Verify(context.Background(), Google, token, testNonce)
		require.Error(t, err, alg)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), alg)
	}
}

func TestVerifyRejectsJSONSerialization(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})

	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       jose.JSONWebKey{Key: signingKey, KeyID: testKID},
	}, nil)
	require.NoError(t, err)
	payload, err := json.Marshal(claimsFor(f.server.URL, "web", nil))
	require.NoError(t, err)
	jws, err := signer.Sign(payload)
	require.NoError(t, err)

	// Providers issue the compact form only; the JSON form is refused even
	// though its signature verifies.
	_, err = v.Verify(context.Background(), Google, jws.FullSerialize(), testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))

	compact, err := jws.CompactSerialize()
	require.NoError(t, err)
	_, err = v.Verify(context.Background(), Google, compact, testNonce)
	require.NoError(t, err)
}

func TestVerifyClaimForms(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	now := time.Now()

	cases := []struct {
		name     string
		audience any
		extra    map[string]any
	}{
		{"audience string", "web", nil},
		{"audience array", []string{"other", "web"}, nil},
		{"nbf within leeway", "web", map[string]any{"nbf": now.Add(2 * time.Minute).Unix()}},
		{"nbf in the past", "web", map[string]any{"nbf": now.Add(-time.Minute).Unix()}},
	}
	for _, c := range cases {
		token := mint(t, signingKey, testKID, claimsFor(f.server.URL, c.audience, c.extra))
		identity, err := v.Verify(context.Background(), Google, token, testNonce)
		require.NoError(t, err, c.name)
		assert.Equal(t, "web", identity.Audience, c.name)
	}
}

func TestVerifyNonce(t *testing.T) {
	f := startIssuer(t, "", "")
	strict := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	lenient := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL, AllowMissingNonce: true})

	sum := sha256.Sum256([]byte(testNonce))
	hashed := hex.EncodeToString(sum[:])
	withNonce := func(claim string) string {
		return mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", map[string]any{"nonce": claim}))
	}
	noClaims := claimsFor(f.server.URL, "web", nil)
	delete(noClaims, "nonce")
	withoutNonce := mint(t, signingKey, testKID, noClaims)

	cases := []struct {
		name     string
		verifier *Verifier
		token    string
		nonce    string
		wantCode string
	}{
		{"raw match", strict, withNonce(testNonce), testNonce, ""},
		{"sha256 match", strict, withNonce(hashed), testNonce, ""},
		{"uppercase sha256", strict, withNonce(strings.ToUpper(hashed)), testNonce, ez.ENOTAUTHENTICATED},
		{"mismatch", strict, withNonce("other"), testNonce, ez.ENOTAUTHENTICATED},
		{"claim missing", strict, withoutNonce, testNonce, ez.ENOTAUTHENTICATED},
		{"argument missing", strict, withNonce(testNonce), "", ez.ENOTAUTHENTICATED},
		{"argument and claim missing", strict, withoutNonce, "", ez.ENOTAUTHENTICATED},
		{"allowed missing, claim present", lenient, withNonce(testNonce), "", ""},
		{"allowed missing, claim absent", lenient, withoutNonce, "", ""},
		{"allowed missing still compares", lenient, withNonce("other"), testNonce, ez.ENOTAUTHENTICATED},
	}
	for _, c := range cases {
		_, err := c.verifier.Verify(context.Background(), Google, c.token, c.nonce)
		if c.wantCode == "" {
			require.NoError(t, err, c.name)
			continue
		}
		require.Error(t, err, c.name)
		assert.Equal(t, c.wantCode, ez.ErrorCode(err), c.name)
	}
}

func TestVerifyInvalidCalls(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	cases := []struct {
		name     string
		provider Provider
		token    string
	}{
		{"unknown provider", Provider("github"), token},
		{"disabled provider", Apple, token},
		{"empty token", Google, ""},
	}
	for _, c := range cases {
		_, err := v.Verify(context.Background(), c.provider, c.token, testNonce)
		require.Error(t, err, c.name)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), c.name)
	}
	assert.Equal(t, int32(0), f.hits.Load())
}

func TestNewRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no provider", Config{}},
		{"only issuers", Config{Google: ProviderConfig{Issuer: "https://accounts.google.com"}}},
		{"empty client id", Config{Google: ProviderConfig{ClientIDs: []string{"web", ""}}}},
		{"issuer without scheme", Config{Google: ProviderConfig{ClientIDs: []string{"web"}, Issuer: "accounts.google.com"}}},
		{"issuer with other scheme", Config{Apple: ProviderConfig{ClientIDs: []string{"web"}, Issuer: "ftp://appleid.apple.com"}}},
		{"issuer without host", Config{Microsoft: ProviderConfig{ClientIDs: []string{"web"}, Tenant: workTenant, Issuer: "https://"}}},
		{"unparsable issuer", Config{Microsoft: ProviderConfig{ClientIDs: []string{"web"}, Tenant: workTenant, Issuer: "http://[::1"}}},
		{"common with issuer", Config{Microsoft: ProviderConfig{
			ClientIDs: []string{"web"},
			Issuer:    "https://login.microsoftonline.com/" + workTenant + "/v2.0",
		}}},
		{"organizations with issuer", Config{Microsoft: ProviderConfig{
			ClientIDs: []string{"web"},
			Tenant:    "Organizations",
			Issuer:    "https://login.microsoftonline.us/organizations/v2.0",
		}}},
		{"tenant of hyphens", Config{Microsoft: ProviderConfig{ClientIDs: []string{"web"}, Tenant: strings.Repeat("-", 36)}}},
		{"tenant domain", Config{Microsoft: ProviderConfig{ClientIDs: []string{"web"}, Tenant: "contoso.onmicrosoft.com"}}},
		{"tenant with slash", Config{Microsoft: ProviderConfig{ClientIDs: []string{"web"}, Tenant: "common/v2.0/x"}}},
	}
	for _, c := range cases {
		_, err := New(c.cfg)
		require.Error(t, err, c.name)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), c.name)
	}
}

func TestVerifyDiscoveryUnavailableIsRetried(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	f.down.Store(true)
	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	assert.Equal(t, int32(1), f.hits.Load())

	f.down.Store(false)
	advance(fetchCooldown)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.hits.Load())
}

func TestVerifyIssuerUnreachableIsRetried(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	require.NoError(t, listener.Close())

	issuer := "http://" + addr
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: issuer})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(issuer, "web", nil))

	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))

	f := startIssuer(t, addr, "")
	advance(fetchCooldown)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(1), f.hits.Load())
}

func TestVerifyDiscoveryWithoutJWKSIsRetried(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	f.noJWKS.Store(true)
	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))

	f.noJWKS.Store(false)
	advance(fetchCooldown)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.hits.Load())
}

func TestVerifyDiscoveryNamingAnotherIssuerIsRetried(t *testing.T) {
	f := startIssuer(t, "", "https://other.example.com")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	assert.Equal(t, int32(1), f.hits.Load())

	advance(fetchCooldown)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	assert.Equal(t, int32(2), f.hits.Load())
}

func TestVerifyKeyFetchFailureCodes(t *testing.T) {
	cases := []struct {
		name      string
		breakKeys func(f *fakeIssuer)
		code      string
	}{
		{"unavailable", func(f *fakeIssuer) { f.down.Store(true) }, ez.EUNAVAILABLE},
		{"not found", func(f *fakeIssuer) { f.keysCode.Store(http.StatusNotFound) }, ez.EUNAVAILABLE},
		{"malformed key set", func(f *fakeIssuer) { f.keysJunk.Store(true) }, ez.EINTERNAL},
		{"key set without usable keys", func(f *fakeIssuer) { f.keysEmpty.Store(true) }, ez.EINTERNAL},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := startIssuer(t, "", "")
			v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
			advance := clockFor(t, v, Google)
			token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

			_, err := v.Verify(context.Background(), Google, token, testNonce)
			require.NoError(t, err)

			// An unknown kid after the cooldown refetches the keys, which
			// now fails.
			advance(fetchCooldown)
			c.breakKeys(f)
			unknown := mint(t, signingKey, "rotated", claimsFor(f.server.URL, "web", nil))
			_, err = v.Verify(context.Background(), Google, unknown, testNonce)
			require.Error(t, err)
			assert.Equal(t, c.code, ez.ErrorCode(err))

			// Cached keys still verify a known kid while the keys are broken.
			_, err = v.Verify(context.Background(), Google, token, testNonce)
			require.NoError(t, err)
		})
	}
}

func TestVerifyStalledBodyIsUnavailable(t *testing.T) {
	cases := []struct {
		name        string
		stall       func(f *fakeIssuer)
		discoveries int32 // after the recovery: a stalled key body keeps the discovered URL
	}{
		{"discovery", func(f *fakeIssuer) { f.stalled.Store(true) }, 2},
		{"keys", func(f *fakeIssuer) { f.keysStalled.Store(true) }, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := startIssuer(t, "", "")
			v, err := New(Config{
				Google:     ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL},
				HTTPClient: &http.Client{Timeout: 200 * time.Millisecond},
			})
			require.NoError(t, err)
			advance := clockFor(t, v, Google)
			token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

			// The provider answers 200 and then stops sending: the client
			// times out reading the body, which is an outage, not a bad
			// document.
			c.stall(f)
			_, err = v.Verify(context.Background(), Google, token, testNonce)
			require.Error(t, err)
			assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))

			close(f.release)
			advance(fetchCooldown)
			_, err = v.Verify(context.Background(), Google, token, testNonce)
			require.NoError(t, err)
			assert.Equal(t, c.discoveries, f.hits.Load())
		})
	}
}

func TestVerifyWaitingForDiscoveryRespectsContext(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	f.blocked.Store(true)
	release := sync.OnceFunc(func() {
		close(f.release)
	})
	t.Cleanup(release)

	first := make(chan error, 1)
	go func() {
		_, err := v.Verify(context.Background(), Google, token, testNonce)
		first <- err
	}()
	require.Eventually(t, func() bool {
		return f.hits.Load() == 1
	}, time.Second, time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := v.Verify(ctx, Google, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	assert.Less(t, time.Since(started), 500*time.Millisecond)
	assert.Equal(t, int32(1), f.hits.Load())

	release()
	select {
	case err := <-first:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("first Verify did not return after discovery was released")
	}
}

func TestVerifyConcurrentDiscoveryRunsOnce(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	const callers = 20
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = v.Verify(context.Background(), Google, token, testNonce)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, int32(1), f.hits.Load())
}

func TestVerifyConcurrentFailedDiscoveryIsShared(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))
	// The held discovery answers 503 once released.
	f.blocked.Store(true)
	f.down.Store(true)
	release := sync.OnceFunc(func() {
		close(f.release)
	})
	t.Cleanup(release)

	const callers = 20
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = v.Verify(context.Background(), Google, token, testNonce)
		}()
	}
	require.Eventually(t, func() bool {
		return f.hits.Load() == 1
	}, time.Second, time.Millisecond)
	// Only lets the callers reach the wait for the held discovery.
	time.Sleep(50 * time.Millisecond)

	release()
	wg.Wait()

	// Every waiter shares the failed discovery instead of requesting in turn.
	for _, err := range errs {
		require.Error(t, err)
		assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	}
	assert.Equal(t, int32(1), f.hits.Load())

	// Within the cooldown the failure answers without a request; a call
	// after it tries again.
	f.down.Store(false)
	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	assert.Equal(t, int32(1), f.hits.Load())

	advance(fetchCooldown)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.hits.Load())
}

func TestVerifyMalformedTokenMakesNoRequest(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})

	noKid := mint(t, signingKey, "", claimsFor(f.server.URL, "web", nil))
	for _, token := range []string{"not-a-token", noKid} {
		_, err := v.Verify(context.Background(), Google, token, testNonce)
		require.Error(t, err)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	}
	assert.Equal(t, int32(0), f.hits.Load())
	assert.Equal(t, int32(0), f.keysHits.Load())
}

// clockFor replaces the key set clock of v's provider with one the test
// moves. The token checks keep using the real clock, so minted tokens stay
// within their expiry.
func clockFor(t *testing.T, v *Verifier, provider Provider) func(time.Duration) {
	t.Helper()

	now := time.Now()
	v.providers[provider].keys.now = func() time.Time {
		return now
	}

	return func(d time.Duration) {
		now = now.Add(d)
	}
}

// settle waits for the fetch v's provider has running behind its callers,
// if any.
func settle(v *Verifier, provider Provider) {
	k := v.providers[provider].keys
	k.mu.Lock()
	done := k.inflight
	k.mu.Unlock()
	if done != nil {
		<-done
	}
}

func TestVerifyCachedKeyDoesNotWaitForTheRefresh(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.keysHits.Load())

	// The refresh is due and the provider hangs. A cached kid is served at
	// once while the refresh runs behind it, and the next cached kid does
	// not start another.
	f.keysBlocked.Store(true)
	release := sync.OnceFunc(func() {
		close(f.release)
	})
	t.Cleanup(release)
	advance(refreshInterval)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = v.Verify(ctx, Google, token, testNonce)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return f.keysHits.Load() == 2
	}, time.Second, time.Millisecond)
	_, err = v.Verify(ctx, Google, token, testNonce)
	require.NoError(t, err)

	release()
	settle(v, Google)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.keysHits.Load())
}

func TestVerifyRemovedKeyStopsVerifyingAfterRefresh(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	removed := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))
	kept := mint(t, signingKey, secondKID, claimsFor(f.server.URL, "web", nil))

	_, err := v.Verify(context.Background(), Google, removed, testNonce)
	require.NoError(t, err)
	_, err = v.Verify(context.Background(), Google, kept, testNonce)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.keysHits.Load())

	// The provider drops the key. It serves from the cache until the
	// refresh is due; the call that starts the refresh is still served
	// from the cache, and the refresh finds the key gone.
	f.keysRotated.Store(true)
	_, err = v.Verify(context.Background(), Google, removed, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(1), f.keysHits.Load())

	advance(refreshInterval)
	_, err = v.Verify(context.Background(), Google, removed, testNonce)
	require.NoError(t, err)
	settle(v, Google)
	_, err = v.Verify(context.Background(), Google, removed, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	assert.Equal(t, int32(2), f.keysHits.Load())

	// The refreshed set serves the surviving key without another fetch.
	_, err = v.Verify(context.Background(), Google, kept, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.keysHits.Load())
}

func TestVerifyStaleKeysServeThroughOutageUntilLimit(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.keysHits.Load())

	// The refresh fails behind the cached answers: the cached key still
	// serves, and the next attempt waits for the cooldown instead of
	// running on every call.
	f.keysCode.Store(http.StatusServiceUnavailable)
	advance(refreshInterval)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	settle(v, Google)
	assert.Equal(t, int32(2), f.keysHits.Load())
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.keysHits.Load())

	advance(fetchCooldown)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	settle(v, Google)
	assert.Equal(t, int32(3), f.keysHits.Load())

	// Past the stale limit the old set is not trusted any more: the call
	// waits for the fetch and gets its failure.
	advance(staleLimit)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	assert.Equal(t, int32(4), f.keysHits.Load())

	// The provider comes back: the next call after the cooldown fetches
	// and verifies again.
	f.keysCode.Store(0)
	advance(fetchCooldown)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(5), f.keysHits.Load())
	// The 503s said nothing about the URL, so discovery did not run again.
	assert.Equal(t, int32(1), f.hits.Load())
}

func TestVerifyFetchFailureIsReported(t *testing.T) {
	f := startIssuer(t, "", "")
	type report struct {
		provider Provider
		err      error
	}
	reports := make(chan report, 10)
	v, err := New(Config{
		Google: ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL},
		OnFetchError: func(provider Provider, err error) {
			reports <- report{provider, err}
		},
	})
	require.NoError(t, err)
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Empty(t, reports)

	// The refresh fails behind a sign-in that succeeds from the cache: the
	// application hears about it anyway.
	f.keysCode.Store(http.StatusServiceUnavailable)
	advance(refreshInterval)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	settle(v, Google)
	require.Len(t, reports, 1)
	got := <-reports
	assert.Equal(t, Google, got.provider)
	assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(got.err))
}

func TestVerifyMovedKeysAreRediscovered(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.hits.Load())
	require.Equal(t, int32(1), f.keysHits.Load())

	// The provider moves its keys and rotates them. The refresh hits the
	// old URL and fails; the cached key still serves.
	f.keysMoved.Store(true)
	f.keysRotated.Store(true)
	advance(refreshInterval)
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	settle(v, Google)
	assert.Equal(t, int32(1), f.hits.Load())
	assert.Equal(t, int32(2), f.keysHits.Load())

	// The next fetch runs discovery again, finds the new URL and the new
	// set behind it.
	advance(fetchCooldown)
	rotated := mint(t, otherKey, rotatedKID, claimsFor(f.server.URL, "web", nil))
	_, err = v.Verify(context.Background(), Google, rotated, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.hits.Load())
	assert.Equal(t, int32(3), f.keysHits.Load())
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
}

func TestVerifyUnusableKeyIsNamed(t *testing.T) {
	f := startIssuer(t, "", "")
	f.keysOdd.Store(true)
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})

	// The usable key verifies; the two odd entries do not hide it.
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))
	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)

	// A token naming an odd entry is refused with the reason, and costs no
	// fetch inside the cooldown. A kid the set does not publish at all is
	// told apart from it.
	for _, kid := range []string{"enc-only", "bad-n"} {
		odd := mint(t, signingKey, kid, claimsFor(f.server.URL, "web", nil))
		_, err = v.Verify(context.Background(), Google, odd, testNonce)
		require.Error(t, err, kid)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), kid)
		assert.Contains(t, ez.ErrorMessage(err), kid)
		assert.Contains(t, ez.ErrorMessage(err), "cannot be used", kid)
	}
	unknown := mint(t, signingKey, "nope", claimsFor(f.server.URL, "web", nil))
	_, err = v.Verify(context.Background(), Google, unknown, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	assert.NotContains(t, ez.ErrorMessage(err), "cannot be used")
	assert.Equal(t, int32(1), f.keysHits.Load())
}

func TestVerifyRotatedKeyVerifiesAfterOneFetch(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	old := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	_, err := v.Verify(context.Background(), Google, old, testNonce)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.keysHits.Load())

	// The provider rotates: a token under the new kid, signed with the new
	// key, verifies after one fetch, which also drops the old key.
	f.keysRotated.Store(true)
	advance(fetchCooldown)
	rotated := mint(t, otherKey, rotatedKID, claimsFor(f.server.URL, "web", nil))
	_, err = v.Verify(context.Background(), Google, rotated, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.keysHits.Load())

	_, err = v.Verify(context.Background(), Google, old, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	assert.Equal(t, int32(2), f.keysHits.Load())
}

func TestVerifyCancelledFetchStartsTheCooldown(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.keysHits.Load())
	advance(fetchCooldown)

	f.keysBlocked.Store(true)
	release := sync.OnceFunc(func() {
		close(f.release)
	})
	t.Cleanup(release)

	// A caller with an unknown kid starts a fetch and hangs up on it.
	holderCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	unknown := mint(t, otherKey, "nope", claimsFor(f.server.URL, "web", nil))
	holder := make(chan error, 1)
	go func() {
		_, err := v.Verify(holderCtx, Google, unknown, testNonce)
		holder <- err
	}()
	require.Eventually(t, func() bool {
		return f.keysHits.Load() == 2
	}, time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-holder:
		require.Error(t, err)
		assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	case <-time.After(5 * time.Second):
		t.Fatal("holder did not return after cancelling")
	}
	release()

	// Hanging up did not skip the cooldown: the next unknown kids inside it
	// are refused without a request. Cached kids keep verifying.
	forged := mint(t, otherKey, "nope-2", claimsFor(f.server.URL, "web", nil))
	for _, kid := range []string{"nope", "nope-2"} {
		_, err = v.Verify(context.Background(), Google, mint(t, otherKey, kid, claimsFor(f.server.URL, "web", nil)), testNonce)
		require.Error(t, err, kid)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), kid)
	}
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	assert.Equal(t, int32(2), f.keysHits.Load())

	advance(fetchCooldown)
	_, err = v.Verify(context.Background(), Google, forged, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	assert.Equal(t, int32(3), f.keysHits.Load())
}

func TestVerifyUnknownKidFetchIsPaced(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))

	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.keysHits.Load())

	// Forged tokens with fresh kids, one after another: the first after
	// the cooldown fetches, the ones inside it are refused without one.
	advance(fetchCooldown)
	for i, kid := range []string{"forged-1", "forged-2", "forged-3"} {
		forged := mint(t, otherKey, kid, claimsFor(f.server.URL, "web", nil))
		_, err = v.Verify(context.Background(), Google, forged, testNonce)
		require.Error(t, err, kid)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), kid)
		assert.Equal(t, int32(2), f.keysHits.Load(), "token %d", i)
	}

	// Cached kids keep verifying meanwhile, and the cooldown ends.
	_, err = v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	advance(fetchCooldown)
	forged := mint(t, otherKey, "forged-4", claimsFor(f.server.URL, "web", nil))
	_, err = v.Verify(context.Background(), Google, forged, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	assert.Equal(t, int32(3), f.keysHits.Load())
}

// startBlockedKeyFetch warms discovery and the key cache with one Verify
// and moves the key set clock past the fetch cooldown, then holds key set
// requests and starts a Verify of a token whose kid the fake does not
// publish. It returns once that Verify's refetch is held, with the Verify's
// result channel, a release function and the clock.
func startBlockedKeyFetch(t *testing.T, f *fakeIssuer, v *Verifier) (<-chan error, func(), func(time.Duration)) {
	t.Helper()

	advance := clockFor(t, v, Google)
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))
	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	require.Equal(t, int32(1), f.keysHits.Load())
	advance(fetchCooldown)

	f.keysBlocked.Store(true)
	release := sync.OnceFunc(func() {
		close(f.release)
	})
	t.Cleanup(release)

	first := make(chan error, 1)
	unknown := mint(t, otherKey, "nope", claimsFor(f.server.URL, "web", nil))
	go func() {
		_, err := v.Verify(context.Background(), Google, unknown, testNonce)
		first <- err
	}()
	require.Eventually(t, func() bool {
		return f.keysHits.Load() == 2
	}, time.Second, time.Millisecond)

	return first, release, advance
}

func TestVerifyConcurrentUnknownKidFetchesOnce(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	first, release, _ := startBlockedKeyFetch(t, f, v)

	const callers = 19
	unknown := mint(t, otherKey, "nope", claimsFor(f.server.URL, "web", nil))
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = v.Verify(context.Background(), Google, unknown, testNonce)
		}()
	}
	// Only lets the callers reach the wait for the held fetch. The hit
	// count below is what the test asserts.
	time.Sleep(50 * time.Millisecond)

	release()
	wg.Wait()
	select {
	case err := <-first:
		errs = append(errs, err)
	case <-time.After(5 * time.Second):
		t.Fatal("first Verify did not return after the key fetch was released")
	}

	for _, err := range errs {
		require.Error(t, err)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	}
	// One fetch warmed the cache, one served every unknown kid.
	assert.Equal(t, int32(2), f.keysHits.Load())
}

func TestVerifyConcurrentFailedKeyFetchIsShared(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	first, release, advance := startBlockedKeyFetch(t, f, v)
	// The held fetch answers 503 once released.
	f.keysCode.Store(http.StatusServiceUnavailable)

	const callers = 19
	unknown := mint(t, otherKey, "nope", claimsFor(f.server.URL, "web", nil))
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = v.Verify(context.Background(), Google, unknown, testNonce)
		}()
	}
	// Only lets the callers reach the wait for the held fetch.
	time.Sleep(50 * time.Millisecond)

	release()
	wg.Wait()
	select {
	case err := <-first:
		errs = append(errs, err)
	case <-time.After(5 * time.Second):
		t.Fatal("first Verify did not return after the key fetch was released")
	}

	// Every waiter shares the failed attempt instead of fetching in turn.
	for _, err := range errs {
		require.Error(t, err)
		assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	}
	assert.Equal(t, int32(2), f.keysHits.Load())

	// The cached kid still verifies. Within the cooldown the failure answers
	// the unknown kid without a request; after it, a call fetches again.
	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))
	_, err := v.Verify(context.Background(), Google, token, testNonce)
	require.NoError(t, err)
	f.keysCode.Store(0)
	_, err = v.Verify(context.Background(), Google, unknown, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	assert.Equal(t, int32(2), f.keysHits.Load())

	advance(fetchCooldown)
	_, err = v.Verify(context.Background(), Google, unknown, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	assert.Equal(t, int32(3), f.keysHits.Load())
}

func TestVerifyCallerGivingUpDoesNotStopTheFetch(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	f.keysBlocked.Store(true)
	release := sync.OnceFunc(func() {
		close(f.release)
	})
	t.Cleanup(release)

	// Cold cache. A caller with a short deadline starts the fetch and gives
	// up; a real sign-in with a long deadline waits for the same fetch.
	holderCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	unknown := mint(t, otherKey, "nope", claimsFor(f.server.URL, "web", nil))
	holder := make(chan error, 1)
	go func() {
		_, err := v.Verify(holderCtx, Google, unknown, testNonce)
		holder <- err
	}()
	require.Eventually(t, func() bool {
		return f.keysHits.Load() == 1
	}, time.Second, time.Millisecond)

	token := mint(t, signingKey, testKID, claimsFor(f.server.URL, "web", nil))
	waiter := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := v.Verify(ctx, Google, token, testNonce)
		waiter <- err
	}()

	select {
	case err := <-holder:
		require.Error(t, err)
		assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	case <-time.After(5 * time.Second):
		t.Fatal("holder did not give up at its deadline")
	}

	// The fetch outlives the caller that started it and answers the waiter.
	release()
	select {
	case err := <-waiter:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not return after the key fetch was released")
	}
	assert.Equal(t, int32(1), f.keysHits.Load())
}

func TestVerifyWaitingForKeyFetchRespectsContext(t *testing.T) {
	f := startIssuer(t, "", "")
	v := newVerifier(t, Google, ProviderConfig{ClientIDs: []string{"web"}, Issuer: f.server.URL})
	first, release, _ := startBlockedKeyFetch(t, f, v)

	unknown := mint(t, otherKey, "nope", claimsFor(f.server.URL, "web", nil))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := v.Verify(ctx, Google, unknown, testNonce)
	require.Error(t, err)
	assert.Equal(t, ez.EUNAVAILABLE, ez.ErrorCode(err))
	assert.Less(t, time.Since(started), 500*time.Millisecond)
	assert.Equal(t, int32(2), f.keysHits.Load())

	release()
	select {
	case err := <-first:
		require.Error(t, err)
		assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	case <-time.After(5 * time.Second):
		t.Fatal("first Verify did not return after the key fetch was released")
	}
}
