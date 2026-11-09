package api

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/api/validate"
	"github.com/vanclief/compose/components/rest/requests"
)

var (
	namePattern    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	allowedMethods = []string{
		http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions,
	}
	// noBodyMethods are the methods whose request type must not have body
	// properties: their parameters belong in the query string.
	noBodyMethods = []string{http.MethodGet, http.MethodHead, http.MethodDelete}
)

// operation is one registered route.
type operation struct {
	reg *Registry

	method string
	path   string // full Echo path
	name   string
	id     string
	tag    string

	hidden       bool
	public       bool
	authenticate AuthenticateFunc
	invoke       func(context.Context, requests.Body) (any, error) // the primary handler
	contentType  string                                            // primary media type of a Blob Res
	producers    []producer                                        // alternate representations, in declaration order
	produces     []string                                          // 200 media types: the primary one, then the producers'
	blob         bool                                              // Res is Blob
	deprecated   bool
	summary      string
	description  string
	timeout      time.Duration
	maxBody      int64
	middleware   []echo.MiddlewareFunc

	reqType reflect.Type
	resType reflect.Type
	params  []openapi.Param
	files   []fileField // non-empty for multipart/form-data operations

	// Set by Ready, read by the handler after the registry's ready flag.
	schema     *validate.Schema
	kinds      []validate.Kind
	hasBody    bool
	bodySchema *huma.Schema // $ref to the request body component, nil without a body
	formKinds  map[string]validate.Kind
}

// Handle registers a typed operation. path uses Echo syntax (/:id) relative
// to the group; name is the operationId suffix. Requests are validated
// against the generated schema, decoded into a new Req, checked with
// Validate, authenticated (see Authenticate) and passed to fn with the
// request context; fn's Res is documented and written as the JSON 200
// response. A Blob Res needs ContentType; Produces adds other
// representations. Res must not be an interface type.
func Handle[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, method, path, name string, fn func(context.Context, P) (Res, error), opts ...Option) {
	var invoke func(context.Context, requests.Body) (any, error)
	if fn != nil {
		invoke = func(ctx context.Context, body requests.Body) (any, error) {
			return fn(ctx, body.(P))
		}
	}
	g.register(method, path, name, reflect.TypeFor[Req](), reflect.TypeFor[Res](), invoke, opts)
}

// GET registers a typed GET operation.
func GET[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, fn func(context.Context, P) (Res, error), opts ...Option) {
	Handle(g, http.MethodGet, path, name, fn, opts...)
}

// POST registers a typed POST operation.
func POST[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, fn func(context.Context, P) (Res, error), opts ...Option) {
	Handle(g, http.MethodPost, path, name, fn, opts...)
}

// PUT registers a typed PUT operation.
func PUT[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, fn func(context.Context, P) (Res, error), opts ...Option) {
	Handle(g, http.MethodPut, path, name, fn, opts...)
}

// PATCH registers a typed PATCH operation.
func PATCH[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, fn func(context.Context, P) (Res, error), opts ...Option) {
	Handle(g, http.MethodPatch, path, name, fn, opts...)
}

// DELETE registers a typed DELETE operation.
func DELETE[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, fn func(context.Context, P) (Res, error), opts ...Option) {
	Handle(g, http.MethodDelete, path, name, fn, opts...)
}

// register validates and records an operation and mounts it on Echo. Any
// problem panics: registration happens at boot.
func (g *Group) register(method, path, name string, reqType, resType reflect.Type, invoke func(context.Context, requests.Body) (any, error), opts []Option) {
	r := g.reg
	r.mu.Lock()
	defer r.mu.Unlock()

	op := &operation{
		reg:     r,
		method:  strings.ToUpper(method),
		path:    g.prefix + path,
		name:    name,
		tag:     g.tag,
		reqType: reqType,
		resType: resType,
		invoke:  invoke,
		maxBody: DefaultMaxBody,
	}
	op.id = name
	if op.tag != "" {
		op.id = op.tag + "." + name
	}

	err := checkOptions(opts, routeScope)
	if err != nil {
		panic(op.registrationError(err))
	}
	for _, list := range [][]Option{r.opts, g.opts, opts} {
		for _, opt := range list {
			opt.op(op)
		}
	}

	err = r.check(op)
	if err != nil {
		panic(op.registrationError(err))
	}

	// Echo refuses a route when an OnAddRoute hook fails, or when its router
	// does not overwrite routes and the app registered the same method and
	// path outside the registry.
	_, err = g.echo.AddRoute(echo.Route{Method: op.method, Path: path, Handler: op.serve, Middlewares: op.middleware})
	if err != nil {
		panic(op.registrationError(ez.New(ez.EINVALID, "echo refused the route: "+err.Error(), err)))
	}

	r.ops = append(r.ops, op)
	r.ids[op.id] = true
	r.routes[routeKey(op.method, op.path)] = op.id
}

// registrationError wraps a registration problem with the operation it is about.
func (op *operation) registrationError(err error) error {
	return ez.New(ez.EINVALID, fmt.Sprintf("api: %s %s (%s): %s", op.method, op.path, op.id, ez.ErrorMessage(err)), err)
}

// check runs every registration rule and reflects the operation's types.
// Called with r.mu held.
func (r *Registry) check(op *operation) error {
	if r.result != nil {
		return ez.New(ez.EINVALID, "registration after Ready", nil)
	}
	if !slices.Contains(allowedMethods, op.method) {
		return ez.New(ez.EINVALID, "unsupported method", nil)
	}
	if !namePattern.MatchString(op.name) {
		return ez.New(ez.EINVALID, fmt.Sprintf("operation name %q must match %s", op.name, namePattern), nil)
	}
	if op.tag != "" && !namePattern.MatchString(op.tag) {
		return ez.New(ez.EINVALID, fmt.Sprintf("tag %q (from the group prefix) must match %s", op.tag, namePattern), nil)
	}
	if r.ids[op.id] {
		return ez.New(ez.EINVALID, "duplicate operationId", nil)
	}
	other, ok := r.routes[routeKey(op.method, op.path)]
	if ok {
		return ez.New(ez.EINVALID, fmt.Sprintf("duplicate route, already registered as %s", other), nil)
	}
	if op.invoke == nil {
		return ez.New(ez.EINVALID, "nil handler", nil)
	}
	if op.resType.Kind() == reflect.Interface {
		return ez.New(ez.EINVALID, fmt.Sprintf("response type %s is an interface: the handler must return a concrete type", op.resType), nil)
	}
	if op.resType == reflect.PointerTo(blobType) {
		return ez.New(ez.EINVALID, "the handler must return api.Blob, not *api.Blob", nil)
	}

	params, err := r.reflector.AddRequest(op.reqType)
	if err != nil {
		return ez.Wrap(err)
	}

	op.blob = op.resType == blobType
	err = checkProduces(op)
	if err != nil {
		return err
	}

	if !op.blob {
		err = r.reflector.AddResponse(op.resType)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	err = checkNoBody(op.method, op.reqType)
	if err != nil {
		return err
	}

	op.files, err = fileFields(op.reqType)
	if err != nil {
		return err
	}
	if len(op.files) > 0 {
		if slices.Contains(noBodyMethods, op.method) {
			return ez.New(ez.EINVALID, fmt.Sprintf("%s requests cannot upload files", op.method), nil)
		}
		err = checkFormFields(op.reqType)
		if err != nil {
			return err
		}
	}

	op.params = params

	err = checkParamTypes(op.reqType, params)
	if err != nil {
		return err
	}
	return checkPathParams(op.path, params)
}

// routeKey identifies a route the way Echo's router does: placeholder names
// do not matter (/runs/:id and /runs/:run_id are the same route).
func routeKey(method, path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		if strings.HasPrefix(s, ":") {
			segments[i] = ":"
		}
	}
	return method + " " + strings.Join(segments, "/")
}

// checkNoBody rejects body properties on GET, HEAD and DELETE: a request
// type whose fields lack query tags would otherwise silently become a
// request body that clients cannot send and that hides the query string.
func checkNoBody(method string, reqType reflect.Type) error {
	if !slices.Contains(noBodyMethods, method) {
		return nil
	}
	fields := bodyFieldNames(derefType(reqType))
	if len(fields) == 0 {
		return nil
	}
	return ez.New(ez.EINVALID, fmt.Sprintf(
		"%s requests cannot have a body, but %s has body fields %s: tag them query:\"...\" and json:\"-\"",
		method, reqType, strings.Join(fields, ", ")), nil)
}

// bodyFieldNames returns the names of the exported fields of t that become
// JSON body properties: every visible field not tagged json:"-", except
// untagged embedded structs, whose fields are promoted.
func bodyFieldNames(t reflect.Type) []string {
	var names []string
	for _, f := range reflect.VisibleFields(t) {
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" || (f.Anonymous && name == "") {
			continue
		}
		names = append(names, f.Name)
	}
	return names
}

// checkPathParams requires a path param for every placeholder and vice versa.
func checkPathParams(path string, params []openapi.Param) error {
	placeholders := placeholdersOf(path)

	var declared []string
	for _, p := range params {
		if p.In == "path" {
			declared = append(declared, p.Name)
		}
	}

	var problems []string
	for _, name := range placeholders {
		if !slices.Contains(declared, name) {
			problems = append(problems, fmt.Sprintf("placeholder :%s has no path:%q field", name, name))
		}
	}
	for _, name := range declared {
		if !slices.Contains(placeholders, name) {
			problems = append(problems, fmt.Sprintf("path:%q field has no :%s placeholder", name, name))
		}
	}
	if len(problems) > 0 {
		return ez.New(ez.EINVALID, strings.Join(problems, "; "), nil)
	}
	return nil
}

func placeholdersOf(path string) []string {
	var names []string
	for _, s := range strings.Split(path, "/") {
		name, ok := strings.CutPrefix(s, ":")
		if ok {
			names = append(names, name)
		}
	}
	return names
}

// specPath converts Echo placeholders (:id) to OpenAPI ({id}).
func specPath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		name, ok := strings.CutPrefix(s, ":")
		if ok {
			segments[i] = "{" + name + "}"
		}
	}
	return strings.Join(segments, "/")
}
