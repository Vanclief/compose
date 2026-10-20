package api

import (
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"
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

// Option configures an operation, or every operation of a group.
type Option func(*operation)

// Timeout sets the request context timeout (requests.WithTimeout).
func Timeout(d time.Duration) Option {
	return func(op *operation) {
		op.timeout = d
	}
}

// MaxBodyBytes sets the request body limit in bytes, overriding the
// registry's MaxBody. Panics when n is not positive.
func MaxBodyBytes(n int64) Option {
	if n <= 0 {
		panic(ez.New(ez.EINVALID, "api: MaxBodyBytes must be positive", nil))
	}
	return func(op *operation) {
		op.maxBody = n
	}
}

// Hidden registers the operation on Echo but omits it from the spec.
func Hidden() Option {
	return func(op *operation) {
		op.hidden = true
	}
}

// Public removes the security requirement.
func Public() Option {
	return func(op *operation) {
		op.public = true
	}
}

// Deprecated marks the operation deprecated.
func Deprecated() Option {
	return func(op *operation) {
		op.deprecated = true
	}
}

// Doc sets the operation summary and, optionally, its description.
func Doc(summary string, description ...string) Option {
	return func(op *operation) {
		op.summary = summary
		op.description = strings.Join(description, "\n\n")
	}
}

// Middleware adds per-route Echo middleware.
func Middleware(m ...echo.MiddlewareFunc) Option {
	return func(op *operation) {
		op.middleware = append(op.middleware, m...)
	}
}

// Group creates a top-level group. Options apply to every operation in it.
func (r *Registry) Group(prefix string, opts ...Option) *Group {
	return &Group{
		reg:    r,
		echo:   r.echo.Group(prefix),
		prefix: prefix,
		tag:    tagOf(prefix),
		opts:   opts,
	}
}

// Group creates a nested group. It inherits the parent's options, and its tag
// is derived from the full prefix.
func (g *Group) Group(prefix string, opts ...Option) *Group {
	full := g.prefix + prefix
	return &Group{
		reg:    g.reg,
		echo:   g.echo.Group(prefix),
		prefix: full,
		tag:    tagOf(full),
		opts:   append(append([]Option(nil), g.opts...), opts...),
	}
}

// Use adds Echo middleware to the group.
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
