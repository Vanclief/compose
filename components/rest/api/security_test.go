package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/internal/fixture"
	"github.com/vanclief/compose/components/rest/reference"
)

// securityRegistry registers one protected and one public operation under
// the given registry options.
func securityRegistry(opts ...api.Option) *api.Registry {
	opts = append(opts, api.Authenticate((&fixture.App{}).Authenticate))
	reg := api.New(newEcho(), opts...)
	api.GET[fixture.GetRequest, fixture.Run](reg.Group("/runs/:id"), "", "get", stub)
	api.GET[fixture.HealthRequest, fixture.HealthResponse](reg.Group("/health"), "", "check", stub, api.Public())
	return reg
}

// securityDocuments returns the 3.1, the 3.0.3 and the strict document.
func securityDocuments(t *testing.T, reg *api.Registry) map[string]map[string]any {
	t.Helper()
	v31, err := reg.Spec(api.V31)
	require.NoError(t, err)
	v30, err := reg.Spec(api.V30)
	require.NoError(t, err)
	return map[string]map[string]any{
		"3.1":    decode(t, v31),
		"3.0.3":  decode(t, v30),
		"strict": decode(t, reg.StrictSpec()),
	}
}

// A key and its secret are required together, and a bearer token is an
// alternative to the pair, in every document.
func TestSecurityPairAndBearer(t *testing.T) {
	reg := securityRegistry(api.Security(
		api.SecurityScheme{Name: "BearerAuth", Type: "http", Scheme: "bearer", BearerFormat: "opaque", Description: "An API key."},
		api.SecurityScheme{Name: "ApiKeyAuth", Header: "API-KEY"},
		api.SecurityScheme{Name: "ApiSecretAuth", Type: "apiKey", Header: "API-SECRET", Description: "The key's secret."},
	))

	for name, doc := range securityDocuments(t, reg) {
		// The pair comes first whatever the order given, as the generator
		// writes it.
		require.Equal(t, []any{
			map[string]any{"ApiKeyAuth": []any{}, "ApiSecretAuth": []any{}},
			map[string]any{"BearerAuth": []any{}},
		}, dig(t, doc, "paths", "/runs/{id}", "get", "security"), name)
		require.NotContains(t, dig(t, doc, "paths", "/health", "get"), "security", name)
		require.NotContains(t, doc, "security", name)

		require.Equal(t, map[string]any{
			"ApiKeyAuth":    map[string]any{"type": "apiKey", "in": "header", "name": "API-KEY"},
			"ApiSecretAuth": map[string]any{"type": "apiKey", "in": "header", "name": "API-SECRET", "description": "The key's secret."},
			"BearerAuth":    map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "opaque", "description": "An API key."},
		}, dig(t, doc, "components", "securitySchemes"), name)
	}
}

// Without an apiKey scheme each http scheme is an alternative on its own.
func TestSecurityBearerOnly(t *testing.T) {
	reg := securityRegistry(api.Security(
		api.SecurityScheme{Name: "BearerAuth", Type: "http", Scheme: "bearer"},
		api.SecurityScheme{Name: "BasicAuth", Type: "http", Scheme: "basic"},
	))

	for name, doc := range securityDocuments(t, reg) {
		require.Equal(t, []any{
			map[string]any{"BearerAuth": []any{}},
			map[string]any{"BasicAuth": []any{}},
		}, dig(t, doc, "paths", "/runs/{id}", "get", "security"), name)
		require.Equal(t, map[string]any{"type": "http", "scheme": "bearer"}, dig(t, doc, "components", "securitySchemes", "BearerAuth"), name)
	}
}

// APIKeyHeader is the short form of one apiKey scheme named apiKey.
func TestSecurityMatchesAPIKeyHeader(t *testing.T) {
	short := securityRegistry(api.APIKeyHeader("API-KEY"))
	long := securityRegistry(api.Security(api.SecurityScheme{Name: "apiKey", Header: "API-KEY"}))

	for _, version := range []api.Version{api.V31, api.V30} {
		want, err := short.Spec(version)
		require.NoError(t, err)
		got, err := long.Spec(version)
		require.NoError(t, err)
		require.JSONEq(t, string(want), string(got))
	}
	require.JSONEq(t, string(short.StrictSpec()), string(long.StrictSpec()))
}

// The move the option exists for: a document that required one header and
// now requires two reads as breaking in the diff, and one that only adds
// the bearer alternative does not.
func TestSecurityDiff(t *testing.T) {
	spec := func(opts ...api.Option) *reference.Document {
		raw, err := securityRegistry(opts...).Spec(api.V31)
		require.NoError(t, err)
		doc, err := reference.Parse(raw)
		require.NoError(t, err)
		return doc
	}
	key := api.SecurityScheme{Name: "apiKey", Header: "API-KEY"}
	secret := api.SecurityScheme{Name: "apiSecret", Header: "API-SECRET"}
	bearer := api.SecurityScheme{Name: "bearer", Type: "http", Scheme: "bearer"}

	one := spec(api.APIKeyHeader("API-KEY"))
	require.True(t, reference.Diff(one, spec(api.Security(key, secret))).Breaking())
	require.False(t, reference.Diff(one, spec(api.Security(key, bearer))).Breaking())
	require.Empty(t, reference.Diff(one, spec(api.Security(key))).Changes)

	// A scheme is known by its name: the same header under another name
	// reads as replaced, so an app keeps the names its document has.
	renamed := api.SecurityScheme{Name: "ApiKeyAuth", Header: "API-KEY"}
	require.True(t, reference.Diff(one, spec(api.Security(renamed))).Breaking())
}

func TestSecurityFailures(t *testing.T) {
	key := api.SecurityScheme{Name: "ApiKeyAuth", Header: "API-KEY"}

	tests := []struct {
		fn   func()
		want string
	}{
		{func() { api.Security() }, "api: Security: needs a scheme"},
		{func() { api.Security(api.SecurityScheme{Header: "API-KEY"}) }, `api: Security: scheme name "" must match ^[A-Za-z0-9._-]+$`},
		{func() { api.Security(api.SecurityScheme{Name: "api key", Header: "API-KEY"}) }, `api: Security: scheme name "api key" must match ^[A-Za-z0-9._-]+$`},
		{func() { api.Security(key, api.SecurityScheme{Name: "ApiKeyAuth", Header: "API-SECRET"}) }, "api: Security: scheme ApiKeyAuth is declared twice"},
		{func() { api.Security(key, api.SecurityScheme{Name: "ApiKeyAuth", Type: "http", Scheme: "bearer"}) }, "api: Security: scheme ApiKeyAuth is declared twice"},
		{func() { api.Security(api.SecurityScheme{Name: "ApiKeyAuth"}) }, "api: Security: scheme ApiKeyAuth of type apiKey needs a Header"},
		// A bearer scheme whose Type was left out is not published as an API key.
		{func() { api.Security(api.SecurityScheme{Name: "BearerAuth", Scheme: "bearer"}) }, "api: Security: scheme BearerAuth of type apiKey needs a Header"},
		{func() { api.Security(api.SecurityScheme{Name: "BearerAuth", Header: "API-KEY", Scheme: "bearer"}) }, "api: Security: scheme BearerAuth of type apiKey sets Scheme or BearerFormat"},
		{func() {
			api.Security(api.SecurityScheme{Name: "ApiKeyAuth", Header: "API-KEY", BearerFormat: "opaque"})
		}, "api: Security: scheme ApiKeyAuth of type apiKey sets Scheme or BearerFormat"},
		{func() { api.Security(key, api.SecurityScheme{Name: "ApiSecretAuth", Header: "Api-Key"}) }, "api: Security: schemes ApiKeyAuth and ApiSecretAuth name the same header Api-Key"},
		{func() { api.Security(api.SecurityScheme{Name: "BearerAuth", Type: "http"}) }, "api: Security: scheme BearerAuth of type http needs a Scheme"},
		{func() {
			api.Security(api.SecurityScheme{Name: "BearerAuth", Type: "http", Scheme: "bearer", Header: "Authorization"})
		}, "api: Security: scheme BearerAuth of type http sets Header"},
		{func() { api.Security(api.SecurityScheme{Name: "OAuth", Type: "oauth2"}) }, `api: Security: scheme OAuth has unknown type "oauth2": use "apiKey" or "http"`},
		{func() { api.New(newEcho(), api.APIKeyHeader("API-KEY"), api.Security(key)) }, "api: New: api.APIKeyHeader and api.Security cannot both be passed"},
		{func() { api.New(newEcho(), api.Security(key), api.APIKeyHeader("API-KEY")) }, "api: New: api.APIKeyHeader and api.Security cannot both be passed"},
		{func() { api.New(newEcho(), api.Security(key), api.Security(key)) }, "api: New: api.Security is passed twice: declare every scheme in one call"},
		{func() { api.New(newEcho()).Group("/runs", api.Security(key)) }, "api.Security is a registry option: pass it to api.New, not to a group"},
	}
	for _, tt := range tests {
		msg := panicMessage(t, tt.fn)
		require.Contains(t, msg, tt.want)
	}

	// The schemes are copied: changing the caller's slice afterwards does
	// not change the document.
	schemes := []api.SecurityScheme{key}
	opt := api.Security(schemes...)
	schemes[0].Header = "OTHER"
	doc := securityDocuments(t, securityRegistry(opt))["3.1"]
	require.Equal(t, "API-KEY", dig(t, doc, "components", "securitySchemes", "ApiKeyAuth", "name"))
}
