package oauth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	composemcp "github.com/vanclief/compose/components/mcp"
)

// accountsSpec is a one-function REST API: accounts.me reports the
// credentials it was called with.
const accountsSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "Acme", "version": "1.0"},
  "tags": [{"name": "Accounts", "description": "Who is calling."}],
  "paths": {
    "/me": {
      "get": {
        "operationId": "accounts.me",
        "tags": ["Accounts"],
        "summary": "The account of the credentials",
        "responses": {"200": {"description": "OK", "content": {"application/json": {"schema": {"type": "object"}}}}}
      }
    }
  }
}`

func accountsAPI() http.Handler {
	e := echo.New()
	e.GET("/me", func(c echo.Context) error {
		header := c.Request().Header
		return c.JSON(http.StatusOK, map[string]string{"key": header.Get("API-KEY"), "secret": header.Get("API-SECRET")})
	})
	return e
}

// newMCPEnv mounts a compose MCP server over accountsAPI behind the
// authorization server.
func newMCPEnv(t *testing.T, mutate func(*Config)) *testEnv {
	t.Helper()
	return newEnv(t, mutate, func(s *Server) http.Handler {
		server, err := composemcp.New(composemcp.Config{
			Spec:    []byte(accountsSpec),
			Handler: accountsAPI(),
			Auth:    s.AuthFunc(),
			WhoAmI:  "accounts.me",
		})
		require.NoError(t, err)
		return server.Handler()
	})
}

// bearerTransport adds the bearer token to every request.
type bearerTransport struct {
	token string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (env *testEnv) connect(token string) (*mcp.ClientSession, error) {
	hc := &http.Client{Transport: bearerTransport{token: token}}
	client := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: env.url + "/mcp", HTTPClient: hc}, nil)
}

// callText calls a tool and returns its text.
func callText(t *testing.T, session *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	require.False(t, res.IsError)
	require.Len(t, res.Content, 1)
	return res.Content[0].(*mcp.TextContent).Text
}

func TestMCPRequiresToken(t *testing.T) {
	env := newMCPEnv(t, nil)

	_, err := env.connect("")
	assert.Error(t, err)

	res := env.bearer(t, "")
	assert.Equal(t, http.StatusUnauthorized, res.status)
	assert.Equal(t, `Bearer resource_metadata="`+env.url+`/.well-known/oauth-protected-resource/mcp"`, res.header.Get("WWW-Authenticate"))

	// Without Fallback a key:secret token is not accepted.
	_, err = env.connect("key:secret")
	assert.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, env.bearer(t, "key:secret").status)
}

func TestMCPWithAccessToken(t *testing.T) {
	env := newMCPEnv(t, nil)
	access, _ := env.tokens(t)

	session, err := env.connect(access)
	require.NoError(t, err)
	defer session.Close()

	assert.JSONEq(t, `{"key":"k1","secret":"s1"}`, callText(t, session, "whoami", nil))
	assert.JSONEq(t, `{"key":"k1","secret":"s1"}`, callText(t, session, "call_function", map[string]any{"name": "accounts.me"}))
}

func TestMCPWithFallback(t *testing.T) {
	env := newMCPEnv(t, func(c *Config) {
		c.Fallback = composemcp.BearerToHeaders("API-KEY", "API-SECRET")
	})

	session, err := env.connect("key:secret")
	require.NoError(t, err)
	defer session.Close()
	assert.JSONEq(t, `{"key":"key","secret":"secret"}`, callText(t, session, "whoami", nil))

	_, err = env.connect("nocolon")
	assert.Error(t, err)
}

// TestMCPSDKClient runs the official SDK OAuth client against the server:
// discovery from the 401 challenge, dynamic registration, the built-in login
// page, the code exchange with PKCE and the resource parameter.
func TestMCPSDKClient(t *testing.T) {
	env := newMCPEnv(t, nil)
	redirect := "http://127.0.0.1:1/callback"

	fetch := func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
		require.NoError(t, err)
		res := env.do(t, req)
		require.Equal(t, http.StatusFound, res.status, res.body)
		loc := res.location(t)
		page := env.get(t, loc.RequestURI())
		require.Contains(t, page.body, `SDK Client</span> at <span class="host">127.0.0.1:1</span> wants to access your Acme account.`)
		res = env.login(t, loc.Query().Get("request"), "ana@acme.com", "pw")
		require.Equal(t, http.StatusSeeOther, res.status, res.body)
		back := res.location(t)
		require.True(t, strings.HasPrefix(back.String(), redirect+"?"))
		return &auth.AuthorizationResult{Code: back.Query().Get("code"), State: back.Query().Get("state")}, nil
	}
	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{
			Metadata: &oauthex.ClientRegistrationMetadata{ClientName: "SDK Client", RedirectURIs: []string{redirect}},
		},
		AuthorizationCodeFetcher: fetch,
	})
	require.NoError(t, err)

	client := mcp.NewClient(&mcp.Implementation{Name: "sdk"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: env.url + "/mcp", OAuthHandler: handler}, nil)
	require.NoError(t, err)
	defer session.Close()
	assert.JSONEq(t, `{"key":"k1","secret":"s1"}`, callText(t, session, "whoami", nil))
}
