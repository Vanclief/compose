package api

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/requests"
)

// Group is a set of operations sharing a path prefix, a tag, Echo middleware
// and options.
type Group struct {
	reg    *Registry
	echo   *echo.Group
	prefix string
	tag    string
	opts   []Option
}

// Option configures the registry (api.New), a group or an operation. Each
// option supports some of these scopes; using it at another one panics at
// boot. Registry options (Info, Description, APIKeyHeader, Security, Server,
// Namer, Descriptions) are only accepted by New. ContentType, Produces and
// Destructive are only accepted by an operation. Every other option is
// accepted at every scope and applies to every operation below it: options
// of New first, then the group's, then the operation's, so the nearest one
// wins. Middleware adds up instead: the middleware of New, of the groups and
// of the operation all run, in that order.
type Option struct {
	name      string
	registry  func(*Registry)
	op        func(*operation)
	routeOnly bool
	policy    policy
}

// policy tells the authentication options apart, so one options list cannot
// carry both.
type policy int

const (
	noPolicy policy = iota
	authenticatePolicy
	publicPolicy
)

// Option scopes, as named in panic messages.
const (
	registryScope = "api.New"
	groupScope    = "a group"
	routeScope    = "an operation"
)

// checkOptions rejects options used at a scope they do not support, and
// api.Public combined with api.Authenticate in one list.
func checkOptions(opts []Option, scope string) error {
	var policies []policy
	for _, opt := range opts {
		if opt.registry == nil && opt.op == nil {
			return ez.New(ez.EINVALID, "zero api.Option: build options with the api functions", nil)
		}
		if opt.registry != nil && scope != registryScope {
			return ez.New(ez.EINVALID, fmt.Sprintf("%s is a registry option: pass it to api.New, not to %s", opt.name, scope), nil)
		}
		if opt.routeOnly && scope != routeScope {
			return ez.New(ez.EINVALID, fmt.Sprintf("%s is an operation option: pass it to the operation, not to %s", opt.name, scope), nil)
		}
		if opt.policy != noPolicy {
			policies = append(policies, opt.policy)
		}
	}
	if slices.Contains(policies, authenticatePolicy) && slices.Contains(policies, publicPolicy) {
		return ez.New(ez.EINVALID, fmt.Sprintf("api.Public and api.Authenticate cannot both be passed to %s", scope), nil)
	}
	return nil
}

// AuthenticateFunc authenticates a request. It runs first, before any
// parameter is coerced or any body byte is read, so request.GetBody() is nil
// inside it; the headers, IP, method, cookies and context are there. It may
// change the request (request.SetContext, request.SetLocale); the handler
// receives request.GetContext() as it is after the call. An error rejects the
// request: it is returned to Echo, so the client never learns whether its
// body was valid.
type AuthenticateFunc func(request requests.Request) error

// Authenticate sets the authenticator of every operation in its scope and
// marks them protected, undoing a Public set further away: the nearest of
// api.New, the groups and the operation wins. Ready panics when a protected
// operation has no authenticator. Panics when fn is nil.
func Authenticate(fn AuthenticateFunc) Option {
	if fn == nil {
		panic(ez.New(ez.EINVALID, "api: Authenticate needs a function", nil))
	}
	return Option{name: "api.Authenticate", policy: authenticatePolicy, op: func(op *operation) {
		op.authenticate = fn
		op.public = false
	}}
}

// Public skips authentication and removes the security requirement from the
// spec, undoing an Authenticate set further away.
func Public() Option {
	return Option{name: "api.Public", policy: publicPolicy, op: func(op *operation) {
		op.public = true
	}}
}

// Timeout sets the request context timeout (requests.WithTimeout). Panics
// when d is not positive.
func Timeout(d time.Duration) Option {
	if d <= 0 {
		panic(ez.New(ez.EINVALID, "api: Timeout must be positive", nil))
	}
	return Option{name: "api.Timeout", op: func(op *operation) {
		op.timeout = d
	}}
}

// MaxBodyBytes sets the request body limit in bytes (DefaultMaxBody when no
// MaxBodyBytes applies). Passed to New it sets the default of every
// operation, which a group's or the operation's own overrides. Panics when n
// is not positive. rest.RejectNUL does not inspect a body over 32 MB, so an
// operation with a higher limit can be sent a NUL byte that middleware
// refuses in a smaller body.
func MaxBodyBytes(n int64) Option {
	if n <= 0 {
		panic(ez.New(ez.EINVALID, "api: MaxBodyBytes must be positive", nil))
	}
	return Option{name: "api.MaxBodyBytes", op: func(op *operation) {
		op.maxBody = n
	}}
}

// MaxBodyValues sets how many values a request body may hold
// (DefaultMaxBodyValues when no MaxBodyValues applies): in a JSON body every
// string, number, boolean, null, array and object counts one, an object's
// keys do not. A multipart form is the body and counts the same way: the
// body object, each declared field sent with a non-empty value and each item
// of an array field, the items the commas of a single value separate or one
// per repeated value; files do not count. Passed to New it sets the default
// of every operation, which a group's or the operation's own overrides. A
// body that holds more is a 400 before it is decoded or validated, a form
// before its values are split. Panics when n is not positive. What a body
// costs follows its values, not its bytes: the validator builds an error for
// every value that fails, before the registry keeps the first 100 fields, of
// 1.5 KB to about 8 KB of allocation as the value's path is short or long
// and runs through nullable structs, so a failing body allocates about 85 MB
// at the default in the costliest schema measured, and about 1 GB at the
// 250,000 to 500,000 values 1 MiB of JSON can hold.
func MaxBodyValues(n int) Option {
	if n <= 0 {
		panic(ez.New(ez.EINVALID, "api: MaxBodyValues must be positive", nil))
	}
	return Option{name: "api.MaxBodyValues", op: func(op *operation) {
		op.maxValues = n
	}}
}

// Hidden registers the operation on Echo but omits it from the spec. The
// strict document lists it, marked x-internal (see Registry.StrictSpec).
func Hidden() Option {
	return Option{name: "api.Hidden", op: func(op *operation) {
		op.hidden = true
	}}
}

// Destructive marks an operation that destroys data without being a DELETE,
// such as a POST that deletes the records its body names: each document
// that lists it, the 3.1, the 3.0.3 and the strict one, carries
// x-destructive: true on it, which components/rest/reference reads as
// Operation.Destructive. An MCP server built from the document then treats
// the operation as it treats a DELETE (mcp.Function.Destructive). On a
// DELETE the mark changes nothing there. Panics at registration on a GET or
// HEAD, which an MCP server keeps read-only whatever it carries.
func Destructive() Option {
	return Option{name: "api.Destructive", routeOnly: true, op: func(op *operation) {
		op.destructive = true
	}}
}

// Deprecated marks the operation deprecated.
func Deprecated() Option {
	return Option{name: "api.Deprecated", op: func(op *operation) {
		op.deprecated = true
	}}
}

// Doc sets the operation summary and, optionally, its description.
func Doc(summary string, description ...string) Option {
	return Option{name: "api.Doc", op: func(op *operation) {
		op.summary = summary
		op.description = strings.Join(description, "\n\n")
	}}
}

// Tag sets the tag of every operation in its scope, in place of the one the
// group's prefix gives, and with it the first half of their operationId:
// under Group("/complexes/:id/expenses", Tag("finance")) the operation named
// list is finance.list and is documented under finance. A group created
// under a group with a Tag keeps that tag unless it sets its own. Panics when
// name does not match [A-Za-z0-9_-]+.
func Tag(name string) Option {
	if !namePattern.MatchString(name) {
		panic(ez.New(ez.EINVALID, fmt.Sprintf("api: Tag %q must match %s", name, namePattern), nil))
	}
	return Option{name: "api.Tag", op: func(op *operation) {
		op.setTag(name)
	}}
}

// Middleware adds per-route Echo middleware.
func Middleware(m ...echo.MiddlewareFunc) Option {
	return Option{name: "api.Middleware", op: func(op *operation) {
		op.middleware = append(op.middleware, m...)
	}}
}

// Group creates a top-level group. Options apply to every operation in it.
// Panics when an option is not a group option.
func (r *Registry) Group(prefix string, opts ...Option) *Group {
	mustGroupOptions(prefix, opts)
	return &Group{
		reg:    r,
		echo:   r.echo.Group(prefix),
		prefix: prefix,
		tag:    tagOf(prefix),
		opts:   opts,
	}
}

// Group creates a nested group. It inherits the parent's options, and its tag
// is derived from the full prefix unless a Tag applies. Panics when an option
// is not a group option.
func (g *Group) Group(prefix string, opts ...Option) *Group {
	full := g.prefix + prefix
	mustGroupOptions(full, opts)
	return &Group{
		reg:    g.reg,
		echo:   g.echo.Group(prefix),
		prefix: full,
		tag:    tagOf(full),
		opts:   append(append([]Option(nil), g.opts...), opts...),
	}
}

func mustGroupOptions(prefix string, opts []Option) {
	err := checkOptions(opts, groupScope)
	if err != nil {
		panic(ez.New(ez.EINVALID, fmt.Sprintf("api: group %s: %s", prefix, ez.ErrorMessage(err)), err))
	}
}

// Use adds Echo middleware to the group. It applies to the operations
// registered and the groups created with Group after the call: Echo copies
// a group's middleware into a route when the route is added, and into a
// sub-group when the sub-group is created.
func (g *Group) Use(m ...echo.MiddlewareFunc) {
	g.echo.Use(m...)
}

// tagOf returns the last non-parameter segment of prefix.
func tagOf(prefix string) string {
	segments := strings.Split(prefix, "/")
	for i := len(segments) - 1; i >= 0; i-- {
		s := segments[i]
		if s != "" && !strings.HasPrefix(s, ":") && s != "*" {
			return s
		}
	}
	return ""
}
