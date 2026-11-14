package generator

import (
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"

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

	e.Add("OPTIONS", "/options/:id", h.GetThing)
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

	file := routeFile(root, "routes.go")
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
