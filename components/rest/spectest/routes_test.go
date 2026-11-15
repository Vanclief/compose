package spectest_test

import (
	"net/http"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"github.com/vanclief/compose/components/rest/spectest"
)

func noop(c *echo.Context) error {
	return nil
}

const routesSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/users": {"get": {"responses": {"200": {"description": "ok"}}}, "post": {"responses": {"200": {"description": "ok"}}}},
    "/users/{id}": {"get": {"responses": {"200": {"description": "ok"}}}},
    "/files/{path}": {"get": {"responses": {"200": {"description": "ok"}}}}
  }
}`

func routesEcho() *echo.Echo {
	e := echo.New()
	e.GET("/users", noop)
	e.POST("/users", noop)
	e.GET("/users/:id", noop)
	e.GET("/files/*", noop)
	return e
}

func TestRoutesMatch(t *testing.T) {
	spectest.Routes(t, routesEcho(), []byte(routesSpec))
}

func TestRoutesReportsBothDirections(t *testing.T) {
	e := routesEcho()
	e.DELETE("/users/:id", noop)
	e.PATCH("/accounts", noop)

	spec := `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/users": {"get": {"responses": {}}, "post": {"responses": {}}, "put": {"responses": {}}},
    "/users/{id}": {"get": {"responses": {}}},
    "/files/{path}": {"get": {"responses": {}}},
    "/zones": {"get": {"responses": {}}}
  }
}`

	fake := &fakeTB{TB: t}
	spectest.Routes(fake, e, []byte(spec))
	require.Len(t, fake.errors, 1)
	require.Equal(t, `spectest: the routes of Echo and the OpenAPI document differ
  in Echo, not in the document:
    PATCH /accounts
    DELETE /users/{id}
  in the document, not in Echo:
    PUT /users
    GET /zones`, fake.errors[0])
}

func TestRoutesComparesParameterNames(t *testing.T) {
	spec := `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {"/users/{user_id}": {"get": {"responses": {}}}}
}`
	e := echo.New()
	e.GET("/users/:id", noop)

	fake := &fakeTB{TB: t}
	spectest.Routes(fake, e, []byte(spec))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "in Echo, not in the document:\n    GET /users/{id}")
	require.Contains(t, fake.errors[0], "in the document, not in Echo:\n    GET /users/{user_id}")
}

func TestRoutesIgnore(t *testing.T) {
	e := routesEcho()
	e.GET("/health", noop)
	e.GET("/docs", noop)
	e.GET("/docs/assets/*", noop)
	e.Any("/mcp", noop)
	e.CONNECT("/tunnel", noop)
	e.Add("PROPFIND", "/dav/:name", noop)
	e.OPTIONS("/users", noop)

	spec := `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/users": {"get": {"responses": {}}, "post": {"responses": {}}},
    "/users/{id}": {"get": {"responses": {}}},
    "/files/{path}": {"get": {"responses": {}}},
    "/legacy": {"get": {"responses": {}}}
  }
}`

	// Unignored, every route OpenAPI cannot express shows as missing.
	fake := &fakeTB{TB: t}
	spectest.Routes(fake, e, []byte(spec))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], `in Echo, not in the document:
    PROPFIND /dav/{name}
    GET /docs
    GET /docs/assets/{path}
    GET /health
    ANY /mcp
    CONNECT /tunnel
    OPTIONS /users`)

	// Rules match both sides, in Echo's path form or OpenAPI's.
	spectest.Routes(t, e, []byte(spec),
		spectest.Ignore(http.MethodGet, "/health"),
		spectest.IgnorePrefix("/docs"),
		spectest.Ignore(spectest.AnyMethod, "/mcp"),
		spectest.Ignore(http.MethodConnect, "/tunnel"),
		spectest.Ignore("PROPFIND", "/dav/:name"),
		spectest.Ignore(http.MethodOptions, "/users"),
		spectest.Ignore(http.MethodGet, "/legacy"),
	)
}

func TestRoutesUnusedIgnoreRuleFails(t *testing.T) {
	fake := &fakeTB{TB: t}
	spectest.Routes(fake, routesEcho(), []byte(routesSpec),
		spectest.Ignore(http.MethodGet, "/users/:id"),
		spectest.Ignore(http.MethodGet, "/gone"),
		spectest.Ignore("get", "/users"),
		spectest.Ignore(http.MethodGet, "/gone/:id"),
		spectest.IgnorePrefix("/old/"),
	)
	require.Len(t, fake.errors, 1)
	// A rule shows as the test wrote it.
	require.Equal(t, `spectest: the routes of Echo and the OpenAPI document differ
  ignore rules that match no route:
    Ignore("GET", "/gone")
    Ignore("get", "/users")
    Ignore("GET", "/gone/:id")
    IgnorePrefix("/old/")`, fake.errors[0])
}

func TestRoutesLeavesOutWhatIsNotARoute(t *testing.T) {
	// Group.Use adds not-found routes, RouteNotFound adds one, and the HEAD
	// and OPTIONS answers Echo gives on its own are no routes at all.
	e := echo.NewWithConfig(echo.Config{Router: echo.NewRouter(echo.RouterConfig{AutoHandleHEAD: true})})
	users := e.Group("/users")
	users.Use(func(next echo.HandlerFunc) echo.HandlerFunc { return next })
	users.GET("", noop)
	users.POST("", noop)
	users.GET("/:id", noop)
	e.GET("/files/*", noop)
	e.RouteNotFound("/*", noop)

	spectest.Routes(t, e, []byte(routesSpec))

	// A HEAD route the app registers is compared.
	e.HEAD("/users/:id", noop)
	fake := &fakeTB{TB: t}
	spectest.Routes(fake, e, []byte(routesSpec))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "in Echo, not in the document:\n    HEAD /users/{id}")
}

func TestRoutesReadsYAMLAnd31(t *testing.T) {
	spec := `openapi: 3.1.0
info:
  title: t
  version: "1"
paths:
  /users:
    get:
      responses:
        200:
          description: ok
    post:
      responses: {}
  /users/{id}:
    get:
      responses: {}
  /files/{path}:
    get:
      responses: {}
`
	spectest.Routes(t, routesEcho(), []byte(spec))
}

func TestRoutesRejectsADocumentItCannotRead(t *testing.T) {
	fake := &fakeTB{TB: t}
	spectest.Routes(fake, routesEcho(), []byte(`{"swagger": "2.0"}`))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "spectest: read the OpenAPI document: Swagger 2.0 documents are not supported")
}

func TestRoutesIgnoreAnyMethodCoversThePath(t *testing.T) {
	e := routesEcho()
	e.Any("/mcp", noop)
	e.GET("/stream", noop)
	e.POST("/stream", noop)

	spec := `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/users": {"get": {"responses": {}}, "post": {"responses": {}}},
    "/users/{id}": {"get": {"responses": {}}},
    "/files/{path}": {"get": {"responses": {}}},
    "/mcp": {"get": {"responses": {}}, "post": {"responses": {}}}
  }
}`
	spectest.Routes(t, e, []byte(spec),
		spectest.Ignore(spectest.AnyMethod, "/mcp"),
		spectest.Ignore(spectest.AnyMethod, "/stream"),
	)
}

func TestRoutesEmptyIgnorePathFails(t *testing.T) {
	fake := &fakeTB{TB: t}
	spectest.Routes(fake, routesEcho(), []byte(routesSpec),
		spectest.IgnorePrefix(""),
		spectest.Ignore(http.MethodGet, ""),
	)
	require.Equal(t, []string{
		`spectest: IgnorePrefix(""): the path is empty; write the path or prefix of the routes to leave out`,
		`spectest: Ignore("GET", ""): the path is empty; write the path or prefix of the routes to leave out`,
	}, fake.errors)
}

func TestRoutesComparesThePathEchoServes(t *testing.T) {
	// Echo serves "" at / and "users" at /users.
	e := echo.New()
	e.GET("", noop)
	e.GET("users", noop)
	e.GET("users/:id", noop)

	spec := `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/": {"get": {"responses": {}}},
    "/users": {"get": {"responses": {}}},
    "/users/{id}": {"get": {"responses": {}}}
  }
}`
	spectest.Routes(t, e, []byte(spec))
	spectest.Routes(t, e, []byte(`{"openapi": "3.0.3", "info": {"title": "t", "version": "1"}, "paths": {}}`),
		spectest.Ignore(http.MethodGet, "/"),
		spectest.IgnorePrefix("/users"),
	)
}

func TestRoutesRulePaths(t *testing.T) {
	e := routesEcho()
	e.File("/favicon.ico", "favicon.ico")
	e.GET("/health", noop)
	e.GET("/docs/:page", noop)
	e.GET("/docs/:page/assets", noop)

	// A rule path without a leading slash is read as Echo reads one, a
	// prefix in Echo's form is put in OpenAPI form, and two rules that match
	// the same route are both used.
	spectest.Routes(t, e, []byte(routesSpec),
		spectest.Ignore(http.MethodGet, "favicon.ico"),
		spectest.Ignore(http.MethodGet, "/health"),
		spectest.IgnorePrefix("/he"),
		spectest.IgnorePrefix("docs/:page"),
	)
}

func TestRoutesSkipsANullPathItem(t *testing.T) {
	spec := `{
  "openapi": "3.0.3",
  "info": {"title": "t", "version": "1"},
  "paths": {
    "/users": {"get": {"responses": {}}, "post": {"responses": {}}},
    "/users/{id}": {"get": {"responses": {}}},
    "/files/{path}": {"get": {"responses": {}}},
    "/gone": null
  }
}`
	spectest.Routes(t, routesEcho(), []byte(spec))
}
