package api

import (
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"

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

	legacy      bool
	hidden      bool
	public      bool
	deprecated  bool
	summary     string
	description string
	timeout     time.Duration
	maxBody     int64
	middleware  []echo.MiddlewareFunc

	reqType reflect.Type
	resType reflect.Type
	params  []openapi.Param

	// Set by Ready, read by the handler after the registry's ready flag.
	schema     *validate.Schema
	kinds      []validate.Kind
	hasBody    bool
	bodySchema *huma.Schema // $ref to the request body component, nil without a body
}

// Handle registers a typed operation. path uses Echo syntax (/:id) relative
// to the group; name is the operationId suffix. Requests are validated
// against the generated schema, decoded into a new Req and passed to the
// app; the app's response is documented as the 200 response Res.
func Handle[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, method, path, name string, opts ...Option) {
	g.register(method, path, name, reflect.TypeFor[Req](), reflect.TypeFor[Res](), nil, opts)
}

// GET registers a typed GET operation.
func GET[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, opts ...Option) {
	Handle[Req, Res, P](g, http.MethodGet, path, name, opts...)
}

// POST registers a typed POST operation.
func POST[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, opts ...Option) {
	Handle[Req, Res, P](g, http.MethodPost, path, name, opts...)
}

// PUT registers a typed PUT operation.
func PUT[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, opts ...Option) {
	Handle[Req, Res, P](g, http.MethodPut, path, name, opts...)
}

// PATCH registers a typed PATCH operation.
func PATCH[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, opts ...Option) {
	Handle[Req, Res, P](g, http.MethodPatch, path, name, opts...)
}

// DELETE registers a typed DELETE operation.
func DELETE[Req, Res any, P interface {
	*Req
	requests.Body
}](g *Group, path, name string, opts ...Option) {
	Handle[Req, Res, P](g, http.MethodDelete, path, name, opts...)
}

// Legacy registers an existing Echo handler with typed metadata only: no
// validation and no param extraction. The spec documents Req's body (if it
// has body properties), the path placeholders and Res as the 200 response,
// and marks the operation x-compose-legacy. Use struct{} for "no body".
func Legacy[Req, Res any](g *Group, method, path, name string, fn echo.HandlerFunc, opts ...Option) {
	if fn == nil {
		panic(ez.New(ez.EINVALID, "api: Legacy handler is nil", nil))
	}
	g.register(method, path, name, reflect.TypeFor[Req](), reflect.TypeFor[Res](), fn, opts)
}

// register validates and records an operation and mounts it on Echo. Any
// problem panics: registration happens at boot.
func (g *Group) register(method, path, name string, reqType, resType reflect.Type, legacy echo.HandlerFunc, opts []Option) {
	r := g.reg
	r.mu.Lock()
	defer r.mu.Unlock()

	op := &operation{
		reg:     r,
		method:  strings.ToUpper(method),
		path:    g.prefix + path,
		name:    name,
		tag:     g.tag,
		legacy:  legacy != nil,
		reqType: reqType,
		resType: resType,
		maxBody: r.maxBody,
	}
	op.id = name
	if op.tag != "" {
		op.id = op.tag + "." + name
	}
	for _, opt := range g.opts {
		opt(op)
	}
	for _, opt := range opts {
		opt(op)
	}

	err := r.check(op)
	if err != nil {
		panic(ez.New(ez.EINVALID, fmt.Sprintf("api: %s %s (%s): %s", op.method, op.path, op.id, ez.ErrorMessage(err)), err))
	}

	r.ops = append(r.ops, op)
	r.ids[op.id] = true
	r.routes[routeKey(op.method, op.path)] = op.id

	h := legacy
	if h == nil {
		h = op.serve
	}
	g.echo.Add(op.method, path, h, op.middleware...)
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

	params, err := r.reflector.AddRequest(op.reqType)
	if err != nil {
		return ez.Wrap(err)
	}

	err = r.reflector.AddResponse(op.resType)
	if err != nil {
		return ez.Wrap(err)
	}

	err = checkNoBody(op.method, op.reqType)
	if err != nil {
		return err
	}

	if op.legacy {
		op.params = legacyParams(op.path, params)
		return nil
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

// legacyParams documents the placeholders of a legacy path, typed from Req's
// path fields when declared, plain strings otherwise. Query and header
// fields are not documented.
func legacyParams(path string, params []openapi.Param) []openapi.Param {
	var out []openapi.Param
	for _, name := range placeholdersOf(path) {
		param := openapi.Param{Name: name, In: "path", Required: true, Schema: stringSchema()}
		for _, p := range params {
			if p.In == "path" && p.Name == name {
				param = p
			}
		}
		out = append(out, param)
	}
	return out
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
