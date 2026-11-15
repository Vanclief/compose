package spectest

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/generator"
	"github.com/vanclief/compose/components/rest/reference"
)

// AnyMethod is the method Routes shows, and Ignore takes, for a route Echo's
// Any registered, which answers every method.
const AnyMethod = "ANY"

// Option changes what Routes compares.
type Option func(*options)

type options struct {
	rules []*rule
}

// rule leaves routes out of the comparison: method and path exactly, every
// method of path for AnyMethod, or every route whose path starts with prefix.
// written is the option as the test wrote it.
type rule struct {
	method  string
	path    string
	prefix  string
	written string
	used    bool
}

// Ignore leaves the route method path out of the comparison, on both sides.
// path is in OpenAPI form ("/users/{id}") or Echo's ("/users/:id"), which is
// read as the generator reads it, and a path without a leading slash is
// read with one, as Echo serves it. method is as Echo registered it, upper
// case, or AnyMethod, which covers an Any route and every method of path in
// Echo and in the document.
func Ignore(method string, path string) Option {
	return func(o *options) {
		written := "Ignore(" + strconv.Quote(method) + ", " + strconv.Quote(path) + ")"
		o.rules = append(o.rules, &rule{method: method, path: rulePath(path), written: written})
	}
}

// IgnorePrefix leaves out every route whose path, in OpenAPI form, starts
// with prefix, read as Ignore reads a path, whatever its method, on both
// sides. It is a plain string prefix: "/docs" also matches "/docs-old"; end
// it with "/" to match whole segments only.
func IgnorePrefix(prefix string) Option {
	return func(o *options) {
		o.rules = append(o.rules, &rule{prefix: rulePath(prefix), written: "IgnorePrefix(" + strconv.Quote(prefix) + ")"})
	}
}

func (r *rule) matches(method string, path string) bool {
	if r.prefix != "" {
		return strings.HasPrefix(path, r.prefix)
	}
	return (r.method == method || r.method == AnyMethod) && r.path == path
}

// route is one method and path, the path in OpenAPI form.
type route struct {
	method string
	path   string
}

func (r route) String() string {
	return r.method + " " + r.path
}

// Routes fails t when the routes e registered and the operations of the
// OpenAPI document spec differ, with one message listing, sorted by path then
// method, the routes Echo has that the document lacks, the operations the
// document has that Echo lacks, and the ignore rules that matched nothing.
//
// Echo's paths are compared as Echo serves them, with a leading slash ("" is
// "/" and "users" is "/users"), and put in OpenAPI form by the generator's
// rule (generator.Route.OpenAPIPath): ":id" becomes "{id}" and a trailing
// "/*" becomes "/{path}". The generator keeps Echo's parameter names, so a
// route whose parameter the document names otherwise is reported on both
// sides.
//
// Only the routes in Echo's route list are compared. The HEAD answer Echo
// gives a GET route (RouterConfig.AutoHandleHEAD) and its OPTIONS answer are
// not routes and are not compared, and neither are the not-found routes
// Group.Use and RouteNotFound add. A HEAD or OPTIONS route the app registers
// itself is compared like any other; the generator documents HEAD routes but
// not OPTIONS ones, so ignore those. A route whose method OpenAPI 3.0 and
// 3.1 cannot express, such as CONNECT, PROPFIND, QUERY, a custom method or
// an Any route (shown as AnyMethod), is compared too, and so always shows as
// missing from the document until it is ignored: leaving it out is the app's
// decision, made where the test shows it.
//
// Ignore and IgnorePrefix leave routes out on purpose, such as health checks,
// the docs page, the MCP endpoint or static files; Ignore(AnyMethod, path)
// leaves out every method of path. A rule with an empty path fails the test,
// and so does a rule that matches no route on either side, so the list
// cannot outlive the routes it was written for. For a document that is a
// subset of the app's routes, such as the public document
// generator.Spec.Public writes beside the full one, ignore the routes it
// leaves out, by prefix where they share one; a test checking both documents
// calls Routes once for each, with its own rules.
func Routes(t testing.TB, e *echo.Echo, spec []byte, opts ...Option) {
	t.Helper()

	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	empty := false
	for _, r := range o.rules {
		if r.path == "" && r.prefix == "" {
			t.Errorf("spectest: %s: the path is empty; write the path or prefix of the routes to leave out", r.written)
			empty = true
		}
	}
	if empty {
		return
	}

	doc, err := reference.Parse(spec)
	if err != nil {
		t.Errorf("spectest: read the OpenAPI document: %s", ez.ErrorMessage(err))
		return
	}

	inEcho := map[route]bool{}
	for _, info := range e.Router().Routes() {
		if info.Method == echo.RouteNotFound {
			continue
		}
		method := info.Method
		if method == echo.RouteAny {
			method = AnyMethod
		}
		r := route{method: method, path: openAPIPath(servedPath(info.Path))}
		if !o.ignored(r) {
			inEcho[r] = true
		}
	}

	inSpec := map[route]bool{}
	for _, path := range doc.Paths.Keys {
		item := doc.Paths.Map[path]
		if item == nil {
			continue
		}
		for _, m := range item.Methods() {
			r := route{method: m.Method, path: path}
			if !o.ignored(r) {
				inSpec[r] = true
			}
		}
	}

	var echoOnly []route
	for r := range inEcho {
		if !inSpec[r] {
			echoOnly = append(echoOnly, r)
		}
	}
	var specOnly []route
	for r := range inSpec {
		if !inEcho[r] {
			specOnly = append(specOnly, r)
		}
	}
	var unused []string
	for _, r := range o.rules {
		if !r.used {
			unused = append(unused, r.written)
		}
	}

	if len(echoOnly) == 0 && len(specOnly) == 0 && len(unused) == 0 {
		return
	}

	var b strings.Builder
	b.WriteString("spectest: the routes of Echo and the OpenAPI document differ")
	writeRoutes(&b, "in Echo, not in the document", echoOnly)
	writeRoutes(&b, "in the document, not in Echo", specOnly)
	if len(unused) > 0 {
		b.WriteString("\n  ignore rules that match no route:")
		for _, s := range unused {
			b.WriteString("\n    " + s)
		}
	}
	t.Errorf("%s", b.String())
}

// ignored reports whether a rule matches r, marking every rule that does as
// used.
func (o *options) ignored(r route) bool {
	ignored := false
	for _, rl := range o.rules {
		if rl.matches(r.method, r.path) {
			rl.used = true
			ignored = true
		}
	}
	return ignored
}

func writeRoutes(b *strings.Builder, title string, routes []route) {
	if len(routes) == 0 {
		return
	}
	slices.SortFunc(routes, func(x, y route) int {
		return cmp.Or(strings.Compare(x.path, y.path), strings.Compare(x.method, y.method))
	})
	b.WriteString("\n  " + title + ":")
	for _, r := range routes {
		b.WriteString("\n    " + r.String())
	}
}

// servedPath is the path Echo serves a route registered at path, as its
// router normalises it: "" is "/" and a path without a leading slash gets
// one. Echo's route list keeps the path as registered.
func servedPath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

// rulePath reads the path or prefix of a rule as Echo's paths are read; an
// empty one stays empty, which Routes refuses.
func rulePath(path string) string {
	if path == "" {
		return ""
	}
	return openAPIPath(servedPath(path))
}

// openAPIPath puts an Echo path in OpenAPI form as the generator does.
func openAPIPath(path string) string {
	return generator.Route{Path: path}.OpenAPIPath()
}
