package generator

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
)

// Route is one route declaration found in the route files.
type Route struct {
	Method string
	// Path is the Echo path, e.g. "/units/:id".
	Path string
	// Namespace is the handler namespace: the Handlers struct field in nested mode, the first
	// path segment in flat mode. It names the operation's tag and prefixes its operationId.
	Namespace string
	// Handler is the handler method name.
	Handler    string
	SourceFile string
	SourceLine int
	// Deprecated routes are wrapped in Config.DeprecatedWrapper and left out of the document.
	Deprecated bool
	// BindsBody reports a handler calling one of Config.BodyBinders.
	BindsBody bool
	// Negotiates reports a handler calling Config.Negotiate.
	Negotiates bool

	flat bool
}

// HandlerKey identifies the route's handler method: "namespace.Method" in nested mode,
// "Method" in flat mode.
func (r Route) HandlerKey() string {
	if r.flat {
		return r.Handler
	}

	return handlerKey(r.Namespace, r.Handler)
}

// OpenAPIPath is the route path in OpenAPI form: ":id" becomes "{id}" and a trailing "/*"
// becomes "/{path}".
func (r Route) OpenAPIPath() string {
	return toOpenAPIPath(r.Path)
}

var (
	reGroupAssign = regexp.MustCompile(`^(\w+)\s*:=\s*(\w+)\.Group\("([^"]*)"`)
	reRouteCall   = regexp.MustCompile(`^(\w+)\.(GET|POST|PUT|PATCH|DELETE|HEAD)\("([^"]*)",\s*(.+)$`)
	rePathParam   = regexp.MustCompile(`:([A-Za-z0-9_]+)`)
	reOASParam    = regexp.MustCompile(`\{([^}]+)\}`)
)

// ParseRoutes returns every route declared in the files matched by cfg.RoutesGlob, sorted by
// path then method.
func ParseRoutes(cfg Config) ([]Route, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, ez.Wrap(err)
	}

	routes, err := parseRoutes(cfg)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return routes, nil
}

func parseRoutes(cfg Config) ([]Route, error) {
	routeFiles, err := filepath.Glob(filepath.Join(cfg.Root, cfg.RoutesGlob))
	if err != nil {
		return nil, ez.Wrap(err)
	}
	if len(routeFiles) == 0 {
		return nil, ez.New(ez.ENOTFOUND, "no route files match "+cfg.RoutesGlob, nil)
	}

	parser := newRouteParser(cfg)

	var routes []Route
	var handlerless []string
	for _, abs := range routeFiles {
		parsed, missing, err := parser.parseFile(abs)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		routes = append(routes, parsed...)
		handlerless = append(handlerless, missing...)
	}

	if len(handlerless) > 0 {
		message := "routes without a " + cfg.ReceiverVar + " handler method; route them through a handler method or Skip them:\n  " + strings.Join(handlerless, "\n  ")
		return nil, ez.New(ez.EINVALID, message, nil)
	}
	if len(routes) == 0 {
		return nil, ez.New(ez.ENOTFOUND, "no routes found in "+cfg.RoutesGlob, nil)
	}

	sort.SliceStable(routes, func(i int, j int) bool {
		if routes[i].Path == routes[j].Path {
			return routes[i].Method < routes[j].Method
		}
		return routes[i].Path < routes[j].Path
	})

	return routes, nil
}

type routeParser struct {
	cfg           Config
	nestedHandler *regexp.Regexp
	flatHandler   *regexp.Regexp
}

func newRouteParser(cfg Config) routeParser {
	receiver := regexp.QuoteMeta(cfg.ReceiverVar)

	return routeParser{
		cfg:           cfg,
		nestedHandler: regexp.MustCompile(`\b` + receiver + `\.(\w+)\.(\w+)`),
		flatHandler:   regexp.MustCompile(`\b` + receiver + `\.(\w+)\b`),
	}
}

// parseFile reads the group assignments and route calls of one file, one declaration per line.
// It returns the routes and the "file:line METHOD path" of every route with no handler method
// that Skip does not cover.
func (p routeParser) parseFile(absPath string) ([]Route, []string, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, nil, ez.Wrap(err)
	}

	lines := strings.Split(string(data), "\n")

	basePaths := map[string]string{}
	rootPaths := map[string]string{}
	for name, base := range p.cfg.RootGroups {
		basePaths[name] = base
		rootPaths[name] = base
	}

	var routes []Route
	var handlerless []string

	for i, raw := range lines {
		lineNo := i + 1
		location := absPath + ":" + strconv.Itoa(lineNo)

		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}

		line = stripTrailingComment(line)

		groupMatches := reGroupAssign.FindStringSubmatch(line)
		if len(groupMatches) > 0 {
			child := groupMatches[1]
			parent := groupMatches[2]
			groupPath := groupMatches[3]

			parentPath, ok := basePaths[parent]
			if !ok {
				return nil, nil, ez.New(ez.EINVALID, location+": unknown parent group "+strconv.Quote(parent), nil)
			}

			basePaths[child] = parentPath + groupPath
			rootPaths[child] = rootPaths[parent]
			continue
		}

		routeMatches := reRouteCall.FindStringSubmatch(line)
		if len(routeMatches) == 0 {
			continue
		}

		receiver := routeMatches[1]
		method := strings.ToUpper(routeMatches[2])
		suffix := routeMatches[3]
		handlerRaw := strings.TrimSpace(routeMatches[4])

		basePath, ok := basePaths[receiver]
		if !ok {
			return nil, nil, ez.New(ez.EINVALID, location+": unknown route group "+strconv.Quote(receiver), nil)
		}

		fullPath := basePath + suffix

		r := Route{
			Method:     method,
			Path:       fullPath,
			SourceFile: filepath.ToSlash(absPath),
			SourceLine: lineNo,
			flat:       p.cfg.flat(),
		}

		if p.cfg.DeprecatedWrapper != "" {
			r.Deprecated = strings.Contains(handlerRaw, p.cfg.DeprecatedWrapper+"(")
		}

		nestedMatch, flatMatch := p.handlerMatches(handlerRaw)
		switch {
		case len(nestedMatch) > 2:
			r.Namespace = lowerFirst(nestedMatch[1])
			r.Handler = nestedMatch[2]
		case len(flatMatch) > 1 && !p.cfg.flat():
			return nil, nil, ez.New(ez.EINVALID, location+": flat handler reference "+p.cfg.ReceiverVar+"."+flatMatch[1]+" is not allowed with a HandlersFile", nil)
		case len(flatMatch) > 1:
			r.Namespace = flatNamespace(strings.TrimPrefix(fullPath, rootPaths[receiver]))
			r.Handler = flatMatch[1]
		}

		if r.Handler == "" && !p.skip(method, fullPath) {
			handlerless = append(handlerless, location+" "+method+" "+fullPath)
		}

		routes = append(routes, r)
	}

	return routes, handlerless, nil
}

// handlerMatches returns the handler reference of a route call's arguments as either
// `h.Namespace.Method` (nestedMatch) or `h.Method` (flatMatch). The handler is the first
// reference; a nested one later in the line (middleware such as h.Auth.Required) does not
// shadow a flat handler before it.
func (p routeParser) handlerMatches(handlerRaw string) ([]string, []string) {
	nestedMatch := p.nestedHandler.FindStringSubmatchIndex(handlerRaw)
	flatMatch := p.flatHandler.FindStringSubmatchIndex(handlerRaw)

	if nestedMatch != nil && (flatMatch == nil || nestedMatch[0] <= flatMatch[0]) {
		return []string{
			handlerRaw[nestedMatch[0]:nestedMatch[1]],
			handlerRaw[nestedMatch[2]:nestedMatch[3]],
			handlerRaw[nestedMatch[4]:nestedMatch[5]],
		}, nil
	}
	if flatMatch != nil {
		return nil, []string{
			handlerRaw[flatMatch[0]:flatMatch[1]],
			handlerRaw[flatMatch[2]:flatMatch[3]],
		}
	}

	return nil, nil
}

func (p routeParser) skip(method string, path string) bool {
	if p.cfg.Skip == nil {
		return false
	}

	return p.cfg.Skip(method, toOpenAPIPath(path))
}

// flatNamespace is the first segment of a path relative to its root group:
// "/code_snapshots/:id" is in "code_snapshots". A path starting with a parameter has none.
func flatNamespace(path string) string {
	path = strings.TrimPrefix(path, "/")
	segment, _, _ := strings.Cut(path, "/")
	if strings.HasPrefix(segment, ":") || strings.HasPrefix(segment, "*") {
		return ""
	}

	return segment
}

func lowerFirst(value string) string {
	if value == "" {
		return ""
	}

	runes := []rune(value)
	first := runes[0]
	if first >= 'A' && first <= 'Z' {
		runes[0] = first + ('a' - 'A')
	}

	return string(runes)
}

func stripTrailingComment(line string) string {
	idx := strings.Index(line, "//")
	if idx < 0 {
		return line
	}
	return strings.TrimSpace(line[:idx])
}

func ensureNoDuplicateOperations(routes []Route) error {
	seen := map[string]struct{}{}

	for _, r := range routes {
		key := r.Method + " " + r.Path
		_, ok := seen[key]
		if ok {
			return ez.New(ez.EINVALID, "duplicate route: "+key, nil)
		}
		seen[key] = struct{}{}
	}

	return nil
}
