package generator

import (
	"go/build"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vanclief/ez"
)

// Echo reads a path parameter's name up to the next "/", so "-" and "." belong to it.
func TestPathParametersKeepEchoNames(t *testing.T) {
	cases := map[string]string{
		"/users/:user-id":         "/users/{user-id}",
		"/files/:name.ext":        "/files/{name.ext}",
		"/users/:user-id/files/*": "/users/{user-id}/files/{path}",
	}
	for echoPath, want := range cases {
		got := toOpenAPIPath(echoPath)
		if got != want {
			t.Errorf("toOpenAPIPath(%q) = %q, want %q", echoPath, got, want)
		}
	}

	files := map[string]string{}
	for name, contents := range requiredFixture {
		files[name] = contents
	}
	files["api/routes.go"] = `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("/invoices/:invoice-id", h.GetInvoice)
	e.POST("/files/:name.ext", h.CreateInvoice)
}
`
	root := writeFixture(t, files)
	spec, err := Generate(Config{Root: root, RoutesGlob: "api/routes.go", HandlerGlob: "api/*.go"})
	if err != nil {
		t.Fatal(err)
	}

	wantParams := map[string][]string{
		"get /invoices/{invoice-id}": {"invoice-id"},
		"post /files/{name.ext}":     {"name.ext"},
	}
	for key, want := range wantParams {
		method, path, _ := strings.Cut(key, " ")
		got := parameterNames(operation(t, spec.Document, path, method))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: expected parameters %v, got %v", key, want, got)
		}
	}
}

func routeFile(root string, name string) string {
	return filepath.ToSlash(filepath.Join(root, name))
}

// Routes are read from the code: a call in a comment or a string literal declares nothing, a
// call spread over several lines is one route, and a call in code that never runs is still a
// declaration.
func TestParseRoutesReadsOnlyCode(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"routes_things.go": `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("/live/:id", h.GetThing)
	// e.GET("/line-commented/:id", h.GetThing)

	/*
	e.GET("/block-commented/:id", h.GetThing)
	*/

	/* start
	e.DELETE("/split-comment/:id", h.DeleteThing) */

	example := ` + "`" + `
e.PATCH("/raw-string/:id", h.UpdateThing)
` + "`" + `
	_ = example

	e.POST("/multi-line/:id",
		h.CreateThing,
	)

	if false {
		e.PUT("/dead-code/:id", h.ReplaceThing)
	}
}
`,
	})

	routes, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes_*.go", HandlerGlob: "*.go"})
	if err != nil {
		t.Fatal(err)
	}

	file := routeFile(root, "routes_things.go")
	want := []Route{
		{Method: "PUT", Path: "/dead-code/:id", Namespace: "dead-code", Handler: "ReplaceThing", SourceFile: file, SourceLine: 24, flat: true},
		{Method: "GET", Path: "/live/:id", Namespace: "live", Handler: "GetThing", SourceFile: file, SourceLine: 4, flat: true},
		{Method: "POST", Path: "/multi-line/:id", Namespace: "multi-line", Handler: "CreateThing", SourceFile: file, SourceLine: 19, flat: true},
	}
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("routes:\n got %+v\nwant %+v", routes, want)
	}
}

// Groups nest, as variables or chained calls, and the root groups are known in every function;
// the handler is the first h.Namespace.Method in the arguments, wherever they are wrapped, and
// neither comments nor the body of a function literal handler count.
func TestParseRoutesReadsNestedGroupsAndHandlers(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"api/routes_widgets.go": `package api

func addWidgetRoutes(e *echo.Echo, h *Handlers) {
	v1 := e.Group("/v1")
	widgets := v1.Group("/widgets", requireAuth) // h.Ignored.Comment
	widgets.GET("", h.Widgets.List, h.Auth.Required)
	widgets.GET("/:id", requireRole("admin",
		h.Widgets.Get))
	widgets.DELETE("/:id", logDeprecated(h.Widgets.Delete)) // h.Other.Thing
	admin.GET("/stats", h.Admin.Stats)
	e.HEAD("/health", health)
}

func addMoreWidgetRoutes(h *Handlers) {
	e.Group("/v1").Group("/widgets").PUT("/:id", h.Widgets.Replace)
	e.GET("/internal/docs", func(c echo.Context) error {
		return h.App.Authenticate(c)
	})
	e.GET("/internal/one-line", func(c echo.Context) error { return h.Widgets.List(c) })
}
`,
	})

	cfg := Config{
		Root:              root,
		RoutesGlob:        "api/routes_*.go",
		HandlersFile:      "api/handlers.go",
		HandlerGlob:       "api/handler/*/*.go",
		RootGroups:        map[string]string{"e": "", "admin": "/admin"},
		DeprecatedWrapper: "logDeprecated",
		Skip: func(method string, path string) bool {
			return path == "/health" || strings.HasPrefix(path, "/internal/")
		},
	}
	routes, err := ParseRoutes(cfg)
	if err != nil {
		t.Fatal(err)
	}

	file := routeFile(root, "api/routes_widgets.go")
	want := []Route{
		{Method: "GET", Path: "/admin/stats", Namespace: "admin", Handler: "Stats", SourceFile: file, SourceLine: 10},
		{Method: "HEAD", Path: "/health", SourceFile: file, SourceLine: 11},
		{Method: "GET", Path: "/internal/docs", SourceFile: file, SourceLine: 16},
		{Method: "GET", Path: "/internal/one-line", SourceFile: file, SourceLine: 19},
		{Method: "GET", Path: "/v1/widgets", Namespace: "widgets", Handler: "List", SourceFile: file, SourceLine: 6},
		{Method: "DELETE", Path: "/v1/widgets/:id", Namespace: "widgets", Handler: "Delete", SourceFile: file, SourceLine: 9, Deprecated: true},
		{Method: "GET", Path: "/v1/widgets/:id", Namespace: "widgets", Handler: "Get", SourceFile: file, SourceLine: 7},
		{Method: "PUT", Path: "/v1/widgets/:id", Namespace: "widgets", Handler: "Replace", SourceFile: file, SourceLine: 15},
	}
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("routes:\n got %+v\nwant %+v", routes, want)
	}
}

// The handler is the argument after the path. The arguments after it are middleware, so a
// route whose handler is a function literal or a plain function is not documented as the
// middleware method that follows it.
func TestParseRoutesNeverTakesMiddlewareAsHandler(t *testing.T) {
	nested := `package api

func addRoutes(e *echo.Echo, h *Handlers) {
	e.GET("/literal", func(c echo.Context) error {
		return h.Widgets.Get(c)
	}, h.Widgets.RequireOwner)
	e.GET("/one-line", func(c echo.Context) error { return h.Widgets.Get(c) }, h.Widgets.RequireOwner)
	e.GET("/plain", health, h.Auth.Required)
	e.GET("/real", h.Widgets.List, h.Auth.Required)
}
`
	flat := `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("/literal", func(c echo.Context) error {
		return h.GetWidget(c)
	}, h.RequireAuth)
	e.GET("/plain", health, h.RequireAuth)
	e.GET("/real", h.ListWidgets, h.RequireAuth)
}
`

	cases := []struct {
		name string
		src  string
		cfg  Config
		want []string
	}{
		{
			name: "nested",
			src:  nested,
			cfg:  Config{RoutesGlob: "routes.go", HandlersFile: "handlers.go", HandlerGlob: "*.go"},
			want: []string{"GET /literal ", "GET /one-line ", "GET /plain ", "GET /real List"},
		},
		{
			name: "flat",
			src:  flat,
			cfg:  Config{RoutesGlob: "routes.go", HandlerGlob: "*.go"},
			want: []string{"GET /literal ", "GET /plain ", "GET /real ListWidgets"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeFixture(t, map[string]string{"routes.go": tc.src})
			cfg := tc.cfg
			cfg.Root = root
			cfg.RootGroups = map[string]string{"e": ""}

			_, err := ParseRoutes(cfg)
			if err == nil {
				t.Fatal("routes without a handler method were not reported")
			}
			if ez.ErrorCode(err) != ez.EINVALID {
				t.Fatalf("error code %s, want %s: %v", ez.ErrorCode(err), ez.EINVALID, err)
			}
			for _, path := range []string{"/literal", "/plain"} {
				if !strings.Contains(err.Error(), "GET "+path) {
					t.Fatalf("error does not list GET %s: %v", path, err)
				}
			}
			if strings.Contains(err.Error(), "GET /real") {
				t.Fatalf("error lists the route with a handler: %v", err)
			}

			cfg.Skip = func(method string, path string) bool {
				return path != "/real"
			}
			routes, err := ParseRoutes(cfg)
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, r := range routes {
				got = append(got, r.Method+" "+r.Path+" "+r.Handler)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("routes:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The handler is read from the code of the handler argument: a reference to the receiver
// variable written inside a string literal names no handler.
func TestParseRoutesReadsNoHandlerFromStrings(t *testing.T) {
	nested := `package api

func addRoutes(e *echo.Echo, h *Handlers) {
	e.GET("/string", withLog("see h.Other.Thing", h.Widgets.Get))
	e.GET("/raw-string", withLog(` + "`h.Other.Thing`" + `, h.Widgets.List))
	e.GET("/url", proxy("https://h.example.com/x"))
}
`
	flat := `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("/string", withLog("see h.Nothing", h.GetWidget))
	e.GET("/raw-string", withLog(` + "`h.Nothing`" + `, h.ListWidgets))
	e.GET("/url", proxy("https://h.example.com/x"))
}
`

	cases := []struct {
		name string
		src  string
		cfg  Config
		want []string
	}{
		{
			name: "nested",
			src:  nested,
			cfg:  Config{RoutesGlob: "routes.go", HandlersFile: "handlers.go", HandlerGlob: "*.go"},
			want: []string{"GET /raw-string widgets.List", "GET /string widgets.Get", "GET /url "},
		},
		{
			name: "flat",
			src:  flat,
			cfg:  Config{RoutesGlob: "routes.go", HandlerGlob: "*.go"},
			want: []string{"GET /raw-string ListWidgets", "GET /string GetWidget", "GET /url "},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := writeFixture(t, map[string]string{"routes.go": tc.src})
			cfg := tc.cfg
			cfg.Root = root

			_, err := ParseRoutes(cfg)
			if err == nil || !strings.Contains(err.Error(), "GET /url") {
				t.Fatalf("the route whose only reference is in a string was not reported as without a handler: %v", err)
			}

			cfg.Skip = func(method string, path string) bool {
				return path == "/url"
			}
			routes, err := ParseRoutes(cfg)
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, r := range routes {
				key := ""
				if r.Handler != "" {
					key = r.HandlerKey()
				}
				got = append(got, r.Method+" "+r.Path+" "+key)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("routes:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The DeprecatedWrapper marks a route deprecated when its call calls that function among the
// arguments after the path: around the handler, among the middleware, inside a function
// literal or on a later line, qualified, with type arguments or in parentheses. A comment, a
// string literal and a function whose name only contains the wrapper's do not.
func TestParseRoutesFindsDeprecatedWrapperCalls(t *testing.T) {
	source := `package api

func addWidgetRoutes(e *echo.Echo, h *Handlers) {
	e.GET("/around", logDeprecated(h.Widgets.Around))
	e.GET("/middleware", h.Widgets.Middleware, logDeprecated(nil))
	e.GET("/literal", wrap(h.Widgets.Literal, func() { logDeprecated(nil) }))
	e.GET("/later-line", h.Widgets.LaterLine,
		logDeprecated(nil))
	e.GET("/qualified", mw.logDeprecated(h.Widgets.Qualified))
	e.GET("/package", mw.Deprecated(h.Widgets.Package))
	e.GET("/package-parenthesised", (mw).Deprecated(h.Widgets.PackageParenthesised))
	e.GET("/nested-package", a.mw.Deprecated(h.Widgets.NestedPackage))
	e.GET("/other-package", xmw.Deprecated(h.Widgets.OtherPackage))
	e.GET("/generic", logDeprecated[int](h.Widgets.Generic))
	e.GET("/parenthesised", (logDeprecated)(h.Widgets.Parenthesised))
	e.GET("/spaced", logDeprecated (h.Widgets.Spaced))
	e.GET("/comment", h.Widgets.Comment /* logDeprecated(nil) */) // logDeprecated(nil)
	e.GET("/string", h.Widgets.String, mw("logDeprecated("))
	e.GET("/raw-string", h.Widgets.RawString, mw(` + "`" + `
logDeprecated(
` + "`" + `))
	e.GET("/longer-name", mylogDeprecated(h.Widgets.LongerName))
	e.GET("/value", h.Widgets.Value, logDeprecated)
	e.GET("/none", h.Widgets.None)
}
`
	paths := []string{"/around", "/middleware", "/literal", "/later-line", "/qualified", "/package", "/package-parenthesised", "/nested-package", "/other-package", "/generic", "/parenthesised", "/spaced", "/comment", "/string", "/raw-string", "/longer-name", "/value", "/none"}

	cases := []struct {
		wrapper    string
		deprecated []string
	}{
		{"logDeprecated", []string{"/around", "/middleware", "/literal", "/later-line", "/qualified", "/generic", "/parenthesised", "/spaced"}},
		{"mw.Deprecated", []string{"/package", "/package-parenthesised"}},
		{"a.mw.Deprecated", []string{"/nested-package"}},
		{"Deprecated", []string{"/package", "/package-parenthesised", "/nested-package", "/other-package"}},
		{"mw.logDeprecated", []string{"/qualified"}},
	}
	for _, test := range cases {
		root := writeFixture(t, map[string]string{"api/routes_widgets.go": source})
		cfg := Config{
			Root:              root,
			RoutesGlob:        "api/routes_*.go",
			HandlersFile:      "api/handlers.go",
			HandlerGlob:       "api/handler/*/*.go",
			RootGroups:        map[string]string{"e": ""},
			DeprecatedWrapper: test.wrapper,
		}
		routes, err := ParseRoutes(cfg)
		if err != nil {
			t.Fatal(err)
		}

		got := map[string]bool{}
		for _, route := range routes {
			got[route.Path] = route.Deprecated
		}
		want := map[string]bool{}
		for _, path := range paths {
			want[path] = false
		}
		for _, path := range test.deprecated {
			want[path] = true
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("wrapper %s: deprecated routes:\n got %v\nwant %v", test.wrapper, got, want)
		}
	}
}

// In flat mode the namespace is the first path segment below the route's root group, and a
// flat handler before a nested middleware reference is the handler.
func TestParseRoutesFlatNamespacesFromRootGroups(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"routes.go": `package api

func addRoutes(api *echo.Group, h *Handler) {
	items := api.Group("/items")
	items.GET("/:id", h.GetItem, h.Auth.Required)
	api.GET("/:id", h.GetRoot)
}
`,
	})

	routes, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes.go", HandlerGlob: "*.go", RootGroups: map[string]string{"api": "/api"}})
	if err != nil {
		t.Fatal(err)
	}

	file := routeFile(root, "routes.go")
	want := []Route{
		{Method: "GET", Path: "/api/:id", Handler: "GetRoot", SourceFile: file, SourceLine: 6, flat: true},
		{Method: "GET", Path: "/api/items/:id", Namespace: "items", Handler: "GetItem", SourceFile: file, SourceLine: 5, flat: true},
	}
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("routes:\n got %+v\nwant %+v", routes, want)
	}
}

// A path or group prefix is any string constant: a literal, a constant of the route file's
// package declared in any of its files, or such strings joined with +. A route or a group can
// be registered on a chain of Group calls.
func TestParseRoutesResolvesConstantPathsAndGroupChains(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"api/routes.go": `package api

const p = "/const"

const (
	prefix = "/pre"
	nested = prefix + "/nested"
	same
)

func addRoutes(e *echo.Echo, h *Handler) {
	g := e.Group("/g")
	g.GET("/literal", h.GetThing)
	g.GET(p, h.GetThing)
	g.GET("/a"+"/b", h.GetThing)
	g.GET(prefix+"/b", h.GetThing)
	g.POST((nested), h.GetThing)
	g.PUT(same, h.GetThing)
	g.GET(other+"/x", h.GetThing)
	e.Group("/v1").GET("/y", h.GetThing)
	x := e.Group("/v1").Group("/x")
	x.GET("/y", h.GetThing)
	v2 := e.Group(prefix)
	v2.GET("/z", h.GetThing)
	(e.Group(p).Group(other)).DELETE("/chain", h.GetThing)
}
`,
		"api/consts.go": "package api\n\nconst other = \"/other\"\n",
	})

	routes, err := ParseRoutes(Config{Root: root, RoutesGlob: "api/routes.go", HandlerGlob: "api/*.go"})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, r := range routes {
		got = append(got, r.Method+" "+r.Path+" "+r.Namespace)
	}
	want := []string{
		"DELETE /const/other/chain const",
		"GET /g/a/b g",
		"GET /g/const g",
		"GET /g/literal g",
		"GET /g/other/x g",
		"GET /g/pre/b g",
		"POST /g/pre/nested g",
		"PUT /g/pre/nested g",
		"GET /pre/z pre",
		"GET /v1/x/y v1",
		"GET /v1/y v1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes:\n got %q\nwant %q", got, want)
	}
}

// A route call whose path is not a string constant, or whose group the finder cannot follow,
// fails generation with its file and line and what was not resolved, unless SkipUnresolved
// leaves it out.
func TestParseRoutesReportsUnresolvedRoutes(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"api/routes.go": `package api

var dynamic = "/dynamic"

func addRoutes(e *echo.Echo, h *Handler, cfg Config) {
	e.GET(dynamic, h.GetThing)
	e.GET(cfg.HealthPath, h.GetThing)
	e.GET(fmt.Sprintf("/%d", 1), h.GetThing)
	e.GET(testOnly, h.GetThing)
	s.api.GET("/x", h.GetThing)
	newGroup(e).GET("/y", h.GetThing)
	e.Group(dynamic).POST("/z", h.GetThing)
	e.GET("/ok", h.GetThing)
}
`,
		"api/consts_test.go": "package api\n\nconst testOnly = \"/test-only\"\n",
	})
	cfg := Config{Root: root, RoutesGlob: "api/routes.go", HandlerGlob: "api/*.go"}

	_, err := ParseRoutes(cfg)
	if err == nil {
		t.Fatal("unresolved routes were not reported")
	}
	if ez.ErrorCode(err) != ez.EINVALID {
		t.Fatalf("error code %s, want %s: %v", ez.ErrorCode(err), ez.EINVALID, err)
	}
	file := routeFile(root, "api/routes.go")
	for _, line := range []string{
		file + `:6: e.GET(dynamic, ...): path dynamic is not a string constant`,
		file + `:7: e.GET(cfg.HealthPath, ...): path cfg.HealthPath is not a string constant`,
		file + `:8: e.GET(fmt.Sprintf("/%d", 1), ...): path fmt.Sprintf("/%d", 1) is not a string constant`,
		file + `:9: e.GET(testOnly, ...): path testOnly is not a string constant`,
		file + `:10: s.api.GET("/x", ...): s.api is not a group the route finder can follow`,
		file + `:11: newGroup(e).GET("/y", ...): newGroup(e) is not a group the route finder can follow`,
		file + `:12: e.Group(dynamic).POST("/z", ...): group prefix dynamic is not a string constant`,
		"SkipUnresolved",
	} {
		if !strings.Contains(filepath.ToSlash(ez.ErrorMessage(err)), line) {
			t.Errorf("error does not contain %q: %v", line, err)
		}
	}
	if strings.Contains(err.Error(), "/ok") {
		t.Errorf("error lists the resolved route: %v", err)
	}

	var skipped []UnresolvedRoute
	cfg.SkipUnresolved = func(route UnresolvedRoute) bool {
		skipped = append(skipped, route)
		return true
	}
	routes, err := ParseRoutes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Path != "/ok" {
		t.Fatalf("routes: %+v, want only GET /ok", routes)
	}
	wantSkipped := []UnresolvedRoute{
		{Method: "GET", Receiver: "e", Path: "dynamic", File: "api/routes.go", Line: 6, Reason: "path dynamic is not a string constant"},
		{Method: "GET", Receiver: "e", Path: "cfg.HealthPath", File: "api/routes.go", Line: 7, Reason: "path cfg.HealthPath is not a string constant"},
		{Method: "GET", Receiver: "e", Path: `fmt.Sprintf("/%d", 1)`, File: "api/routes.go", Line: 8, Reason: `path fmt.Sprintf("/%d", 1) is not a string constant`},
		{Method: "GET", Receiver: "e", Path: "testOnly", File: "api/routes.go", Line: 9, Reason: "path testOnly is not a string constant"},
		{Method: "GET", Receiver: "s.api", Path: `"/x"`, File: "api/routes.go", Line: 10, Reason: "s.api is not a group the route finder can follow"},
		{Method: "GET", Receiver: "newGroup(e)", Path: `"/y"`, File: "api/routes.go", Line: 11, Reason: "newGroup(e) is not a group the route finder can follow"},
		{Method: "POST", Receiver: "e.Group(dynamic)", Path: `"/z"`, File: "api/routes.go", Line: 12, Reason: "group prefix dynamic is not a string constant"},
	}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Fatalf("SkipUnresolved received:\n got %+v\nwant %+v", skipped, wantSkipped)
	}
}

// lineOf is the line number of the first line of src containing marker.
func lineOf(t *testing.T, src string, marker string) int {
	t.Helper()

	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, marker) {
			return i + 1
		}
	}
	t.Fatalf("no line contains %q", marker)

	return 0
}

// Group variables follow Go's scopes: a declaration in a block, an if, a loop or another
// function is not seen outside it, an inner scope sees the outer ones, and `=` in straight-line
// code changes the prefix from that statement on. A function constant shadows a package one.
func TestParseRoutesTracksGroupsPerScope(t *testing.T) {
	src := `package api

const local = "/package"

func addRoutes(e *echo.Echo, h *Handler) {
	g := e.Group("/a")
	{
		g := g.Group("/b")
		g.GET("/x", h.GetThing)
	}
	g.GET("/y", h.GetThing)
	if g := e.Group("/if"); g != nil {
		g.GET("/x", h.GetThing)
	}
	g.GET("/after-if", h.GetThing)
	for _, v := range []string{"/b"} {
		g := g.Group("/loop")
		g.GET("/x", h.GetThing)
		_ = v
	}
	g.GET("/after-loop", h.GetThing)

	v := e.Group("/v1")
	v.GET("/one", h.GetThing)
	v = e.Group("/v2")
	v.GET("/two", h.GetThing)
	v = v.Group("/sub")
	v.GET("/three", h.GetThing)

	const local = "/local"
	v.GET(local, h.GetThing)

	if true {
		x := e.Group("/branch-local")
		x = x.Group("/more")
		x.GET("/x", h.GetThing)
	}
	func() {
		g.GET("/closure", h.GetThing)
	}()
	e = echo.New()
	e.GET("/root", h.GetThing)
}

func other(e *echo.Echo, h *Handler, admin bool) {
	g := e.Group("/other")
	g.GET("/z", h.GetThing)
	g.GET(local, h.GetThing)
	if admin {
		g = e.Group("/admin")
		g.GET("/in-branch", h.GetThing)
	} else {
		g.GET("/in-else", h.GetThing)
	}
	same := e.Group("/same")
	if admin {
		same = e.Group("/same")
	}
	same.GET("/after", h.GetThing)
}
`
	root := writeFixture(t, map[string]string{"routes.go": src})

	routes, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes.go", HandlerGlob: "*.go"})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, r := range routes {
		got = append(got, r.Method+" "+r.Path)
	}
	want := []string{
		"GET /a/after-if",
		"GET /a/after-loop",
		"GET /a/b/x",
		"GET /a/closure",
		"GET /a/loop/x",
		"GET /a/y",
		"GET /admin/in-branch",
		"GET /branch-local/more/x",
		"GET /if/x",
		"GET /other/in-else",
		"GET /other/package",
		"GET /other/z",
		"GET /root",
		"GET /same/after",
		"GET /v1/one",
		"GET /v2/sub/local",
		"GET /v2/sub/three",
		"GET /v2/two",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes:\n got %q\nwant %q", got, want)
	}
}

// A group variable whose group the finder cannot know at the route fails generation: one
// reassigned in a branch and used after it, one reassigned in a loop it is used in, one a
// function literal uses while its function reassigns it, a parameter not named in RootGroups,
// one set from a call, and one declared in another function.
func TestParseRoutesReportsGroupsOfUnknownValue(t *testing.T) {
	src := `package api

func branch(e *echo.Echo, h *Handler, admin bool) {
	g := e.Group("/a")
	if admin {
		g = g.Group("/b") // branch assignment
	}
	g.GET("/after-branch", h.GetThing)
}

func loop(e *echo.Echo, h *Handler) {
	g := e.Group("/a")
	for i := 0; i < 2; i++ {
		g.GET("/in-loop", h.GetThing)
		g = g.Group("/b") // loop assignment
	}
	g.GET("/after-loop", h.GetThing)
}

func closure(e *echo.Echo, h *Handler) {
	g := e.Group("/a")
	register := func() {
		g.GET("/closure", h.GetThing)
	}
	g = e.Group("/b")
	register()
}

func param(g *echo.Group, h *Handler) {
	g.GET("/param", h.GetThing)
}

func fromCall(e *echo.Echo, h *Handler) {
	g := newGroup(e)
	g.GET("/call", h.GetThing)
}

func leak(h *Handler) {
	g.GET("/leak", h.GetThing)
}
`
	root := writeFixture(t, map[string]string{"routes.go": src})
	cfg := Config{Root: root, RoutesGlob: "routes.go", HandlerGlob: "*.go"}

	_, err := ParseRoutes(cfg)
	if err == nil || ez.ErrorCode(err) != ez.EINVALID {
		t.Fatalf("expected an invalid error, got %v", err)
	}

	got := map[string]string{}
	cfg.SkipUnresolved = func(route UnresolvedRoute) bool {
		got[route.Path] = route.Reason
		return true
	}
	_, err = ParseRoutes(cfg)
	if err == nil || ez.ErrorCode(err) != ez.ENOTFOUND {
		t.Fatalf("expected no routes once every unresolved one is skipped, got %v", err)
	}

	// A reason names the file relative to Root, so it does not depend on the machine.
	file := "routes.go"
	branchLine := strconv.Itoa(lineOf(t, src, "branch assignment"))
	loopLine := strconv.Itoa(lineOf(t, src, "loop assignment"))
	want := map[string]string{
		`"/after-branch"`: `"g" is reassigned at ` + file + ":" + branchLine + ` in a branch, a loop or a function literal, so the group it holds here is not known`,
		`"/in-loop"`:      `"g" is reassigned at ` + file + ":" + loopLine + ` in a branch, a loop or a function literal, so the group it holds here is not known`,
		`"/after-loop"`:   `"g" is reassigned at ` + file + ":" + loopLine + ` in a branch, a loop or a function literal, so the group it holds here is not known`,
		`"/closure"`:      `"g" is used in a function literal and reassigned in its function, so the group it holds when the literal runs is not known`,
		`"/param"`:        `unknown route group "g", a function parameter not named in RootGroups`,
		`"/call"`:         `newGroup(e) is not a group the route finder can follow`,
		`"/leak"`:         `unknown route group "g"`,
	}
	for path, reason := range got {
		got[path] = filepath.ToSlash(reason)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unresolved routes:\n got %q\nwant %q", got, want)
	}
}

// The finder never documents a group a variable may not hold at the route: a body left early
// by break, continue, return or fallthrough, a goto, a pointer to the variable and a route
// method used as a value make the route unresolved. Right-hand sides are read before any
// variable is set, and a RootGroups name set to a Group call or a variable whose group the
// finder cannot tell, such as a parameter a branch may have set to a group, is not taken for its
// root group, nor is one that is a range variable, a type switch variable or a named result.
func TestParseRoutesKnowsNoGroupAVariableMayNotHold(t *testing.T) {
	runRouteCases(t, []routeCase{
		{
			name: "break",
			body: `func f(e *echo.Echo, h *Handler, xs []bool) {
	g := e.Group("/v1")
	for _, x := range xs {
		g = e.Group("/v2")
		if x {
			break
		}
		g = e.Group("/v1")
	}
	g.GET("/after", h.GetThing)
}`,
			unresolved: []string{`g.GET("/after")`},
		},
		{
			name: "continue",
			body: `func f(e *echo.Echo, h *Handler, xs []bool) {
	g := e.Group("/v1")
	for _, x := range xs {
		g.GET("/in-loop", h.GetThing)
		g = e.Group("/v2")
		if x {
			continue
		}
		g = e.Group("/v1")
	}
}`,
			unresolved: []string{`g.GET("/in-loop")`},
		},
		{
			name: "return from a function literal",
			body: `func f(e *echo.Echo, h *Handler, c bool) {
	g := e.Group("/v1")
	func() {
		g = e.Group("/v2")
		if c {
			return
		}
		g = e.Group("/v1")
	}()
	g.GET("/after-literal", h.GetThing)
}`,
			unresolved: []string{`g.GET("/after-literal")`},
		},
		{
			name: "break out of a switch",
			body: `func f(e *echo.Echo, h *Handler, a bool, b bool) {
	g := e.Group("/v1")
	switch {
	case a:
		g = e.Group("/v2")
		if b {
			break
		}
		g = e.Group("/v1")
	}
	g.GET("/x", h.GetThing)
}`,
			unresolved: []string{`g.GET("/x")`},
		},
		{
			name: "fallthrough",
			body: `func f(e *echo.Echo, h *Handler, x int) {
	g := e.Group("/v1")
	switch x {
	case 1:
		g = e.Group("/v2")
		fallthrough
	case 2:
		g.GET("/ft", h.GetThing)
	}
}`,
			unresolved: []string{`g.GET("/ft")`},
		},
		{
			name: "goto forward",
			body: `func f(e *echo.Echo, h *Handler, skip bool) {
	g := e.Group("/v1")
	if skip {
		goto L
	}
	g = e.Group("/v2")
L:
	g.GET("/goto", h.GetThing)
}`,
			unresolved: []string{`g.GET("/goto")`},
		},
		{
			name: "goto backward",
			body: `func f(e *echo.Echo, h *Handler, more bool) {
	g := e.Group("/v1")
again:
	g.GET("/again", h.GetThing)
	if more {
		g = e.Group("/v2")
		goto again
	}
}`,
			unresolved: []string{`g.GET("/again")`},
		},
		{
			name: "swap",
			body: `func f(e *echo.Echo, h *Handler) {
	a := e.Group("/a")
	b := e.Group("/b")
	a, b = b, a
	b.GET("/x", h.GetThing)
}`,
			routes: []string{"GET /a/x"},
		},
		{
			name: "define reads the outer variable",
			body: `func f(e *echo.Echo, h *Handler) {
	a := e.Group("/outer")
	{
		a, b := e.Group("/inner"), a.Group("/b")
		b.GET("/x", h.GetThing)
		a.GET("/y", h.GetThing)
	}
}`,
			routes: []string{"GET /inner/y", "GET /outer/b/x"},
		},
		{
			name: "root name set to a group of an unknown prefix",
			body: `func f(e *echo.Echo, h *Handler, prefix string) {
	api := e.Group(prefix)
	api.GET("/p", h.GetThing)
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			unresolved: []string{`api.GET("/p")`},
		},
		{
			name: "root name set to a group of an unknown parent",
			body: `func f(srv *echo.Echo, h *Handler) {
	api := srv.Group("/v9")
	api.GET("/rn2", h.GetThing)
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			unresolved: []string{`api.GET("/rn2")`},
		},
		{
			name: "root name set to a variable of an unknown group",
			body: `func f(e *echo.Echo, h *Handler, c bool) {
	g := e.Group("/a")
	if c {
		g = e.Group("/b")
	}
	{
		api := g
		api.GET("/alias", h.GetThing)
	}
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			unresolved: []string{`api.GET("/alias")`},
		},
		{
			name: "root name set to a parameter a branch may have set to a group",
			body: `func f(e *echo.Echo, srv *echo.Group, h *Handler, c bool) {
	if c {
		srv = e.Group("/x")
	}
	api := srv
	api.GET("/y", h.GetThing)
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			unresolved: []string{`api.GET("/y")`},
		},
		{
			name: "root name set in a branch to a group of an unknown prefix",
			body: `func f(e *echo.Echo, api *echo.Group, h *Handler, c bool, prefix string) {
	if c {
		api = e.Group(prefix)
	}
	api.GET("/cond", h.GetThing)
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			unresolved: []string{`api.GET("/cond")`},
		},
		{
			name: "pointer",
			body: `func f(e *echo.Echo, h *Handler) {
	g := e.Group("/v1")
	p := &g
	*p = e.Group("/v2")
	g.GET("/ptr", h.GetThing)
}`,
			unresolved: []string{`g.GET("/ptr")`},
		},
		{
			name: "pointer passed to a function",
			body: `func f(e *echo.Echo, h *Handler) {
	g := e.Group("/v1")
	register := func() {
		g.GET("/ptr-literal", h.GetThing)
	}
	setV2(e, &g)
	g.GET("/ptr-call", h.GetThing)
	register()
}`,
			unresolved: []string{`g.GET("/ptr-call")`, `g.GET("/ptr-literal")`},
		},
		{
			name: "method value",
			body: `func f(e *echo.Echo, h *Handler) {
	get := e.GET
	get("/mv", h.GetThing)
	register(e.POST, "/mv2", h.GetThing)
	e.Add(echo.GET, "/constant", h.GetThing)
	e.GET("/called", h.GetThing)
}`,
			routes:     []string{"GET /called"},
			unresolved: []string{`e.Add("/constant")`, `e.GET()`, `e.POST()`},
		},
		{
			name: "method value on a variable shadowing an import",
			body: `func f(e *echo.Echo, h *Handler) {
	echo := e
	get := echo.GET
	get("/mv", h.GetThing)
}`,
			unresolved: []string{`echo.GET()`},
		},
		{
			name: "parenthesised route method called",
			body: `func f(e *echo.Echo, h *Handler) {
	(e.GET)("/paren-call", h.GetThing)
}`,
			routes: []string{"GET /paren-call"},
		},
		{
			name: "group set by a function literal called later",
			body: `func f(e *echo.Echo, h *Handler) {
	var cur *echo.Group
	section := func() {
		cur = e.Group("/v2")
	}
	cur = e.Group("/v1")
	section()
	cur.GET("/x", h.GetThing)
}`,
			unresolved: []string{`cur.GET("/x")`},
		},
		{
			name: "group set in a branch of a function literal called later",
			body: `func f(e *echo.Echo, h *Handler, admin bool) {
	g := e.Group("/a")
	pick := func() {
		if admin {
			g = e.Group("/admin")
		}
	}
	g = e.Group("/b")
	pick()
	g.GET("/x", h.GetThing)
}`,
			unresolved: []string{`g.GET("/x")`},
		},
		{
			name: "group reset by a function literal called later",
			body: `func f(e *echo.Echo, h *Handler) {
	g := e.Group("/a")
	reset := func() {
		g = e.Group("/a")
	}
	g = e.Group("/b")
	reset()
	g.GET("/x", h.GetThing)
}`,
			unresolved: []string{`g.GET("/x")`},
		},
		{
			name: "root name bound by a type switch",
			body: `func f(e *echo.Echo, h *Handler, v any) {
	switch api := v.(type) {
	case *echo.Group:
		api.GET("/x", h.GetThing)
	}
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			unresolved: []string{`api.GET("/x")`},
		},
		{
			name: "root name as a range value",
			body: `func f(e *echo.Echo, h *Handler) {
	for _, api := range []*echo.Group{e.Group("/v1"), e.Group("/v2")} {
		api.GET("/x", h.GetThing)
	}
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			unresolved: []string{`api.GET("/x")`},
		},
		{
			name: "root name as a named result",
			body: `func f(e *echo.Echo, h *Handler) (api *echo.Group) {
	defer func() {
		api.GET("/x", h.GetThing)
	}()
	return e.Group("/b")
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			unresolved: []string{`api.GET("/x")`},
		},
		{
			name: "root name declared without a value or set from another call",
			body: `func f(h *Handler) {
	e := echo.New()
	var api *echo.Group
	e.GET("/root", h.GetThing)
	api.GET("/declared", h.GetThing)
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			routes:     []string{"GET /api/declared", "GET /root"},
		},
		{
			name: "parenthesised assignment",
			body: `func f(e *echo.Echo, h *Handler) {
	g := e.Group("/a")
	(g) = e.Group("/b")
	g.GET("/x", h.GetThing)
}`,
			routes: []string{"GET /b/x"},
		},
		{
			name: "parenthesised range variable",
			body: `func f(e *echo.Echo, h *Handler, gs []*echo.Group) {
	g := e.Group("/a")
	for _, (g) = range gs {
	}
	g.GET("/x", h.GetThing)
}`,
			unresolved: []string{`g.GET("/x")`},
		},
		{
			name: "parenthesised select assignment",
			body: `func f(e *echo.Echo, h *Handler, ch chan *echo.Group) {
	g := e.Group("/a")
	select {
	case (g) = <-ch:
	}
	g.GET("/x", h.GetThing)
}`,
			unresolved: []string{`g.GET("/x")`},
		},
		{
			name: "write through the group pointer",
			body: `func f(e *echo.Echo, h *Handler) {
	g := e.Group("/a")
	*g = *e.Group("/b")
	g.GET("/x", h.GetThing)
}`,
			unresolved: []string{`g.GET("/x")`},
		},
		{
			name: "pointer written after a reassignment",
			body: `func f(e *echo.Echo, h *Handler) {
	g := e.Group("/a")
	p := &g
	g = e.Group("/c")
	*p = e.Group("/d")
	g.GET("/x", h.GetThing)
}`,
			unresolved: []string{`g.GET("/x")`},
		},
		{
			name: "every branch sets the same group",
			body: `func f(e *echo.Echo, h *Handler, auth bool, a bool, b bool) {
	var api2 *echo.Group
	if auth {
		api2 = e.Group("/v1", mw)
	} else {
		api2 = e.Group("/v1")
	}
	api2.GET("/x", h.GetThing)

	var s *echo.Group
	switch {
	case a:
		s = e.Group("/s", mw)
	default:
		s = e.Group("/s")
	}
	s.GET("/y", h.GetThing)

	var chain *echo.Group
	if a {
		chain = e.Group("/c")
	} else if b {
		chain = e.Group("/c")
	} else {
		chain = e.Group("/c", mw)
	}
	chain.GET("/z", h.GetThing)
}`,
			routes: []string{"GET /c/z", "GET /s/y", "GET /v1/x"},
		},
		{
			name: "branches that do not cover every case",
			body: `func f(e *echo.Echo, h *Handler, a bool, b bool) {
	var g *echo.Group
	if a {
		g = e.Group("/v1")
	} else if b {
		g = e.Group("/v1")
	}
	g.GET("/x", h.GetThing)

	var s *echo.Group
	switch {
	case a:
		s = e.Group("/s")
	case b:
		s = e.Group("/s")
	}
	s.GET("/y", h.GetThing)

	var d *echo.Group
	if a {
		d = e.Group("/d")
	} else {
		d = e.Group("/other")
	}
	d.GET("/z", h.GetThing)
}`,
			unresolved: []string{`d.GET("/z")`, `g.GET("/x")`, `s.GET("/y")`},
		},
		{
			name: "loop that changes a group on a later iteration",
			body: `func f(e *echo.Echo, h *Handler, n int) {
	x, y := e.Group("/a"), e.Group("/a")
	for i := 0; i < n; i++ {
		x = y
		y = e.Group("/b")
	}
	x.GET("/x", h.GetThing)
}`,
			unresolved: []string{`x.GET("/x")`},
		},
		{
			name: "loop that passes a group through three variables",
			body: `func f(e *echo.Echo, h *Handler, n int) {
	x, y, z := e.Group("/a"), e.Group("/a"), e.Group("/a")
	for i := 0; i < n; i++ {
		x.GET("/in-loop", h.GetThing)
		x = y
		y = z
		z = e.Group("/b")
	}
	x.GET("/after", h.GetThing)
}`,
			unresolved: []string{`x.GET("/after")`, `x.GET("/in-loop")`},
		},
		{
			name: "break out of a switch with a default",
			body: `func f(e *echo.Echo, h *Handler, a bool, b bool) {
	g := e.Group("/v1")
	switch {
	case a:
		g = e.Group("/v2")
		if b {
			break
		}
		g = e.Group("/v1")
	default:
	}
	g.GET("/x", h.GetThing)
}`,
			unresolved: []string{`g.GET("/x")`},
		},
		{
			name: "root name used in a function literal and reassigned",
			body: `func f(e *echo.Echo, api *echo.Group, h *Handler) {
	register := func() {
		api.GET("/x", h.GetThing)
	}
	api = e.Group("/v2")
	register()
}`,
			rootGroups: map[string]string{"e": "", "api": "/api"},
			unresolved: []string{`api.GET("/x")`},
		},
		{
			name: "path and handler from one call",
			body: `func f(e *echo.Echo, h *Handler) {
	e.GET(route(h))
	e.GET("/known", h.GetThing)
}

func route(h *Handler) (string, echo.HandlerFunc) {
	return "/spread", h.GetThing
}`,
			routes:     []string{"GET /known"},
			unresolved: []string{`e.GET(route(h))`},
		},
		{
			name: "variable shadowing a package constant",
			body: `const p = "/pkg"

func f(e *echo.Echo, h *Handler) {
	p := "/var"
	e.GET(p, h.GetThing)
}`,
			unresolved: []string{`e.GET(p)`},
		},
	})
}

// routeCase is a route file of one package api importing echo, the RootGroups it is read with
// (the default when nil), and the routes and unresolved registrations it declares, in order.
type routeCase struct {
	name       string
	body       string
	rootGroups map[string]string
	routes     []string
	unresolved []string
}

func runRouteCases(t *testing.T, cases []routeCase) {
	t.Helper()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package api\n\nimport \"github.com/labstack/echo/v5\"\n\n" + tc.body + "\n"
			root := writeFixture(t, map[string]string{"routes.go": src})
			cfg := Config{Root: root, RoutesGlob: "routes.go", HandlerGlob: "*.go", RootGroups: tc.rootGroups}

			_, err := ParseRoutes(cfg)
			if len(tc.unresolved) > 0 && (err == nil || !strings.Contains(err.Error(), "SkipUnresolved")) {
				t.Fatalf("expected an unresolved route error, got %v", err)
			}

			var unresolved []string
			cfg.SkipUnresolved = func(route UnresolvedRoute) bool {
				unresolved = append(unresolved, route.Receiver+"."+route.Method+"("+route.Path+")")
				return true
			}
			routes, err := ParseRoutes(cfg)
			if err != nil && len(tc.routes) > 0 {
				t.Fatal(err)
			}

			var got []string
			for _, r := range routes {
				got = append(got, r.Method+" "+r.Path)
			}
			sort.Strings(unresolved)
			if !reflect.DeepEqual(got, tc.routes) || !reflect.DeepEqual(unresolved, tc.unresolved) {
				t.Fatalf("routes %q, unresolved %q\nwant routes %q, unresolved %q", got, unresolved, tc.routes, tc.unresolved)
			}
		})
	}
}

// A RootGroups name set to a call of anything but Group or to a field is its root group, also
// when the value is one of several results of a call. v1.38.0 refused all but a call with one
// result.
func TestParseRoutesTakesARootNameSetToACallOrAFieldForItsRoot(t *testing.T) {
	apiGroups := map[string]string{"e": "", "api": "/api"}
	runRouteCases(t, []routeCase{
		{
			name: "call with several results",
			body: `func newEcho() (*echo.Echo, error) { return echo.New(), nil }

func f(h *Handler) error {
	e, err := newEcho()
	if err != nil {
		return err
	}
	e.GET("/x", h.GetThing)
	return nil
}`,
			routes: []string{"GET /x"},
		},
		{
			name: "var from a call with several results",
			body: `func f(h *Handler) {
	var api, other = two()
	_ = other
	api.GET("/x", h.GetThing)
}`,
			rootGroups: apiGroups,
			routes:     []string{"GET /api/x"},
		},
		{
			name: "assigned from a call with several results",
			body: `func f(e *echo.Echo, h *Handler) error {
	api := e.Group("/v1")
	var err error
	api, err = newAPI()
	if err != nil {
		return err
	}
	api.GET("/x", h.GetThing)
	return nil
}`,
			rootGroups: apiGroups,
			routes:     []string{"GET /api/x"},
		},
		{
			name: "field",
			body: `type server struct {
	echo *echo.Echo
}

func (s *server) addRoutes(h *Handler) {
	e := s.echo
	e.GET("/x", h.GetThing)
}`,
			routes: []string{"GET /x"},
		},
		{
			name: "field assigned and declared with var",
			body: `func f(e *echo.Echo, h *Handler, s *Server) {
	e = s.echo
	e.GET("/x", h.GetThing)
	var api = s.v2
	api.GET("/y", h.GetThing)
}`,
			rootGroups: apiGroups,
			routes:     []string{"GET /api/y", "GET /x"},
		},
	})
}

// A RootGroups name set to a variable holds what the variable holds, so a route on it fails
// generation, with the variable's reason, when the finder does not know that group: a parameter
// not named in RootGroups, a package variable, a variable set to a field or to a Group call on a
// group the finder does not know, or one a branch may have set to a group. So does a RootGroups
// name set to an element, a type assertion or a pointer read, as on v1.38.0.
func TestParseRoutesRefusesARootNameSetToAVariableOrAnElement(t *testing.T) {
	src := `package api

import "github.com/labstack/echo/v5"

var api = echo.New().Group("/api")

func register(g *echo.Group, h *Handler) {
	v1 := g.Group("/v1")
	e := v1
	e.GET("/group-of-parameter", h.GetThing)
}

func (s *Server) groupOfField(h *Handler) {
	x := s.api
	v1 := x.Group("/v1")
	e := v1
	e.GET("/group-of-field", h.GetThing)
}

func groupAssigned(h *Handler) {
	x := echo.New()
	x = x.Group("/v1")
	e := x
	e.GET("/group-assigned", h.GetThing)
}

func groupAssignedInABranch(h *Handler, c bool) {
	x := echo.New()
	if c {
		x = x.Group("/v1")
	}
	e := x
	e.GET("/group-assigned-in-a-branch", h.GetThing)
}

func addV1(g *echo.Group, h *Handler) {
	e := g
	e.GET("/parameter", h.GetThing)
}

func packageVariable(h *Handler) {
	e := api
	e.GET("/package-variable", h.GetThing)
}

func element(h *Handler, gs []*echo.Echo) {
	e := (gs[1])
	e.GET("/element", h.GetThing)
}

func typeAssertion(h *Handler, v any) {
	e := v.(*echo.Echo)
	e.GET("/type-assertion", h.GetThing)
}

func pointer(h *Handler, p **echo.Echo) {
	e := *p
	e.GET("/pointer", h.GetThing)
}
`
	root := writeFixture(t, map[string]string{"routes.go": src})
	cfg := Config{Root: root, RoutesGlob: "routes.go", HandlerGlob: "*.go"}

	_, err := ParseRoutes(cfg)
	if err == nil || ez.ErrorCode(err) != ez.EINVALID {
		t.Fatalf("expected an invalid error, got %v", err)
	}

	got := map[string]string{}
	cfg.SkipUnresolved = func(route UnresolvedRoute) bool {
		got[route.Path] = route.Reason
		return true
	}
	_, err = ParseRoutes(cfg)
	if err == nil || ez.ErrorCode(err) != ez.ENOTFOUND {
		t.Fatalf("expected no routes once every unresolved one is skipped, got %v", err)
	}

	parameter := `unknown route group "g", a function parameter not named in RootGroups`
	newEcho := `echo.New() is not a group the route finder can follow`
	want := map[string]string{
		`"/group-of-parameter"`:         parameter,
		`"/group-of-field"`:             `s.api is not a group the route finder can follow`,
		`"/group-assigned"`:             newEcho,
		`"/group-assigned-in-a-branch"`: newEcho,
		`"/parameter"`:                  parameter,
		`"/package-variable"`:           `unknown route group "api"`,
		`"/element"`:                    `gs[1] is not a group the route finder can follow`,
		`"/type-assertion"`:             `v.(*echo.Echo) is not a group the route finder can follow`,
		`"/pointer"`:                    `*p is not a group the route finder can follow`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unresolved routes:\n got %q\nwant %q", got, want)
	}
}

// A handler method named like a route method, h.Things.GET, is a handler wherever the route call
// reads it, not a route method used as a value; a route method of a group used as a value still
// is one.
func TestParseRoutesReadsHandlersNamedLikeRouteMethods(t *testing.T) {
	src := `package api

func addRoutes(e *echo.Echo, h *Handlers, s *Server) {
	e.GET("/things/:id", h.Things.GET)
	e.POST("/things", withLog(h.Things.POST), h.Auth.HEAD)
	e.PUT("/things/:id", s.h.Things.PUT)
	e.DELETE("/things/:id", func(c *echo.Context) error {
		remove := h.Things.DELETE
		return remove(c)
	})
	g := e.Group("/g")
	get := g.GET
	get("/value", h.Things.Get)
}
`
	root := writeFixture(t, map[string]string{"routes.go": src})
	cfg := Config{Root: root, RoutesGlob: "routes.go", HandlersFile: "handlers.go", HandlerGlob: "*.go"}

	_, err := ParseRoutes(cfg)
	want := routeFile(root, "routes.go") + `:12: g.GET: the method GET is used as a value, not called, so the routes registered through it are not known`
	if err == nil || !strings.Contains(filepath.ToSlash(ez.ErrorMessage(err)), want) {
		t.Fatalf("expected the error to contain %q, got %v", want, err)
	}
	if strings.Contains(err.Error(), "h.Things") || strings.Contains(err.Error(), "h.Auth") {
		t.Fatalf("a handler was taken for a route method value: %v", err)
	}

	cfg.SkipUnresolved = func(route UnresolvedRoute) bool {
		return route.Receiver == "g"
	}
	// The DELETE handler is a function literal, so the route has no handler method.
	cfg.Skip = func(method string, path string) bool {
		return method == "DELETE"
	}
	routes, err := ParseRoutes(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, r := range routes {
		got = append(got, r.Method+" "+r.Path+" "+r.HandlerKey())
	}
	wantRoutes := []string{"POST /things things.POST", "DELETE /things/:id ", "GET /things/:id things.GET", "PUT /things/:id things.PUT"}
	if !reflect.DeepEqual(got, wantRoutes) {
		t.Fatalf("routes:\n got %q\nwant %q", got, wantRoutes)
	}
}

// Only a selector written as a route call reads a handler, h.Method in flat mode and
// h.Namespace.Method in nested mode, through the receiver variable or a field holding it, is a
// handler named like a route method; any other selector through the receiver variable, such as
// a route method of a field, h.echo.GET, is a route method used as a value, and so is h.GET where
// a group shadows the receiver variable.
func TestParseRoutesTakesOnlyHandlerSelectorsForHandlers(t *testing.T) {
	cases := []struct {
		name         string
		handlersFile string
		body         string
		routes       []string
		unresolved   []string
	}{
		{
			name: "flat",
			body: `func (h *Server) routes(s *Server) {
	e := h.echo
	e.GET("/x", h.X)
	e.GET("/head", h.HEAD)
	e.POST("/post", s.h.POST)
	get := h.echo.GET
	get("/y", h.Y)
	put := h.Things.PUT
	_ = put
}`,
			routes:     []string{"GET /head", "POST /post", "GET /x"},
			unresolved: []string{"h.Things.PUT", "h.echo.GET"},
		},
		{
			name: "flat receiver shadowed by a group",
			body: `func (h *Server) routes(e *echo.Echo, hh *Server) {
	e.GET("/before", h.GET)
	{
		h := e.Group("/admin")
		register(h.GET, hh)
	}
	e.GET("/after", h.HEAD)
}`,
			routes:     []string{"GET /after", "GET /before"},
			unresolved: []string{"h.GET"},
		},
		{
			name:         "nested",
			handlersFile: "handlers.go",
			body: `func addRoutes(e *echo.Echo, h *Handlers, s *Server) {
	e.GET("/things/:id", h.Things.GET)
	e.PUT("/things/:id", s.h.Things.PUT)
	head := h.HEAD
	post := h.Things.Sub.POST
	_, _ = head, post
}`,
			routes:     []string{"GET /things/:id", "PUT /things/:id"},
			unresolved: []string{"h.HEAD", "h.Things.Sub.POST"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "package api\n\nimport \"github.com/labstack/echo/v5\"\n\n" + tc.body + "\n"
			root := writeFixture(t, map[string]string{"routes.go": src})
			var unresolved []string
			cfg := Config{
				Root:         root,
				RoutesGlob:   "routes.go",
				HandlersFile: tc.handlersFile,
				HandlerGlob:  "*.go",
				SkipUnresolved: func(route UnresolvedRoute) bool {
					unresolved = append(unresolved, route.Receiver+"."+route.Method)
					return true
				},
			}

			routes, err := ParseRoutes(cfg)
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, r := range routes {
				got = append(got, r.Method+" "+r.Path)
			}
			sort.Strings(unresolved)
			if !reflect.DeepEqual(got, tc.routes) || !reflect.DeepEqual(unresolved, tc.unresolved) {
				t.Fatalf("routes %q, unresolved %q\nwant routes %q, unresolved %q", got, unresolved, tc.routes, tc.unresolved)
			}
		})
	}
}

// A route registered on a known group with Any, Match, Add, AddRoute, CONNECT, OPTIONS, TRACE or
// QUERY fails generation, as the document would leave it out, unless SkipUnresolved does. RouteNotFound,
// Use, Static and File are not API routes, and these methods on a receiver the finder does not
// take for a group are not routes at all.
func TestParseRoutesReportsRoutesOfMethodsItDoesNotDocument(t *testing.T) {
	src := `package api

func addRoutes(e *echo.Echo, h *Handler, s *Server, z *big.Int) {
	e.GET("/things", h.GetThing)
	e.Any("/any", h.GetThing)
	e.Match([]string{"GET", "POST"}, "/match", h.GetThing)
	e.Add("GET", "/add", h.GetThing)
	e.CONNECT("/connect", h.GetThing)
	e.OPTIONS("/options", h.GetThing)
	e.TRACE("/trace", h.GetThing)
	g := e.Group("/g")
	g.Any("/any", h.GetThing)
	g.AddRoute(echo.Route{Method: "GET", Path: "/route", Handler: h.GetThing})
	e.RouteNotFound("/*", h.GetThing)
	e.Use(mw)
	e.Static("/static", "public")
	e.File("/favicon.ico", "favicon.ico")
	s.api.Any("/unknown", h.GetThing)
	s.api.AddRoute(route)
	ok, _ := regexp.Match("a", []byte("a"))
	z.Add(z, big.NewInt(1))
	_ = ok
	e.QUERY("/query", h.GetThing)
}
`
	root := writeFixture(t, map[string]string{"routes.go": src})
	cfg := Config{Root: root, RoutesGlob: "routes.go", HandlerGlob: "*.go"}

	_, err := ParseRoutes(cfg)
	if err == nil || ez.ErrorCode(err) != ez.EINVALID {
		t.Fatalf("expected an invalid error, got %v", err)
	}
	reason := "the generator documents only routes registered with GET, POST, PUT, PATCH, DELETE or HEAD"
	file := routeFile(root, "routes.go")
	for _, line := range []string{
		file + `:5: e.Any("/any", ...): ` + reason,
		file + `:6: e.Match("/match", ...): ` + reason,
		file + `:7: e.Add("/add", ...): ` + reason,
		file + `:12: g.Any("/any", ...): ` + reason,
		file + `:13: g.AddRoute(...): ` + reason,
	} {
		if !strings.Contains(filepath.ToSlash(ez.ErrorMessage(err)), line) {
			t.Errorf("error does not contain %q: %v", line, err)
		}
	}

	var skipped []UnresolvedRoute
	cfg.SkipUnresolved = func(route UnresolvedRoute) bool {
		skipped = append(skipped, route)
		return true
	}
	routes, err := ParseRoutes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Path != "/things" {
		t.Fatalf("routes: %+v, want only GET /things", routes)
	}
	unresolved := func(method string, receiver string, path string, line int) UnresolvedRoute {
		return UnresolvedRoute{Method: method, Receiver: receiver, Path: path, File: "routes.go", Line: line, Reason: reason}
	}
	wantSkipped := []UnresolvedRoute{
		unresolved("Any", "e", `"/any"`, 5),
		unresolved("Match", "e", `"/match"`, 6),
		unresolved("Add", "e", `"/add"`, 7),
		unresolved("CONNECT", "e", `"/connect"`, 8),
		unresolved("OPTIONS", "e", `"/options"`, 9),
		unresolved("TRACE", "e", `"/trace"`, 10),
		unresolved("Any", "g", `"/any"`, 12),
		unresolved("AddRoute", "g", "", 13),
		unresolved("QUERY", "e", `"/query"`, 23),
	}
	if !reflect.DeepEqual(skipped, wantSkipped) {
		t.Fatalf("SkipUnresolved received:\n got %+v\nwant %+v", skipped, wantSkipped)
	}
}

// A route registered with a method the generator does not document on a group the finder cannot
// resolve, a Group call whose prefix is not a constant or a variable that held a known group
// before a branch reassigned it or its address was taken, or that each branch set to another
// known group, fails generation with the reason a GET there gives. A call of one of these methods
// with other arguments than Echo's, on a root group or on a parameter or variable of a RootGroups
// name, is not a route, nor is one on a range variable.
func TestParseRoutesReportsUndocumentedMethodsOnGroupsItCannotResolve(t *testing.T) {
	src := `package api

import "github.com/labstack/echo/v5"

func addRoutes(e *echo.Echo, h *Handler, prefix string, debug bool, regexes []*regexp.Regexp, b []byte, cs []*Calls) {
	e.GET("/things", h.GetThing)
	e.Group(prefix).Any("/hook", h.GetThing)
	g := e.Group("/v1")
	if debug {
		g = e.Group("/debug")
	}
	g.Match([]string{"GET"}, "/m", h.GetThing)
	g.OPTIONS("/o", h.GetThing)
	for _, r := range regexes {
		r.Match(b)
	}
	for _, c := range cs {
		c.Add("GET", "/range", h.GetThing)
	}
}

func tally(e *big.Int, x *big.Int) {
	e.Add(e, x)
}

func match(b []byte) bool {
	e := regexp.MustCompile("a")
	return e.Match(b)
}

func count() {
	each(func(e *Counter) {
		e.Add(1)
	})
}

func lostByPointer(e *echo.Echo, h *Handler) {
	g := e.Group("/a")
	p := &g
	_ = p
	g.Any("/pointer", h.GetThing)
}

func setInBothBranches(e *echo.Echo, h *Handler, c bool) {
	var g *echo.Group
	if c {
		g = e.Group("/a")
	} else {
		g = e.Group("/b")
	}
	g.Any("/both", h.GetThing)
}

func otherArguments(e *echo.Echo, x any, a any, b any) {
	e.Any(x)
	e.AddRoute(a, b)
}
`
	root := writeFixture(t, map[string]string{"routes.go": src})
	cfg := Config{Root: root, RoutesGlob: "routes.go", HandlerGlob: "*.go"}

	var skipped []UnresolvedRoute
	cfg.SkipUnresolved = func(route UnresolvedRoute) bool {
		skipped = append(skipped, route)
		return true
	}
	routes, err := ParseRoutes(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 1 || routes[0].Path != "/things" {
		t.Fatalf("routes: %+v, want only GET /things", routes)
	}

	undocumented := "the generator documents only routes registered with GET, POST, PUT, PATCH, DELETE or HEAD"
	reassigned := `"g" is reassigned at routes.go:10 in a branch, a loop or a function literal, so the group it holds here is not known; ` + undocumented
	want := []UnresolvedRoute{
		{Method: "Any", Receiver: "e.Group(prefix)", Path: `"/hook"`, File: "routes.go", Line: 7, Reason: "group prefix prefix is not a string constant; " + undocumented},
		{Method: "Match", Receiver: "g", Path: `"/m"`, File: "routes.go", Line: 12, Reason: reassigned},
		{Method: "OPTIONS", Receiver: "g", Path: `"/o"`, File: "routes.go", Line: 13, Reason: reassigned},
		{Method: "Any", Receiver: "g", Path: `"/pointer"`, File: "routes.go", Line: 41, Reason: `"g" may be set through a pointer from routes.go:39 on, so the group it holds is not known; ` + undocumented},
		{Method: "Any", Receiver: "g", Path: `"/both"`, File: "routes.go", Line: 51, Reason: `"g" is reassigned at routes.go:47 in a branch, a loop or a function literal, so the group it holds here is not known; ` + undocumented},
	}
	for i := range skipped {
		skipped[i].Reason = filepath.ToSlash(skipped[i].Reason)
	}
	if !reflect.DeepEqual(skipped, want) {
		t.Fatalf("SkipUnresolved received:\n got %+v\nwant %+v", skipped, want)
	}
}

// Echo serves a path that does not start with a slash under one, so the document does too; an
// empty path stays as written.
func TestParseRoutesDocumentsAPathWithALeadingSlash(t *testing.T) {
	root := writeFixture(t, map[string]string{"routes.go": `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("things", h.GetThing)
	g := e.Group("v1")
	g.GET("things/:id", h.GetThing)
	e.GET("", h.GetThing)
	items := e.Group("/items")
	items.GET("", h.GetThing)
}
`})

	routes, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes.go", HandlerGlob: "*.go"})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, r := range routes {
		got = append(got, r.Method+" "+r.OpenAPIPath()+" "+r.Namespace)
	}
	want := []string{"GET  ", "GET /items items", "GET /things things", "GET /v1things/{id} v1things"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes:\n got %q\nwant %q", got, want)
	}
}

// Two paths in the document that differ only in the names of their parameters are one OpenAPI
// path, so generation fails naming both routes, unless Skip leaves one of them out.
func TestGenerateRefusesOnePathWithTwoParameterNames(t *testing.T) {
	files := map[string]string{}
	for name, contents := range requiredFixture {
		files[name] = contents
	}
	files["api/routes.go"] = `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("/invoices/:id", h.GetInvoice)
	e.DELETE("/invoices/:invoice", h.CreateInvoice)
}
`
	root := writeFixture(t, files)

	_, err := Generate(Config{Root: root, RoutesGlob: "api/routes.go", HandlerGlob: "api/*.go"})
	if err == nil || ez.ErrorCode(err) != ez.EINVALID {
		t.Fatalf("expected an invalid error, got %v", err)
	}
	file := routeFile(root, "api/routes.go")
	want := "routes GET /invoices/{id} (" + file + ":4) and DELETE /invoices/{invoice} (" + file + ":5) are one OpenAPI path with differently named parameters; use one parameter name in both"
	if filepath.ToSlash(ez.ErrorMessage(err)) != want {
		t.Fatalf("error message:\n got %q\nwant %q", ez.ErrorMessage(err), want)
	}

	// A route Skip leaves out is not in the document, so the other is the only one on the path.
	spec, err := Generate(Config{
		Root:        root,
		RoutesGlob:  "api/routes.go",
		HandlerGlob: "api/*.go",
		Skip: func(method string, path string) bool {
			return method == "DELETE"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := spec.Document["paths"].(map[string]any)
	item, _ := paths["/invoices/{id}"].(map[string]any)
	if len(paths) != 1 || len(item) != 1 || item["get"] == nil {
		t.Fatalf("paths: %v, want only GET /invoices/{id}", paths)
	}
}

// A handler is read through a receiver variable that is a field, s.h.Method and
// s.h.Namespace.Method, as well as through the variable itself.
func TestParseRoutesReadsHandlersOnAReceiverField(t *testing.T) {
	cases := []struct {
		name string
		src  string
		cfg  Config
		want []string
	}{
		{
			name: "flat",
			src:  "package api\n\nfunc (s *Server) addRoutes(e *echo.Echo) {\n\te.GET(\"/field\", s.h.GetThing)\n\te.GET(\"/deep\", s.api.h.ListThings)\n}\n",
			cfg:  Config{RoutesGlob: "routes.go", HandlerGlob: "*.go"},
			want: []string{"GET /deep ListThings", "GET /field GetThing"},
		},
		{
			name: "nested",
			src:  "package api\n\nfunc (s *Server) addRoutes(e *echo.Echo) {\n\te.GET(\"/nested\", s.h.Things.Get)\n}\n",
			cfg:  Config{RoutesGlob: "routes.go", HandlersFile: "handlers.go", HandlerGlob: "*.go"},
			want: []string{"GET /nested things.Get"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Root = writeFixture(t, map[string]string{"routes.go": tc.src})

			routes, err := ParseRoutes(cfg)
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, r := range routes {
				got = append(got, r.Method+" "+r.Path+" "+r.HandlerKey())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("routes:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Routes are read from the files a default build on the running platform compiles: test files,
// files built only with a tag such as ignore and files named or constrained for another
// platform declare nothing, and a file constrained to the running platform does.
func TestParseRoutesReadsOnlyBuiltFiles(t *testing.T) {
	other := "windows"
	if runtime.GOOS == other {
		other = "linux"
	}
	route := func(header string, path string) string {
		return header + "package api\n\nfunc addRoutes(e *echo.Echo, h *Handler) {\n\te.GET(\"" + path + "\", h.GetThing)\n}\n"
	}
	root := writeFixture(t, map[string]string{
		"routes.go":                      route("", "/real"),
		"routes_test.go":                 route("", "/from-test"),
		"routes_ignored.go":              route("//go:build ignore\n\n", "/from-ignore"),
		"routes_" + other + ".go":        route("", "/from-other-platform-name"),
		"routes_constrained.go":          route("//go:build "+other+"\n\n", "/from-other-platform-constraint"),
		"routes_this_platform_only.go":   route("//go:build "+runtime.GOOS+"\n\n", "/from-this-platform"),
		"routes_" + runtime.GOOS + ".go": route("", "/from-this-platform-name"),
	})

	routes, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes*.go", HandlerGlob: "*.go"})
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, r := range routes {
		got = append(got, r.Path)
	}
	want := []string{"/from-this-platform", "/from-this-platform-name", "/real"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routes:\n got %q\nwant %q", got, want)
	}
}

// A route file that imports "C" is built only with cgo on, so with cgo off it declares nothing.
func TestParseRoutesReadsCgoFilesOnlyWithCgoOn(t *testing.T) {
	root := writeFixture(t, map[string]string{
		"routes.go":     "package api\n\nfunc addRoutes(e *echo.Echo, h *Handler) {\n\te.GET(\"/plain\", h.GetThing)\n}\n",
		"routes_cgo.go": "package api\n\n// #include <stdlib.h>\nimport \"C\"\n\nfunc addCgoRoutes(e *echo.Echo, h *Handler) {\n\te.GET(\"/cgo\", h.GetThing)\n}\n",
	})
	enabled := build.Default.CgoEnabled
	t.Cleanup(func() {
		build.Default.CgoEnabled = enabled
	})

	for _, tc := range []struct {
		cgo  bool
		want []string
	}{
		{cgo: true, want: []string{"/cgo", "/plain"}},
		{cgo: false, want: []string{"/plain"}},
	} {
		build.Default.CgoEnabled = tc.cgo
		routes, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes*.go", HandlerGlob: "*.go"})
		if err != nil {
			t.Fatal(err)
		}

		var got []string
		for _, r := range routes {
			got = append(got, r.Path)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("cgo %v: routes %q, want %q", tc.cgo, got, tc.want)
		}
	}
}

// Function literals and loops nested deep are read in time: each level is walked a bounded
// number of times, not twice as often as the level around it.
func TestParseRoutesReadsDeepNestingInTime(t *testing.T) {
	const depth = 32
	src := "package api\n\nfunc addRoutes(e *echo.Echo, h *Handler, n int) {\n"
	src += strings.Repeat("\t_ = func() {\n", depth)
	src += "\tg := e.Group(\"/literals\")\n\tg.GET(\"/deep\", h.GetThing)\n"
	src += strings.Repeat("\t}\n", depth)
	src += strings.Repeat("\tfor i := 0; i < n; i++ {\n", depth)
	src += "\tg := e.Group(\"/loops\")\n\tg.GET(\"/deep\", h.GetThing)\n"
	src += strings.Repeat("\t}\n", depth)
	src += "}\n"
	root := writeFixture(t, map[string]string{"routes.go": src})

	type result struct {
		routes []Route
		err    error
	}
	done := make(chan result, 1)
	go func() {
		routes, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes.go", HandlerGlob: "*.go"})
		done <- result{routes: routes, err: err}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("reading %d nested function literals and %d nested loops took more than 20s", depth, depth)
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	var paths []string
	for _, r := range got.routes {
		paths = append(paths, r.Path)
	}
	want := []string{"/literals/deep", "/loops/deep"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("routes %q, want %q", paths, want)
	}
}

// A long chain of selectors named like a route method is read in time: each selector's chain is
// walked once, not rebuilt name by name.
func TestParseRoutesReadsLongSelectorChainsInTime(t *testing.T) {
	const length = 3000
	src := "package api\n\nfunc addRoutes(e *echo.Echo, h *Handler) {\n\te.GET(\"/x\", h.GetThing)\n\t_ = h" + strings.Repeat(".GET", length) + "\n}\n"
	root := writeFixture(t, map[string]string{"routes.go": src})

	type result struct {
		routes []Route
		err    error
	}
	done := make(chan result, 1)
	go func() {
		routes, err := ParseRoutes(Config{
			Root:        root,
			RoutesGlob:  "routes.go",
			HandlerGlob: "*.go",
			SkipUnresolved: func(route UnresolvedRoute) bool {
				return true
			},
		})
		done <- result{routes: routes, err: err}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("reading a chain of %d selectors took more than 5s", length)
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(got.routes) != 1 || got.routes[0].Path != "/x" {
		t.Fatalf("routes: %+v, want only GET /x", got.routes)
	}
}

func TestParseRoutesReportsBadRouteFiles(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		code  string
		want  string
	}{
		"not Go": {
			files: map[string]string{"routes_bad.go": "e.GET(\"/example\", h.List)\n"},
			code:  ez.EINVALID,
			want:  "routes_bad.go",
		},
		"unknown route group": {
			files: map[string]string{"routes_a.go": "package api\n\nfunc addRoutes() {\n\tmissing.GET(\"/x\", h.List)\n}\n"},
			code:  ez.EINVALID,
			want:  "routes_a.go:4: missing.GET(\"/x\", ...): unknown route group \"missing\"",
		},
		"group of another file": {
			files: map[string]string{
				"routes_a.go": "package api\n\nfunc addA() {\n\titems := e.Group(\"/items\")\n\titems.GET(\"\", h.List)\n}\n",
				"routes_b.go": "package api\n\nfunc addB() {\n\titems.POST(\"\", h.Create)\n}\n",
			},
			code: ez.EINVALID,
			want: "routes_b.go:4: items.POST(\"\", ...): unknown route group \"items\"",
		},
		"route without a handler": {
			files: map[string]string{"routes_a.go": "package api\n\nfunc addRoutes() {\n\te.GET(\"/x\",\n\t\tnotAHandler)\n}\n"},
			code:  ez.EINVALID,
			want:  "routes_a.go:4 GET /x",
		},
		"no routes": {
			files: map[string]string{"routes_a.go": "package api\n\n// e.GET(\"/x\", h.List)\n"},
			code:  ez.ENOTFOUND,
			want:  "no routes found",
		},
	}

	for name, tc := range cases {
		root := writeFixture(t, tc.files)
		_, err := ParseRoutes(Config{Root: root, RoutesGlob: "routes_*.go", HandlerGlob: "*.go"})
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		if ez.ErrorCode(err) != tc.code {
			t.Errorf("%s: expected code %s, got %s (%v)", name, tc.code, ez.ErrorCode(err), err)
		}
		if !strings.Contains(err.Error(), tc.want) && !strings.Contains(ez.ErrorMessage(err), tc.want) {
			t.Errorf("%s: expected the error to contain %q, got %v (message %q)", name, tc.want, err, ez.ErrorMessage(err))
		}
	}
}
