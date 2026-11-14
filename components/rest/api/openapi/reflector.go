package openapi

import (
	"encoding"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/primitives/enums"
	"github.com/vanclief/compose/types"
)

// RefPrefix is the prefix of every $ref produced by the Reflector.
const RefPrefix = "#/components/schemas/"

var (
	timeType            = reflect.TypeFor[time.Time]()
	schemaProviderType  = reflect.TypeFor[huma.SchemaProvider]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	rawMessageType      = reflect.TypeFor[json.RawMessage]()
	optionalTypeType    = reflect.TypeFor[optionalType]()
	urlType             = reflect.TypeFor[url.URL]()
)

// optionalType is implemented by types.Optional[T], a body field that records
// whether its key was present. It is matched by method, so types/ does not
// depend on this package.
type optionalType interface {
	OptionalType() reflect.Type
}

// optionalElem returns T when t is a types.Optional[T] (not a pointer to one).
func optionalElem(t reflect.Type) (reflect.Type, bool) {
	if t.Kind() != reflect.Struct || !t.Implements(optionalTypeType) {
		return nil, false
	}
	return reflect.Zero(t).Interface().(optionalType).OptionalType(), true
}

// IsOptional reports whether t, or the type it points to, is a
// types.Optional.
func IsOptional(t reflect.Type) bool {
	_, ok := optionalElem(deref(t))
	return ok
}

// minLengthOne is the minLength of the builtins whose text decoder reads ""
// as their zero value. Every schema of those builtins shares it: a field's
// minLength tag replaces the pointer, never writes through it.
var minLengthOne = 1

// builtinSchemas maps types whose schema we know better than huma's default:
// the shape encoding/json reads and writes. A pointer to one is nullable, and
// so is one marked Nullable here, which encoding/json writes as null.
var builtinSchemas = map[reflect.Type]huma.Schema{
	reflect.TypeFor[uuid.UUID]():         {Type: huma.TypeString, Format: "uuid"},
	reflect.TypeFor[types.Email]():       {Type: huma.TypeString, Format: "email"},
	reflect.TypeFor[types.PhoneNumber](): {Type: huma.TypeString, Format: "phone"},
	reflect.TypeFor[types.Locale]():      {Type: huma.TypeString},
	reflect.TypeFor[types.Language]():    {Type: huma.TypeString},
	reflect.TypeFor[types.UnixSeconds](): {Type: huma.TypeInteger, Format: "int64"},
	// A string kind, written as a bare number.
	reflect.TypeFor[json.Number](): {Type: huma.TypeNumber},
	// Its text decoder takes both families and turns "" into a nil IP without
	// an error, which the ip format refuses.
	reflect.TypeFor[net.IP](): {Type: huma.TypeString, Format: "ip"},
	// Their text decoders turn "" into the zero value without an error,
	// which minLength refuses; no standard format describes them.
	reflect.TypeFor[netip.Prefix]():   {Type: huma.TypeString, MinLength: &minLengthOne},
	reflect.TypeFor[netip.AddrPort](): {Type: huma.TypeString, MinLength: &minLengthOne},
	// A text unmarshaler, but read and written as a JSON number.
	reflect.TypeFor[big.Int](): {Type: huma.TypeInteger},
	// null when not Valid.
	reflect.TypeFor[uuid.NullUUID](): {Type: huma.TypeString, Format: "uuid", Nullable: true},
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
			names:   map[reflect.Type]string{},
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
// as a response type. Idempotent. Returns lint errors, and an error when a
// struct it reaches sends a JSON key its schema does not list (see
// checkKeys).
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

	err = lintType(t, false)
	if err != nil {
		return ez.Wrap(err)
	}

	defer recoverPanic(&err)
	s := r.reg.Schema(t, true, "Response")
	err = r.reg.checkKeys(t, false)
	if err != nil {
		return ez.Wrap(err)
	}

	r.responses[t] = s
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

	err = lintType(t, true)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	defer recoverPanic(&err)

	params, err = buildParams(r.reg, t)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	s := r.reg.Schema(t, true, "Request")
	err = r.reg.checkKeys(t, true)
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
	if rec == nil {
		return
	}
	msg := fmt.Sprint(rec)
	ezErr, ok := rec.(*ez.Error)
	if ok {
		msg = ez.ErrorMessage(ezErr)
	}
	*err = ez.New(ez.EINVALID, "openapi: "+msg, nil)
}

// recordedEnums is enums.Values. A test replaces it to reach the guard of
// enumValues, as enums.Set refuses the types the guard is for.
var recordedEnums = enums.Values

// enumValues returns the values enums.Set recorded for t. A type without a
// package path, such as string, is never an enum: enums.Set refuses it, and
// an entry for one would make every field of that type the enum.
func enumValues(t reflect.Type) ([]string, bool) {
	if t.PkgPath() == "" {
		return nil, false
	}
	return recordedEnums(t)
}

// registry implements huma.Registry. It mirrors huma's map registry, but
// because huma.SchemaFromType receives this registry, every nested field
// passes through Schema and gets the compose hooks (builtins and enums).
type registry struct {
	schemas map[string]*huma.Schema
	types   map[string]reflect.Type
	names   map[reflect.Type]string // the component of each type, see Schema
	aliases map[reflect.Type]reflect.Type
	namer   func(reflect.Type) string
}

// collisionHint is appended to duplicate component name errors.
const collisionHint = "; give one component a distinct name with openapi.WithNamer (e.g. openapi.SegmentsNamer(2)) or api.Namer"

var _ huma.Registry = (*registry)(nil)

func (r *registry) Schema(t reflect.Type, allowRef bool, hint string) *huma.Schema {
	origType := t
	t = deref(t)

	// A builtin is nullable behind a pointer whatever its kind: uuid.UUID is
	// an array and net.IP a slice, for which origType decays below.
	pointer := origType.Kind() == reflect.Pointer

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
		builtin.Nullable = builtin.Nullable || pointer
		return &builtin
	}

	values, ok := enumValues(t)
	if ok {
		// Always a fresh pointer: a field-level enum tag mutates it.
		// Pointers are nullable; Finalize rewrites a nullable $ref as
		// anyOf [ref, null].
		return &huma.Schema{Ref: RefPrefix + r.registerEnum(t, values), Nullable: origType.Kind() == reflect.Pointer}
	}

	elem, ok := optionalElem(t)
	if ok {
		return r.optionalSchema(elem, hint)
	}

	getsRef := t.Kind() == reflect.Struct && !isOpaque(t)
	name := r.schemaName(origType, hint)

	if getsRef {
		// Looked up by type, not by name: an anonymous struct whose hint
		// was taken got a numbered name, and the hint's own component is
		// another type's.
		known, ok := r.names[t]
		if ok {
			if allowRef {
				return &huma.Schema{Ref: RefPrefix + known}
			}
			return r.schemas[known]
		}

		_, exists := r.schemas[name]
		if exists {
			if t.Name() != "" {
				panic(ez.New(ez.EINVALID, fmt.Sprintf("duplicate component name %s for %s and %s%s", name, t, r.types[name], collisionHint), nil))
			}

			name = r.freeName(name)
		}

		// Register first so recursive types can reference themselves.
		r.schemas[name] = &huma.Schema{}
		r.types[name] = t
		r.names[t] = name
	}

	s := huma.SchemaFromType(r, origType)
	if !getsRef {
		if s != nil && s.Type == huma.TypeInteger && !isOpaque(t) {
			setIntRange(s, t.Kind())
		}
		// The pointer decayed above, so huma did not see it: a nil pointer
		// to a fixed-size array, or to a text type of array or slice kind,
		// is written as null.
		if pointer && origType.Kind() != reflect.Pointer && s != nil && s.Type != "" {
			s.Nullable = true
		}
		return s
	}

	r.schemas[name] = s
	if allowRef {
		return &huma.Schema{Ref: RefPrefix + name}
	}
	return s
}

// optionalSchema returns the schema of a types.Optional[elem] field: elem's
// schema made nullable, the way a pointer field is in a request. Enums and
// scalars are marked Nullable, so field tags (enum, minLength) still apply
// to elem's schema and Finalize rewrites a nullable enum $ref as
// anyOf [ref, null]. A struct $ref is wrapped now, because huma rejects a
// nullable object reference.
func (r *registry) optionalSchema(elem reflect.Type, hint string) *huma.Schema {
	s := r.Schema(elem, true, hint)
	if s == nil {
		return nil
	}
	if s.Ref != "" {
		typ := r.TypeFromRef(s.Ref)
		if typ == nil || typ.Kind() == reflect.Struct {
			wrapNull(s)
			return s
		}
	}
	if s.Ref != "" || s.Type != "" {
		s.Nullable = true
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
			panic(ez.New(ez.EINVALID, fmt.Sprintf("duplicate component name %s for %s and %s%s", name, t, existing, collisionHint), nil))
		}
		return name
	}

	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}

	r.schemas[name] = &huma.Schema{Type: huma.TypeString, Enum: enum}
	r.types[name] = t
	r.names[t] = name

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
// component: builtins, time.Time, types.Optional, schema providers and text
// unmarshalers.
func isOpaque(t reflect.Type) bool {
	_, ok := builtinSchemas[t]
	if ok || t == timeType || t.Implements(optionalTypeType) {
		return true
	}
	ptr := reflect.PointerTo(t)
	return t.Implements(schemaProviderType) || ptr.Implements(schemaProviderType) ||
		t.Implements(textUnmarshalerType) || ptr.Implements(textUnmarshalerType)
}
