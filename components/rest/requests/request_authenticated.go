package requests

import (
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/vanclief/ez"
)

type AuthenticatedRequest struct {
	StandardRequest
	// Token is the bearer token from the Authorization header, or the cookie
	// named by WithCookie when the header is absent.
	Token string
	// FromCookie is true when Token came from a cookie rather than the
	// Authorization header.
	FromCookie bool
}

// AuthOption configures where Authenticate looks for credentials besides the
// Authorization header.
type AuthOption func(*authConfig)

type authConfig struct {
	cookie         string
	allowedOrigins []string
}

// WithCookie reads the token from the named cookie when no Authorization
// header is present. Requests authenticated by cookie must pass the CSRF
// rule: a method other than GET, HEAD or OPTIONS needs an Origin header
// listed in allowedOrigins (exact match). With no allowedOrigins, cookies
// authenticate only GET, HEAD and OPTIONS. The method comes from WithMethod;
// a request without one is treated as unsafe.
func WithCookie(name string, allowedOrigins ...string) AuthOption {
	return func(cfg *authConfig) {
		cfg.cookie = name
		cfg.allowedOrigins = allowedOrigins
	}
}

// Authenticate reads the credentials of the request, in order: the
// Authorization bearer token, then the WithCookie cookie. A malformed
// Authorization header is EINVALID and the cookie is not tried. A request with
// no credentials is ENOTAUTHENTICATED. Authenticate only extracts credentials;
// the caller verifies them.
func (r StandardRequest) Authenticate(opts ...AuthOption) (*AuthenticatedRequest, error) {
	cfg := authConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	authRequest := &AuthenticatedRequest{StandardRequest: r}

	token, err := ParseBearer(r.Header)
	if err == nil {
		authRequest.Token = token
		return authRequest, nil
	}
	if ez.ErrorCode(err) != ez.ENOTAUTHENTICATED {
		return nil, ez.Wrap(err)
	}

	if cfg.cookie != "" {
		cookie, err := (&http.Request{Header: r.Header}).Cookie(cfg.cookie)
		if err == nil && cookie.Value != "" {
			err = checkCookieOrigin(r, cfg.allowedOrigins)
			if err != nil {
				return nil, ez.Wrap(err)
			}
			authRequest.Token = cookie.Value
			authRequest.FromCookie = true
			return authRequest, nil
		}
	}

	return nil, ez.New(ez.ENOTAUTHENTICATED, "Request has no credentials", nil)
}

// LegacyCredentials returns the legacy header pair, for REST APIs that keep
// API-key clients working while they move to Authorization: Bearer. It reads
// nothing else. Both headers absent is ez.ENOTAUTHENTICATED; only one present
// is ez.EINVALID. Compose it after Authenticate and fall through only on
// ez.ENOTAUTHENTICATED, so a malformed Authorization header or a cookie that
// fails the CSRF rule is still refused rather than retried with the pair:
//
//	authReq, err := req.Authenticate()
//	if ez.ErrorCode(err) == ez.ENOTAUTHENTICATED {
//		key, secret, err := req.LegacyCredentials("API-KEY", "API-SECRET")
//		if err != nil {
//			return ez.Wrap(err)
//		}
//		return resolveLegacy(key, secret)
//	}
//	if err != nil {
//		return ez.Wrap(err)
//	}
func (r StandardRequest) LegacyCredentials(keyHeader, secretHeader string) (string, string, error) {
	key := r.Header.Get(keyHeader)
	secret := r.Header.Get(secretHeader)
	if key == "" && secret == "" {
		return "", "", ez.New(ez.ENOTAUTHENTICATED, "Request has no credentials", nil)
	}
	if key == "" {
		return "", "", ez.New(ez.EINVALID, "Request is missing "+keyHeader+" authentication header", nil)
	}
	if secret == "" {
		return "", "", ez.New(ez.EINVALID, "Request is missing "+secretHeader+" authentication header", nil)
	}
	return key, secret, nil
}

// checkCookieOrigin applies the CSRF rule of WithCookie. GET, HEAD and
// OPTIONS pass; any other method, including an empty one, needs an Origin
// listed in allowedOrigins.
func checkCookieOrigin(r StandardRequest, allowedOrigins []string) error {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return nil
	}
	origin := r.Header.Get("Origin")
	if origin == "" || !slices.Contains(allowedOrigins, origin) {
		return ez.New(ez.ENOTAUTHORIZED, "Origin is not allowed for cookie authentication", nil)
	}
	return nil
}

// ParseBearer returns the token of a well-formed "Bearer <token>"
// Authorization header. It is the single parser for the module; mcp uses it
// too. The scheme compares case-insensitively and is separated from the
// non-empty token by one or more spaces or tabs, as RFC 7235 allows. The
// token itself contains no whitespace. An absent header is
// ENOTAUTHENTICATED; a present but malformed one, or more than one, is
// EINVALID, so callers can tell "no credentials" from "bad credentials" with
// ez.ErrorCode.
func ParseBearer(header http.Header) (string, error) {
	values := header.Values("Authorization")
	if len(values) == 0 {
		return "", ez.New(ez.ENOTAUTHENTICATED, "Request has no Authorization header", nil)
	}

	malformed := ez.New(ez.EINVALID, "Authorization header must be Bearer <token>", nil)
	if len(values) > 1 {
		return "", malformed
	}
	fields := strings.FieldsFunc(values[0], isBlank)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return "", malformed
	}
	token := fields[1]
	if strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return "", malformed
	}
	return token, nil
}

// isBlank reports the whitespace RFC 7235 allows between the scheme and the
// token: space and horizontal tab.
func isBlank(r rune) bool {
	return r == ' ' || r == '\t'
}
