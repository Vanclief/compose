package requests

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// authRequest builds a request with the given method and header pairs.
func authRequest(t *testing.T, method string, pairs ...string) *StandardRequest {
	t.Helper()
	header := http.Header{}
	for i := 0; i < len(pairs); i += 2 {
		header.Add(pairs[i], pairs[i+1])
	}
	request := New(header, "127.0.0.1", WithMethod(method))
	t.Cleanup(request.Cancel)
	return request
}

func TestParseBearer(t *testing.T) {
	valid := map[string]string{
		"Bearer abc":     "abc",
		"bearer x":       "x",
		"BEARER x":       "x",
		"Bearer k:s.t-1": "k:s.t-1",
		// RFC 7235 allows one or more spaces or tabs after the scheme, and
		// the MCP SDK's bearer check accepts them, so ParseBearer must too.
		"Bearer  x":   "x",
		"Bearer\tx":   "x",
		"Bearer \t x": "x",
		" Bearer x":   "x",
		"Bearer x ":   "x",
	}
	for value, want := range valid {
		t.Run(value, func(t *testing.T) {
			token, err := ParseBearer(http.Header{"Authorization": {value}})
			require.NoError(t, err)
			assert.Equal(t, want, token)
		})
	}

	malformed := []string{"", "Bearer", "Bearer ", "Bearer x y", "Bearer x\ty", "Bearer x", "Basic abc", "abc"}
	for _, value := range malformed {
		t.Run(value, func(t *testing.T) {
			_, err := ParseBearer(http.Header{"Authorization": {value}})
			require.Error(t, err)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			assert.Equal(t, "Authorization header must be Bearer <token>", ez.ErrorMessage(err))
		})
	}

	_, err := ParseBearer(http.Header{"Authorization": {"Bearer a", "Bearer b"}})
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), "two Authorization headers are malformed")

	_, err = ParseBearer(http.Header{})
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
}

func TestAuthenticateBearer(t *testing.T) {
	auth, err := authRequest(t, http.MethodPost, "Authorization", "bearer tok").Authenticate()
	require.NoError(t, err)
	assert.Equal(t, "tok", auth.Token)
	assert.False(t, auth.FromCookie)

	_, err = authRequest(t, http.MethodGet, "Authorization", "Basic abc").Authenticate()
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

func TestAuthenticatePrefersHeaderOverCookie(t *testing.T) {
	request := authRequest(t, http.MethodPost, "Authorization", "Bearer header", "Cookie", "session=cookie")
	auth, err := request.Authenticate(WithCookie("session"))
	require.NoError(t, err)
	assert.Equal(t, "header", auth.Token)
	assert.False(t, auth.FromCookie)
}

func TestAuthenticateMalformedHeaderDoesNotFallBack(t *testing.T) {
	request := authRequest(t, http.MethodGet, "Authorization", "Bearer x y", "Cookie", "session=cookie")
	_, err := request.Authenticate(WithCookie("session"))
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Equal(t, "Authorization header must be Bearer <token>", ez.ErrorMessage(err))
}

func TestAuthenticateCookie(t *testing.T) {
	origins := []string{"https://app.example.com", "https://admin.example.com"}

	cases := []struct {
		name    string
		method  string
		origin  string
		allowed []string
		code    string
	}{
		{name: "GET without Origin", method: http.MethodGet},
		{name: "HEAD without Origin", method: http.MethodHead},
		{name: "OPTIONS without Origin", method: http.MethodOptions},
		{name: "GET with unlisted Origin", method: http.MethodGet, origin: "https://evil.example.com", allowed: origins},
		{name: "POST without Origin", method: http.MethodPost, allowed: origins, code: ez.ENOTAUTHORIZED},
		{name: "POST with listed Origin", method: http.MethodPost, origin: "https://admin.example.com", allowed: origins},
		{name: "POST with unlisted Origin", method: http.MethodPost, origin: "https://evil.example.com", allowed: origins, code: ez.ENOTAUTHORIZED},
		{name: "POST with Origin differing in case", method: http.MethodPost, origin: "https://APP.example.com", allowed: origins, code: ez.ENOTAUTHORIZED},
		{name: "POST with no allowed origins", method: http.MethodPost, origin: "https://app.example.com", code: ez.ENOTAUTHORIZED},
		{name: "DELETE with listed Origin", method: http.MethodDelete, origin: "https://app.example.com", allowed: origins},
		{name: "empty method is unsafe", method: "", allowed: origins, code: ez.ENOTAUTHORIZED},
		{name: "empty method with listed Origin", method: "", origin: "https://app.example.com", allowed: origins},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pairs := []string{"Cookie", "other=1; session=tok"}
			if tc.origin != "" {
				pairs = append(pairs, "Origin", tc.origin)
			}
			auth, err := authRequest(t, tc.method, pairs...).Authenticate(WithCookie("session", tc.allowed...))
			if tc.code != "" {
				require.Error(t, err)
				assert.Equal(t, tc.code, ez.ErrorCode(err))
				assert.Equal(t, "Origin is not allowed for cookie authentication", ez.ErrorMessage(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "tok", auth.Token)
			assert.True(t, auth.FromCookie)
		})
	}
}

func TestAuthenticateWithoutMethodOption(t *testing.T) {
	// A handler that forgets WithMethod must not bypass the CSRF rule.
	header := http.Header{"Cookie": {"session=tok"}}
	request := New(header, "127.0.0.1")
	defer request.Cancel()

	_, err := request.Authenticate(WithCookie("session"))
	assert.Equal(t, ez.ENOTAUTHORIZED, ez.ErrorCode(err))
}

func TestAuthenticateCookieAbsentFallsThrough(t *testing.T) {
	_, err := authRequest(t, http.MethodPost, "Cookie", "other=1; session=").Authenticate(WithCookie("session"))
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "an empty cookie is no credential")

	_, err = authRequest(t, http.MethodGet, "Cookie", "other=1").Authenticate(WithCookie("session"))
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
}

func TestAuthenticateIgnoresLegacyHeaders(t *testing.T) {
	request := authRequest(t, http.MethodPost, "API-KEY", "k", "API-SECRET", "s")
	_, err := request.Authenticate(WithCookie("session"))
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	assert.Equal(t, "Request has no credentials", ez.ErrorMessage(err))
}

func TestLegacyCredentials(t *testing.T) {
	cases := []struct {
		name    string
		pairs   []string
		code    string
		message string
	}{
		{name: "both present", pairs: []string{"API-KEY", "k", "API-SECRET", "s"}},
		{name: "both absent", code: ez.ENOTAUTHENTICATED, message: "Request has no credentials"},
		{name: "key only", pairs: []string{"API-KEY", "k"}, code: ez.EINVALID, message: "Request is missing API-SECRET authentication header"},
		{name: "secret only", pairs: []string{"API-SECRET", "s"}, code: ez.EINVALID, message: "Request is missing API-KEY authentication header"},
		{name: "bearer is not read", pairs: []string{"Authorization", "Bearer tok"}, code: ez.ENOTAUTHENTICATED, message: "Request has no credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, secret, err := authRequest(t, http.MethodPost, tc.pairs...).LegacyCredentials("API-KEY", "API-SECRET")
			if tc.code != "" {
				require.Error(t, err)
				assert.Equal(t, tc.code, ez.ErrorCode(err))
				assert.Equal(t, tc.message, ez.ErrorMessage(err))
				assert.Empty(t, key)
				assert.Empty(t, secret)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "k", key)
			assert.Equal(t, "s", secret)
		})
	}
}

// authenticateOrLegacy is the composition documented on LegacyCredentials.
func authenticateOrLegacy(request *StandardRequest) (string, error) {
	authReq, err := request.Authenticate()
	if ez.ErrorCode(err) == ez.ENOTAUTHENTICATED {
		key, _, err := request.LegacyCredentials("API-KEY", "API-SECRET")
		if err != nil {
			return "", ez.Wrap(err)
		}
		return "legacy:" + key, nil
	}
	if err != nil {
		return "", ez.Wrap(err)
	}
	return "bearer:" + authReq.Token, nil
}

func TestLegacyCredentialsComposition(t *testing.T) {
	source, err := authenticateOrLegacy(authRequest(t, http.MethodPost, "API-KEY", "k", "API-SECRET", "s"))
	require.NoError(t, err)
	assert.Equal(t, "legacy:k", source)

	source, err = authenticateOrLegacy(authRequest(t, http.MethodPost, "Authorization", "Bearer tok", "API-KEY", "k", "API-SECRET", "s"))
	require.NoError(t, err)
	assert.Equal(t, "bearer:tok", source)

	// A malformed Authorization header is refused even though a valid pair is
	// present: Authenticate returns EINVALID, so the pair is never read.
	source, err = authenticateOrLegacy(authRequest(t, http.MethodPost, "Authorization", "Bearer x y", "API-KEY", "k", "API-SECRET", "s"))
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Equal(t, "Authorization header must be Bearer <token>", ez.ErrorMessage(err))
	assert.Empty(t, source)
}

func TestAuthenticateNoCredentials(t *testing.T) {
	_, err := authRequest(t, http.MethodGet).Authenticate()
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	assert.Equal(t, "Request has no credentials", ez.ErrorMessage(err))
}
