package generator

import (
	"path/filepath"
	"reflect"
	"runtime"
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

	e.GET(dynamicPath, h.GetThing)
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

// Groups nest and their variables are known for the rest of the file, in any function; the
// handler is the first h.Namespace.Method in the arguments, wherever they are wrapped, and
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
	widgets.PUT("/:id", h.Widgets.Replace)
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
			want:  "routes_a.go:4: unknown route group \"missing\"",
		},
		"group of another file": {
			files: map[string]string{
				"routes_a.go": "package api\n\nfunc addA() {\n\titems := e.Group(\"/items\")\n\titems.GET(\"\", h.List)\n}\n",
				"routes_b.go": "package api\n\nfunc addB() {\n\titems.POST(\"\", h.Create)\n}\n",
			},
			code: ez.EINVALID,
			want: "routes_b.go:4: unknown route group \"items\"",
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
