package openapi

import (
	"encoding"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/primitives/enums"
	"github.com/vanclief/compose/types"
)

// RefPrefix is the prefix of every $ref produced by the Reflector.
const RefPrefix = "#/components/schemas/"

var (
	timeType            = reflect.TypeFor[time.Time]()
	schemaProviderType  = reflect.TypeFor[huma.SchemaProvider]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// builtinSchemas maps types whose schema we know better than huma's default.
var builtinSchemas = map[reflect.Type]huma.Schema{
	reflect.TypeFor[uuid.UUID]():         {Type: huma.TypeString, Format: "uuid"},
	reflect.TypeFor[types.Email]():       {Type: huma.TypeString, Format: "email"},
	reflect.TypeFor[types.PhoneNumber](): {Type: huma.TypeString, Format: "phone"},
	reflect.TypeFor[types.Locale]():      {Type: huma.TypeString},
	reflect.TypeFor[types.Language]():    {Type: huma.TypeString},
	reflect.TypeFor[types.UnixSeconds](): {Type: huma.TypeInteger, Format: "int64"},
}

// Reflector collects request and response types and produces components.
// It is not safe for concurrent use.
type Reflector struct {
	reg       *registry
	requests  map[reflect.Type]*requestRoot
	reqOrder  []reflect.Type
	responses map[reflect.Type]*huma.Schema
	respOrder []reflect.Type
	result    *Result
}

type requestRoot struct {
	name   string
	params []Param
}

// Option configures a Reflector.
type Option func(*Reflector)

// WithNamer sets the component namer. It receives the dereferenced named
// type (possibly a generic instantiation) and returns its component name.
// Unnamed types keep huma's hint-based names. Default: DefaultNamer.
func WithNamer(namer func(t reflect.Type) string) Option {
	return func(r *Reflector) {
		if namer != nil {
			r.reg.namer = namer
		}
	}
}

// NewReflector returns an empty Reflector.
func NewReflector(opts ...Option) *Reflector {
	r := &Reflector{
		reg: &registry{
			schemas: map[string]*huma.Schema{},
			types:   map[string]reflect.Type{},
			seen:    map[reflect.Type]bool{},
			aliases: map[reflect.Type]reflect.Type{},
			namer:   DefaultNamer,
		},
		requests:  map[reflect.Type]*requestRoot{},
		responses: map[reflect.Type]*huma.Schema{},
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// AddResponse registers t (struct, pointer to struct, or slice/map thereof)
// as a response type. Idempotent. Returns lint errors.
func (r *Reflector) AddResponse(t reflect.Type) (err error) {
	if t == nil {
		return ez.New(ez.EINVALID, "openapi: nil response type", nil)
	}
	if r.result != nil {
		return ez.New(ez.EINVALID, "openapi: AddResponse called after Finalize", nil)
	}

	t = deref(t)
	_, ok := r.responses[t]
	if ok {
		return nil
	}

	err = lintType(t)
	if err != nil {
		return ez.Wrap(err)
	}

	defer recoverPanic(&err)
	r.responses[t] = r.reg.Schema(t, true, "Response")
	r.respOrder = append(r.respOrder, t)

	return nil
}

// AddRequest registers t (struct or pointer to struct) as a request root,
// lints it, and returns its parameters. Idempotent for the same t.
func (r *Reflector) AddRequest(t reflect.Type) (params []Param, err error) {
	if t == nil {
		return nil, ez.New(ez.EINVALID, "openapi: nil request type", nil)
	}
	if r.result != nil {
		return nil, ez.New(ez.EINVALID, "openapi: AddRequest called after Finalize", nil)
	}

	t = deref(t)
	root, ok := r.requests[t]
	if ok {
		return copyParams(root.params), nil
	}
	if t.Kind() != reflect.Struct || isOpaque(t) {
		return nil, ez.New(ez.EINVALID, fmt.Sprintf("openapi: request type %s must be a plain struct", t), nil)
	}

	err = lintType(t)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	defer recoverPanic(&err)

	params, err = buildParams(r.reg, t)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	s := r.reg.Schema(t, true, "Request")
	err = r.reg.checkRequestKeys(t)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	root = &requestRoot{name: strings.TrimPrefix(s.Ref, RefPrefix), params: params}
	r.requests[t] = root
	r.reqOrder = append(r.reqOrder, t)

	return copyParams(params), nil
}

// recoverPanic turns a huma panic (duplicate names, malformed tag values)
// into an error.
func recoverPanic(err *error) {
	rec := recover()
	if rec != nil {
		*err = ez.New(ez.EINVALID, fmt.Sprintf("openapi: %v", rec), nil)
	}
}

// registry implements huma.Registry. It mirrors huma's map registry, but
// because huma.SchemaFromType receives this registry, every nested field
// passes through Schema and gets the compose hooks (builtins and enums).
type registry struct {
	schemas map[string]*huma.Schema
	types   map[string]reflect.Type
	seen    map[reflect.Type]bool
	aliases map[reflect.Type]reflect.Type
	namer   func(reflect.Type) string
}

// collisionHint is appended to duplicate component name errors.
const collisionHint = "; give one component a distinct name with openapi.WithNamer (e.g. openapi.SegmentsNamer(2)) or api.Namer"

var _ huma.Registry = (*registry)(nil)

func (r *registry) Schema(t reflect.Type, allowRef bool, hint string) *huma.Schema {
	origType := t
	t = deref(t)

	// Pointer to array decays to array.
	if t.Kind() == reflect.Array || t.Kind() == reflect.Slice {
		origType = t
	}

	alias, ok := r.aliases[t]
	if ok {
		return r.Schema(alias, allowRef, hint)
	}

	builtin, ok := builtinSchemas[t]
	if ok {
		builtin.Nullable = origType.Kind() == reflect.Pointer
		return &builtin
	}

	values, ok := enums.Values(t)
	if ok {
		// Always a fresh pointer: a field-level enum tag mutates it.
		// Pointers are nullable; Finalize rewrites a nullable $ref as
		// anyOf [ref, null].
		return &huma.Schema{Ref: RefPrefix + r.registerEnum(t, values), Nullable: origType.Kind() == reflect.Pointer}
	}

	getsRef := t.Kind() == reflect.Struct && !isOpaque(t)
	name := r.schemaName(origType, hint)

	if getsRef {
		_, exists := r.schemas[name]
		if exists {
			if r.seen[t] {
				if allowRef {
					return &huma.Schema{Ref: RefPrefix + name}
				}
				return r.schemas[name]
			}

			if t.Name() != "" {
				panic(fmt.Errorf("duplicate component name %s for %s and %s%s", name, t, r.types[name], collisionHint))
			}

			name = r.freeName(name)
		}

		// Register first so recursive types can reference themselves.
		r.schemas[name] = &huma.Schema{}
		r.types[name] = t
		r.seen[t] = true
	}

	s := huma.SchemaFromType(r, origType)
	if !getsRef {
		if s != nil && s.Type == huma.TypeInteger && !isOpaque(t) {
			setIntRange(s, t.Kind())
		}
		return s
	}

	r.schemas[name] = s
	if allowRef {
		return &huma.Schema{Ref: RefPrefix + name}
	}
	return s
}

func (r *registry) freeName(base string) string {
	for i := 1; ; i++ {
		name := fmt.Sprintf("%s%d", base, i)
		_, exists := r.schemas[name]
		if !exists {
			return name
		}
	}
}

func (r *registry) registerEnum(t reflect.Type, values []string) string {
	name := r.schemaName(t, "")
	existing, ok := r.types[name]
	if ok {
		if existing != t {
			panic(fmt.Errorf("duplicate component name %s for %s and %s%s", name, t, existing, collisionHint))
		}
		return name
	}

	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}

	r.schemas[name] = &huma.Schema{Type: huma.TypeString, Enum: enum}
	r.types[name] = t
	r.seen[t] = true

	return name
}

func (r *registry) SchemaFromRef(ref string) *huma.Schema {
	if !strings.HasPrefix(ref, RefPrefix) {
		return nil
	}
	return r.schemas[strings.TrimPrefix(ref, RefPrefix)]
}

func (r *registry) TypeFromRef(ref string) reflect.Type {
	return r.types[strings.TrimPrefix(ref, RefPrefix)]
}

func (r *registry) Map() map[string]*huma.Schema {
	return r.schemas
}

func (r *registry) RegisterTypeAlias(t reflect.Type, alias reflect.Type) {
	r.aliases[t] = alias
}

var (
	pkgPathPrefix  = regexp.MustCompile(`(?:[\w.\-~]+/)+`)
	genericCleaner = strings.NewReplacer("[]", "List", "*", "Ptr", "[", "_", "]", "_", ",", "_", " ", "")
)

// schemaName names named types with the configured namer and unnamed types
// with huma's hint-based naming.
func (r *registry) schemaName(t reflect.Type, hint string) string {
	t = deref(t)
	if t.Name() == "" {
		return huma.DefaultSchemaNamer(t, hint)
	}
	return r.namer(t)
}

// DefaultNamer returns `<lastPathSegment>.<TypeName>`. Generic arguments are
// package-qualified too: Page[a.User] becomes `pkg.Page_a.User`. It is
// SegmentsNamer(1).
func DefaultNamer(t reflect.Type) string {
	return segmentsName(t, 1)
}

// SegmentsNamer returns a namer that qualifies type names (and generic
// arguments) with the last n package path segments joined by ".":
// SegmentsNamer(2) names finance/units.ListRequest `finance.units.ListRequest`.
// n below 1 is treated as 1.
func SegmentsNamer(n int) func(reflect.Type) string {
	if n < 1 {
		n = 1
	}
	return func(t reflect.Type) string {
		return segmentsName(t, n)
	}
}

func segmentsName(t reflect.Type, n int) string {
	t = deref(t)
	name := t.Name()
	if name == "" {
		return huma.DefaultSchemaNamer(t, "")
	}

	base, args, generic := strings.Cut(name, "[")
	if generic {
		args = pkgPathPrefix.ReplaceAllStringFunc(strings.TrimSuffix(args, "]"), func(prefix string) string {
			return lastSegments(strings.TrimSuffix(prefix, "/"), n-1, ".")
		})
		base += "_" + strings.TrimRight(genericCleaner.Replace(args), "_")
	}

	if t.PkgPath() == "" {
		return base
	}
	return lastSegments(t.PkgPath(), n, "") + "." + base
}

// lastSegments joins the last n segments of a slash-separated path with ".",
// followed by suffix when non-empty. n <= 0 yields "".
func lastSegments(p string, n int, suffix string) string {
	if n <= 0 {
		return ""
	}
	parts := strings.Split(p, "/")
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	return strings.Join(parts, ".") + suffix
}

// setIntRange bounds sized integer kinds to what Go can hold, so values that
// pass the schema never fail conversion. Explicit minimum/maximum tags are
// applied later by huma and linted to stay within these ranges.
func setIntRange(s *huma.Schema, kind reflect.Kind) {
	lo, hi, ok := intRange(kind)
	if !ok {
		return
	}
	s.Minimum = &lo
	if hi != nil {
		s.Maximum = hi
	}
	if kind == reflect.Uint32 {
		s.Format = "int64"
	}
}

// intRange returns the bounds published for an integer kind. hi is nil when
// no maximum is published (uint, uint64). ok is false for int and int64.
func intRange(kind reflect.Kind) (lo float64, hi *float64, ok bool) {
	var max float64
	switch kind {
	case reflect.Int8:
		lo, max = math.MinInt8, math.MaxInt8
	case reflect.Int16:
		lo, max = math.MinInt16, math.MaxInt16
	case reflect.Int32:
		lo, max = math.MinInt32, math.MaxInt32
	case reflect.Uint8:
		max = math.MaxUint8
	case reflect.Uint16:
		max = math.MaxUint16
	case reflect.Uint32:
		max = math.MaxUint32
	case reflect.Uint, reflect.Uint64:
		return 0, nil, true
	default:
		return 0, nil, false
	}
	return lo, &max, true
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// isOpaque reports whether t gets a scalar schema instead of an object
// component: builtins, time.Time, schema providers and text unmarshalers.
func isOpaque(t reflect.Type) bool {
	_, ok := builtinSchemas[t]
	if ok || t == timeType {
		return true
	}
	ptr := reflect.PointerTo(t)
	return t.Implements(schemaProviderType) || ptr.Implements(schemaProviderType) ||
		t.Implements(textUnmarshalerType) || ptr.Implements(textUnmarshalerType)
}
