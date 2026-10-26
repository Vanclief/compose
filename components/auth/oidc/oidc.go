// Package oidc verifies OpenID Connect ID tokens from Google, Apple and
// Microsoft.
//
// A client (iOS, Android, web) signs in with the provider's SDK and posts the
// ID token to the application, which calls Verify before trusting the token's
// subject and email. Verify checks the RS256 signature with
// github.com/go-jose/go-jose, then the expiry, issuer, audience and nonce,
// handling the provider quirks (Microsoft's multi-tenant issuer, Apple's
// string booleans), and returns a normalized Identity. What the application
// does with the identity is up to it.
//
// Signing keys are fetched and cached by this package so the JWKS "issuer"
// field that Microsoft requires checking is honored: a key that carries an
// issuer only signs tokens whose iss equals it after "{tenantid}" is replaced
// by the token's tid. Keys without an issuer, which is all of Google's and
// Apple's, sign for any issuer.
//
// The nonce check only protects against replay when the application issues
// the nonce, stores it and consumes it once before calling Verify. A nonce the
// client made up and sent along with the token proves nothing, because
// whoever holds a leaked token can read its nonce claim and send it. Verify
// can only compare; it cannot enforce single use.
//
// Discovery and the key set are fetched lazily, the first time a provider's
// Verify meets a token whose key is not cached. A fetched set serves for 24
// hours, then the next Verify starts a refresh behind it without waiting,
// so a key the provider removed stops verifying once that refresh lands:
// within a day and a fetch under steady traffic, while a service with no
// sign-ins for days serves the first token after the quiet period from
// the cache. A token naming a kid the set does not publish waits for a
// fetch, at most one a minute. A failure is not cached: the calls waiting
// on that fetch share its error, the next call after the minute tries
// again, cached keys keep serving for up to 7 days meanwhile, and
// Config.OnFetchError hears about each failure so the outage is seen
// before that. Errors are ez.EINVALID for a bad call,
// ez.ENOTAUTHENTICATED for a token that does not verify, ez.EUNAVAILABLE
// when the provider does not answer and ez.EINTERNAL when it answers a
// document that cannot be used, so an outage never reads as an unknown
// account.
package oidc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/vanclief/ez"
)

// Provider names an identity provider.
type Provider string

const (
	Google    Provider = "google"
	Apple     Provider = "apple"
	Microsoft Provider = "microsoft"
)

const (
	googleIssuer      = "https://accounts.google.com"
	appleIssuer       = "https://appleid.apple.com"
	microsoftTemplate = "https://login.microsoftonline.com/{tenantid}/v2.0"
	// googleIssuerWithoutScheme is the iss some Google tokens carry. It is
	// accepted only when the provider uses the default googleIssuer.
	googleIssuerWithoutScheme = "accounts.google.com"
	// consumersTenant is Microsoft's personal-accounts tenant: every personal
	// Microsoft account signs in through it.
	consumersTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"
	// notBeforeLeeway tolerates clock skew on the nbf claim. exp has none.
	notBeforeLeeway = 5 * time.Minute
)

// microsoftIssuer is the issuer of a token signed in through a multi-tenant
// Microsoft endpoint. The group is the tenant id the tid claim must repeat.
// Microsoft issues tenant ids in lowercase, and the consumers and
// organizations rules compare them exactly.
var microsoftIssuer = regexp.MustCompile(`^https://login\.microsoftonline\.com/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/v2\.0$`)

// tenantPlaceholder is the template part of a Microsoft key issuer. Microsoft
// documents matching it case-insensitively.
var tenantPlaceholder = regexp.MustCompile(`(?i)\{tenantid\}`)

// tenantID is a lowercase Microsoft tenant id, the form Microsoft's
// discovery documents publish.
var tenantID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Config enables providers. A provider with no ClientIDs is disabled.
type Config struct {
	Google    ProviderConfig
	Apple     ProviderConfig
	Microsoft ProviderConfig
	// HTTPClient is used for discovery and key fetching. The default has a
	// 10 second timeout. A fetch runs detached from the Verify call that
	// started it and is capped at 30 seconds regardless of the client.
	HTTPClient *http.Client
	// OnFetchError, when set, is called with each discovery or key fetch
	// failure, from the fetch's own goroutine. Verify keeps serving cached
	// keys through failures for up to 7 days without reporting them, so
	// log or report them here to see an outage before sign-ins start
	// failing.
	OnFetchError func(provider Provider, err error)
}

// ProviderConfig configures one provider.
type ProviderConfig struct {
	// ClientIDs are the accepted audiences: one per client app (iOS, Android, web). An
	// empty list disables the provider, and Verify answers ez.EINVALID for it.
	ClientIDs []string
	// Issuer overrides the provider's default issuer URL. For tests and private
	// deployments. The default is the provider's public issuer. Google's
	// scheme-less "accounts.google.com" is accepted only with the default issuer.
	// For Microsoft, Issuer can only be set together with a tenant id; for a
	// private or sovereign cloud set Tenant to the tenant id and Issuer to its URL.
	Issuer string
	// Tenant is Microsoft only: "common" (default), "consumers", "organizations" or a
	// tenant id, case-insensitive. Ignored for the other providers.
	// "consumers" accepts only personal accounts and "organizations" only work or
	// school accounts, while "common" accepts both.
	Tenant string
	// AllowMissingNonce accepts a token with no nonce claim when Verify is called with an
	// empty nonce. Default false: a missing nonce is ez.ENOTAUTHENTICATED. Microsoft's
	// MSAL libraries manage the nonce themselves, so applications set this for Microsoft.
	// Without a nonce any token the provider signed for one of the ClientIDs is a
	// sign-in, so Verify refuses Microsoft access tokens outright (see Verify).
	// Prefer a nonce where the client library takes one, and keep the sign-in
	// app registration from exposing an API.
	AllowMissingNonce bool
}

// Identity is the verified, normalized content of an ID token.
type Identity struct {
	Provider      Provider
	Subject       string // the provider's stable account id ("sub")
	Email         string // empty when the token carries none
	EmailVerified bool   // Google and Apple email_verified, always false for Microsoft
	PrivateEmail  bool   // Apple "Hide My Email" relay address
	HostedDomain  string // Google Workspace domain ("hd"), empty for personal accounts and other providers
	Issuer        string // the token's issuer, for logging
	Audience      string // the matched client id
}

// Verifier verifies ID tokens. It is safe for concurrent use.
type Verifier struct {
	providers map[Provider]*provider
}

type provider struct {
	name              Provider
	clientIDs         []string
	issuer            string
	tenant            string // Microsoft: lowercase keyword or tenant id
	multiTenant       bool   // Microsoft common, consumers or organizations
	allowMissingNonce bool
	client            *http.Client
	keys              *keySet
}

// New validates cfg and returns a Verifier. It makes no network call. It
// returns ez.EINVALID when no provider is enabled, when a client id is empty,
// when an issuer is not an absolute http(s) URL or when the Microsoft tenant
// is not valid.
func New(cfg Config) (*Verifier, error) {
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	v := &Verifier{providers: make(map[Provider]*provider)}
	configs := []struct {
		name Provider
		cfg  ProviderConfig
	}{
		{Google, cfg.Google},
		{Apple, cfg.Apple},
		{Microsoft, cfg.Microsoft},
	}
	for _, c := range configs {
		p, err := newProvider(c.name, c.cfg, client)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		if cfg.OnFetchError != nil {
			p.keys.onFetchError = func(err error) {
				cfg.OnFetchError(c.name, err)
			}
		}
		if len(p.clientIDs) > 0 {
			v.providers[c.name] = p
		}
	}
	if len(v.providers) == 0 {
		return nil, ez.New(ez.EINVALID, "oidc: no provider has client ids", nil)
	}

	return v, nil
}

func newProvider(name Provider, cfg ProviderConfig, client *http.Client) (*provider, error) {
	if slices.Contains(cfg.ClientIDs, "") {
		return nil, ez.New(ez.EINVALID, fmt.Sprintf("oidc: %s client id is empty", name), nil)
	}

	p := &provider{
		name:              name,
		clientIDs:         cfg.ClientIDs,
		issuer:            cfg.Issuer,
		allowMissingNonce: cfg.AllowMissingNonce,
		client:            client,
	}
	switch name {
	case Google:
		if p.issuer == "" {
			p.issuer = googleIssuer
		}
	case Apple:
		if p.issuer == "" {
			p.issuer = appleIssuer
		}
	case Microsoft:
		tenant := strings.ToLower(cfg.Tenant)
		if tenant == "" {
			tenant = "common"
		}
		p.tenant = tenant
		p.multiTenant = tenant == "common" || tenant == "consumers" || tenant == "organizations"
		if !p.multiTenant && !tenantID.MatchString(tenant) {
			return nil, ez.New(ez.EINVALID, "oidc: microsoft tenant must be common, consumers, organizations or a tenant id", nil)
		}
		// The multi-tenant issuer rule in Verify knows only the public cloud.
		if p.multiTenant && p.issuer != "" {
			return nil, ez.New(ez.EINVALID, "oidc: microsoft issuer can only be set with a tenant id", nil)
		}
		if p.issuer == "" {
			p.issuer = "https://login.microsoftonline.com/" + tenant + "/v2.0"
		}
	}

	u, err := url.Parse(p.issuer)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, ez.New(ez.EINVALID, fmt.Sprintf("oidc: %s issuer must be an absolute http(s) URL", name), err)
	}
	p.keys = &keySet{discover: p.discover, client: client, now: time.Now}

	return p, nil
}

// Verify checks an ID token from the given provider and returns the normalized identity.
// nonce is the value the application issued for this sign-in (see the package
// doc). When it is not empty the token's nonce claim must equal it or its
// SHA-256 as lowercase hex. When it is empty the token is refused unless the
// provider has AllowMissingNonce.
func (v *Verifier) Verify(ctx context.Context, provider Provider, idToken, nonce string) (Identity, error) {
	p, ok := v.providers[provider]
	if !ok {
		return Identity{}, ez.New(ez.EINVALID, fmt.Sprintf("oidc: provider %q is unknown or disabled", provider), nil)
	}
	if idToken == "" {
		return Identity{}, ez.New(ez.EINVALID, "oidc: id token is empty", nil)
	}

	// ParseSigned takes the compact form only and refuses any alg but RS256.
	// It runs before any network call, so a malformed token costs none.
	token, err := jwt.ParseSigned(idToken, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: malformed token", err)
	}
	if len(token.Headers) != 1 {
		return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: token must have one signature", nil)
	}
	// Every provider key has a kid, so a token without one cannot verify
	// and is refused before it can cost a key fetch.
	if token.Headers[0].KeyID == "" {
		return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: token has no kid", nil)
	}
	key, err := p.keys.key(ctx, token.Headers[0].KeyID)
	if err != nil {
		return Identity{}, ez.Wrap(err)
	}

	// Claims verifies the signature with key and decodes the verified
	// payload; nothing is read from the token before that.
	var claims jwt.Claims
	var custom struct {
		Nonce          string   `json:"nonce"`
		Email          string   `json:"email"`
		EmailVerified  flexBool `json:"email_verified"`
		IsPrivateEmail flexBool `json:"is_private_email"`
		HostedDomain   string   `json:"hd"`
		TenantID       string   `json:"tid"`
		Scope          *string  `json:"scp"`
		ObjectID       string   `json:"oid"`
		IDType         string   `json:"idtyp"`
	}
	err = token.Claims(&key.key, &claims, &custom)
	if err != nil {
		return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: token does not verify", err)
	}

	// A missing exp reads as expired. nbf gets a leeway, exp does not.
	now := time.Now()
	if claims.Expiry == nil || !claims.Expiry.Time().After(now) {
		return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: token is expired", nil)
	}
	if claims.NotBefore != nil && now.Add(notBeforeLeeway).Before(claims.NotBefore.Time()) {
		return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: token is not yet valid", nil)
	}

	if p.multiTenant {
		match := microsoftIssuer.FindStringSubmatch(claims.Issuer)
		if match == nil {
			return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: issuer not accepted", nil)
		}
		if custom.TenantID != match[1] {
			return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: tid does not match issuer", nil)
		}
		switch p.tenant {
		case "consumers":
			if custom.TenantID != consumersTenant {
				return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: token is not from a personal account", nil)
			}
		case "organizations":
			if custom.TenantID == consumersTenant {
				return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: token is from a personal account", nil)
			}
		}
	} else if claims.Issuer != p.issuer {
		googleWithoutScheme := p.issuer == googleIssuer && claims.Issuer == googleIssuerWithoutScheme
		if !googleWithoutScheme {
			return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: issuer not accepted", nil)
		}
	}

	// When the app registration exposes an API, Microsoft signs its access
	// tokens with the same keys, issuer and audience as the ID tokens, and an
	// access token is not a sign-in. Delegated access tokens carry scp.
	// App-only (client credentials) tokens carry no user instead: Microsoft
	// documents that their sub equals oid, and idtyp says "app" when the
	// registration asks for that claim. roles tells nothing apart, ID tokens
	// carry the user's app roles too.
	if provider == Microsoft {
		appOnly := custom.IDType == "app" || (custom.ObjectID != "" && custom.ObjectID == claims.Subject)
		if custom.Scope != nil || appOnly {
			return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: token is an access token", nil)
		}
	}

	if key.issuer != "" {
		keyIssuer := tenantPlaceholder.ReplaceAllLiteralString(key.issuer, custom.TenantID)
		if keyIssuer != claims.Issuer {
			return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: signing key is not valid for this issuer", nil)
		}
	}

	audience := ""
	for _, id := range p.clientIDs {
		if slices.Contains(claims.Audience, id) {
			audience = id
			break
		}
	}
	if audience == "" {
		return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: audience not accepted", nil)
	}

	if nonce == "" {
		if !p.allowMissingNonce {
			return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: nonce is required", nil)
		}
	} else if !nonceMatches(custom.Nonce, nonce) {
		return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: nonce mismatch", nil)
	}

	if claims.Subject == "" {
		return Identity{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: token has no subject", nil)
	}

	identity := Identity{
		Provider: provider,
		Subject:  claims.Subject,
		Email:    custom.Email,
		Issuer:   claims.Issuer,
		Audience: audience,
	}
	switch provider {
	case Google:
		identity.EmailVerified = bool(custom.EmailVerified)
		identity.HostedDomain = custom.HostedDomain
	case Apple:
		identity.EmailVerified = bool(custom.EmailVerified)
		identity.PrivateEmail = bool(custom.IsPrivateEmail)
	}

	return identity, nil
}

// discover reads the provider's discovery document and returns its jwks_uri.
// The key set calls it from its fetch until it succeeds.
func (p *provider) discover(ctx context.Context) (string, error) {
	discoveryURL := strings.TrimSuffix(p.issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return "", ez.New(ez.EINTERNAL, fmt.Sprintf("oidc: %s discovery failed", p.name), err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return "", ez.New(ez.EUNAVAILABLE, fmt.Sprintf("oidc: %s discovery failed", p.name), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", ez.New(ez.EUNAVAILABLE, fmt.Sprintf("oidc: %s discovery answered HTTP %d", p.name, resp.StatusCode), nil)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
	if err != nil {
		return "", ez.New(ez.EUNAVAILABLE, fmt.Sprintf("oidc: %s discovery failed", p.name), err)
	}
	var document struct {
		Issuer  string `json:"issuer"`
		JWKSURL string `json:"jwks_uri"`
	}
	err = json.Unmarshal(body, &document)
	if err != nil {
		return "", ez.New(ez.EINTERNAL, fmt.Sprintf("oidc: %s discovery document is malformed", p.name), err)
	}

	// The document must name the issuer it was fetched for (OIDC Discovery
	// 4.3). Microsoft's common and organizations publish the {tenantid}
	// template, consumers publishes its fixed tenant issuer.
	issuerMatches := document.Issuer == p.issuer
	if p.multiTenant {
		issuerMatches = document.Issuer == microsoftTemplate || microsoftIssuer.MatchString(document.Issuer)
	}
	if !issuerMatches {
		return "", ez.New(ez.EINTERNAL, fmt.Sprintf("oidc: %s discovery document names another issuer", p.name), nil)
	}
	if document.JWKSURL == "" {
		return "", ez.New(ez.EINTERNAL, fmt.Sprintf("oidc: %s discovery document has no jwks_uri", p.name), nil)
	}

	return document.JWKSURL, nil
}

func nonceMatches(claim, nonce string) bool {
	sum := sha256.Sum256([]byte(nonce))
	hashed := hex.EncodeToString(sum[:])
	raw := subtle.ConstantTimeCompare([]byte(claim), []byte(nonce))
	digest := subtle.ConstantTimeCompare([]byte(claim), []byte(hashed))

	return raw|digest == 1
}

// flexBool reads a JSON boolean or the strings "true" and "false", the two
// forms Apple uses for email_verified and is_private_email.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", `"true"`:
		*b = true
	case "false", `"false"`, "null":
		*b = false
	default:
		return ez.New(ez.EINVALID, "oidc: boolean claim must be true or false", nil)
	}

	return nil
}
