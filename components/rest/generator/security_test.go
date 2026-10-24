package generator

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// An http scheme is documented as bearer with its optional format, an apiKey scheme without
// a Type stays a header key, and the global requirement lists every scheme.
func TestSecuritySchemes(t *testing.T) {
	root := writeFixture(t, nestedFixture)
	cfg := nestedFixtureConfig(root)
	cfg.Security = []SecurityScheme{
		{Name: "ApiKeyAuth", Header: "API-KEY"},
		{Name: "BearerAuth", Type: "http", Scheme: "bearer", BearerFormat: "opaque", Description: "The token POST /login returns."},
		{Name: "PlainBearer", Type: "http", Scheme: "bearer"},
	}

	spec, err := Generate(cfg)
	require.NoError(t, err)

	schemes := spec.Document["components"].(map[string]any)["securitySchemes"].(map[string]any)
	require.Equal(t, map[string]any{"type": "apiKey", "in": "header", "name": "API-KEY"}, schemes["ApiKeyAuth"])
	require.Equal(t, map[string]any{"type": "http", "scheme": "bearer", "bearerFormat": "opaque", "description": "The token POST /login returns."}, schemes["BearerAuth"])
	require.Equal(t, map[string]any{"type": "http", "scheme": "bearer"}, schemes["PlainBearer"])

	wantSecurity := []any{
		map[string]any{"ApiKeyAuth": []any{}},
		map[string]any{"BearerAuth": []any{}},
		map[string]any{"PlainBearer": []any{}},
	}
	require.Equal(t, wantSecurity, spec.Document["security"])
}

// apiKey schemes share one requirement object (all sent together, first), and each http
// scheme is its own alternative requirement object, in config order.
func TestSecurityRequirementGrouping(t *testing.T) {
	tests := []struct {
		name     string
		security []SecurityScheme
		want     []any
	}{
		{
			name: "apiKey only",
			security: []SecurityScheme{
				{Name: "ApiKeyAuth", Header: "API-KEY"},
				{Name: "ApiSecretAuth", Type: "apiKey", Header: "API-SECRET"},
			},
			want: []any{
				map[string]any{"ApiKeyAuth": []any{}, "ApiSecretAuth": []any{}},
			},
		},
		{
			name: "apiKey pair and http",
			security: []SecurityScheme{
				{Name: "BearerAuth", Type: "http", Scheme: "bearer"},
				{Name: "ApiKeyAuth", Header: "API-KEY"},
				{Name: "ApiSecretAuth", Header: "API-SECRET"},
			},
			want: []any{
				map[string]any{"ApiKeyAuth": []any{}, "ApiSecretAuth": []any{}},
				map[string]any{"BearerAuth": []any{}},
			},
		},
		{
			name: "http only",
			security: []SecurityScheme{
				{Name: "BearerAuth", Type: "http", Scheme: "bearer"},
			},
			want: []any{
				map[string]any{"BearerAuth": []any{}},
			},
		},
		{
			name: "two http keep config order",
			security: []SecurityScheme{
				{Name: "UserBearer", Type: "http", Scheme: "bearer"},
				{Name: "AppBearer", Type: "http", Scheme: "bearer"},
			},
			want: []any{
				map[string]any{"UserBearer": []any{}},
				map[string]any{"AppBearer": []any{}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeFixture(t, nestedFixture)
			cfg := nestedFixtureConfig(root)
			cfg.Security = tt.security

			spec, err := Generate(cfg)
			require.NoError(t, err)
			require.Equal(t, tt.want, spec.Document["security"])
		})
	}
}

func TestSecuritySchemesInvalid(t *testing.T) {
	tests := []struct {
		name   string
		scheme SecurityScheme
	}{
		{name: "http without scheme", scheme: SecurityScheme{Name: "BearerAuth", Type: "http"}},
		{name: "unknown type", scheme: SecurityScheme{Name: "OAuth", Type: "oauth2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := nestedFixtureConfig(t.TempDir())
			cfg.Security = []SecurityScheme{tt.scheme}

			_, err := Generate(cfg)
			require.Error(t, err)
			require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		})
	}
}
