package validate

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/vanclief/ez"
)

// FieldError is one failing field. In is the request location ("path",
// "query", "header" or "body"), Name the dotted path inside it ("status",
// "config.name", "tags.2", or "" for the location itself) and Message the
// reason, which never contains the submitted value.
type FieldError struct {
	In      string
	Name    string
	Message string
}

// MaxFields is the most fields one error lists. A request can fail on as
// many fields as it has values, so an uncapped list would make the answer
// many times the size of the request that caused it.
const MaxFields = 100

// fieldsError is the cause of the ez.EINVALID returned by NewFieldsError.
type fieldsError struct {
	fields  []FieldError
	message string
}

func (e *fieldsError) Error() string {
	return e.message
}

// NewFieldsError builds a single ez.EINVALID error from fields, sorted by
// location (path, query, header, body) then name, a run of digits in a name
// compared as a number, with duplicates removed.
// The message joins "<name> <message>" with "; ", prefixing parameter names
// with their location. Only the first MaxFields are kept: the message then
// ends with "; and <n> more", and Fields returns the ones kept. Returns nil
// when fields is empty.
func NewFieldsError(fields []FieldError) error {
	return newFieldsError(fields, false)
}

// newFieldsError is NewFieldsError for a list that stopped at the cap, when
// truncated is true: more fields failed than it holds, and the message ends
// with "; and more", as it has no count of them.
func newFieldsError(fields []FieldError, truncated bool) error {
	fields = normalize(fields)
	if len(fields) == 0 {
		return nil
	}

	more := len(fields) - MaxFields
	if more > 0 {
		fields = slices.Clone(fields[:MaxFields])
	}

	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = displayName(f) + " " + f.Message
	}
	message := strings.Join(parts, "; ")
	switch {
	case truncated:
		message += "; and more"
	case more > 0:
		message += "; and " + strconv.Itoa(more) + " more"
	}

	return ez.New(ez.EINVALID, message, &fieldsError{fields: fields, message: message})
}

// Fields returns the structured field errors carried by err, at most
// MaxFields of them, or nil.
func Fields(err error) []FieldError {
	var fe *fieldsError
	if !errors.As(err, &fe) {
		return nil
	}
	return slices.Clone(fe.fields)
}

func displayName(f FieldError) string {
	switch {
	case f.In == "" && f.Name == "":
		return "request"
	case f.In == "body" && f.Name != "":
		return f.Name
	case f.Name == "":
		return f.In
	}
	return f.In + " " + f.Name
}

var locationOrder = map[string]int{"path": 0, "query": 1, "header": 2, "body": 3}

func locationRank(in string) int {
	rank, ok := locationOrder[in]
	if !ok {
		return len(locationOrder)
	}
	return rank
}

func normalize(fields []FieldError) []FieldError {
	out := slices.Clone(fields)
	slices.SortFunc(out, func(a, b FieldError) int {
		rank := locationRank(a.In) - locationRank(b.In)
		if rank != 0 {
			return rank
		}
		names := compareNames(a.Name, b.Name)
		if names != 0 {
			return names
		}
		return strings.Compare(a.Message, b.Message)
	})
	return slices.Compact(out)
}

// compareNames orders field names as strings.Compare does, except that a run
// of digits is compared as a number, so "steps.2" comes before "steps.10"
// and the fields NewFieldsError keeps of a long array are its first items.
func compareNames(a, b string) int {
	x := a
	y := b
	for x != "" && y != "" {
		if !isDigit(x[0]) || !isDigit(y[0]) {
			if x[0] != y[0] {
				return cmp.Compare(x[0], y[0])
			}
			x = x[1:]
			y = y[1:]
			continue
		}

		xn := digitRun(x)
		yn := digitRun(y)
		xv := strings.TrimLeft(x[:xn], "0")
		yv := strings.TrimLeft(y[:yn], "0")
		if len(xv) != len(yv) {
			return cmp.Compare(len(xv), len(yv))
		}
		if xv != yv {
			return strings.Compare(xv, yv)
		}
		x = x[xn:]
		y = y[yn:]
	}
	if len(x) != len(y) {
		return cmp.Compare(len(x), len(y))
	}
	// Equal as numbers, such as "a.01" and "a.1": keep the order total.
	return strings.Compare(a, b)
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// digitRun returns the length of the run of digits s starts with.
func digitRun(s string) int {
	n := 0
	for n < len(s) && isDigit(s[n]) {
		n++
	}
	return n
}

// collector gathers the FieldErrors of a santhosh error tree, at most
// MaxFields distinct ones. root is the compiled schema the instance was
// validated against; it tells declared property names apart from
// client-chosen map keys.
type collector struct {
	root   *jsonschema.Schema
	seen   map[FieldError]bool
	fields []FieldError
	// full is set at the first distinct field past MaxFields, where the walk
	// stops, so its cost is bounded by the cap and not by how many values
	// failed.
	full bool
}

// collect walks the santhosh error tree and maps its leaves to FieldErrors,
// in the order of their instance locations, until more than MaxFields
// distinct fields failed. It returns the fields found, and true when it
// stopped at the cap. It sorts the causes of the tree's nodes in place.
func collect(root *jsonschema.Schema, verr *jsonschema.ValidationError) ([]FieldError, bool) {
	c := &collector{root: root, seen: map[FieldError]bool{}}
	c.walk(verr)
	return c.fields, c.full
}

func (c *collector) add(f FieldError) {
	if c.full || c.seen[f] {
		return
	}
	if len(c.fields) == MaxFields {
		c.full = true
		return
	}
	c.seen[f] = true
	c.fields = append(c.fields, f)
}

func (c *collector) walk(verr *jsonschema.ValidationError) {
	switch verr.ErrorKind.(type) {
	case *kind.AnyOf, *kind.OneOf:
		c.alternatives(verr)
		return
	}

	if len(verr.Causes) == 0 {
		c.leaf(verr)
		return
	}

	for _, cause := range sortedCauses(verr) {
		if c.full {
			return
		}
		c.walk(cause)
	}
}

// alternatives handles anyOf/oneOf. When every branch failed only on type
// at this location the wanted types are merged into one message ("must be
// string or null"). Otherwise branches that failed on type are dropped and
// the remaining, more specific errors are reported.
func (c *collector) alternatives(verr *jsonschema.ValidationError) {
	if len(verr.Causes) == 0 {
		c.leaf(verr)
		return
	}

	var leaves []*jsonschema.ValidationError
	for _, cause := range sortedCauses(verr) {
		leaves = append(leaves, flatten(cause)...)
	}

	var want []string
	var rest []*jsonschema.ValidationError
	for _, leaf := range leaves {
		typ, ok := leaf.ErrorKind.(*kind.Type)
		if ok && slices.Equal(leaf.InstanceLocation, verr.InstanceLocation) {
			want = append(want, typ.Want...)
			continue
		}
		rest = append(rest, leaf)
	}

	if len(rest) == 0 {
		c.add(newField(displayPath(c.root, verr.InstanceLocation), "", "must be "+typeList(want)))
		return
	}

	for _, leaf := range rest {
		if c.full {
			return
		}
		c.walk(leaf)
	}
}

func flatten(verr *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(verr.Causes) == 0 {
		return []*jsonschema.ValidationError{verr}
	}
	var leaves []*jsonschema.ValidationError
	for _, cause := range sortedCauses(verr) {
		leaves = append(leaves, flatten(cause)...)
	}
	return leaves
}

// sortedCauses sorts the causes of verr in place by instance location,
// keeping the order of causes at the same location, and returns them. The
// validator lists the properties of an object in map order, so without it
// the fields found before the cap would change from one request to the
// next.
func sortedCauses(verr *jsonschema.ValidationError) []*jsonschema.ValidationError {
	byLocation := func(a, b *jsonschema.ValidationError) int {
		return compareLocations(a.InstanceLocation, b.InstanceLocation)
	}
	if !slices.IsSortedFunc(verr.Causes, byLocation) {
		slices.SortStableFunc(verr.Causes, byLocation)
	}
	return verr.Causes
}

// compareLocations orders instance locations as NewFieldsError orders
// names: the request location first, then segment by segment with a run of
// digits compared as a number, a location before the ones inside it.
func compareLocations(a, b []string) int {
	for i := range min(len(a), len(b)) {
		if i == 0 {
			rank := locationRank(a[0]) - locationRank(b[0])
			if rank != 0 {
				return rank
			}
		}
		names := compareNames(a[i], b[i])
		if names != 0 {
			return names
		}
	}
	return cmp.Compare(len(a), len(b))
}

func (c *collector) leaf(verr *jsonschema.ValidationError) {
	segments := displayPath(c.root, verr.InstanceLocation)

	switch k := verr.ErrorKind.(type) {
	case *kind.Required:
		for _, name := range k.Missing {
			c.add(newField(segments, name, "is required"))
		}
		return
	case *kind.AdditionalProperties:
		// Listed in map order: sorted, the ones kept at the cap do not vary.
		names := slices.Clone(k.Properties)
		slices.Sort(names)
		for _, name := range names {
			c.add(newField(segments, unknownName(name), "is not a known field"))
		}
		return
	}

	c.add(newField(segments, "", message(verr.ErrorKind)))
}

func message(errKind jsonschema.ErrorKind) string {
	switch k := errKind.(type) {
	case *kind.Type:
		return "must be " + typeList(k.Want)
	case *kind.Enum:
		values := make([]string, len(k.Want))
		for i, v := range k.Want {
			values[i] = jsonValue(v)
		}
		return "must be one of " + strings.Join(values, ", ")
	case *kind.Format:
		return "must be a valid " + k.Want
	case *kind.Minimum:
		return "must be >= " + ratString(k.Want)
	case *kind.Maximum:
		return "must be <= " + ratString(k.Want)
	case *kind.ExclusiveMinimum:
		return "must be > " + ratString(k.Want)
	case *kind.ExclusiveMaximum:
		return "must be < " + ratString(k.Want)
	case *kind.MultipleOf:
		return "must be a multiple of " + ratString(k.Want)
	case *kind.MinLength:
		return "must be at least " + counted(k.Want, "character", "characters")
	case *kind.MaxLength:
		return "must be at most " + counted(k.Want, "character", "characters")
	case *kind.MinItems:
		return "must have at least " + counted(k.Want, "item", "items")
	case *kind.MaxItems:
		return "must have at most " + counted(k.Want, "item", "items")
	case *kind.MinProperties:
		return "must have at least " + counted(k.Want, "property", "properties")
	case *kind.MaxProperties:
		return "must have at most " + counted(k.Want, "property", "properties")
	case *kind.UniqueItems:
		return "must not contain duplicates"
	case *kind.Pattern:
		return "must match the expected format"
	case *kind.Const:
		return "must equal the fixed value"
	}
	return "is invalid"
}

// newField splits the display path of an instance location into its
// request location and the dotted name inside it, appending child when set.
// child must come from the schema or be sanitised by the caller.
func newField(segments []string, child string, msg string) FieldError {
	if child != "" {
		segments = append(segments[:len(segments):len(segments)], child)
	}
	if len(segments) == 0 {
		return FieldError{Message: msg}
	}
	return FieldError{In: segments[0], Name: strings.Join(segments[1:], "."), Message: msg}
}

// mapKeySegment replaces client-chosen map keys in field names.
const mapKeySegment = "*"

// displayPath renders an instance location for messages by walking it
// through the schema. Declared property names and array indices are kept;
// any other segment is a client-chosen map key (additionalProperties or
// patternProperties) and becomes "*", so names stay bounded and never echo
// submitted text.
func displayPath(root *jsonschema.Schema, loc []string) []string {
	out := make([]string, len(loc))
	current := expand([]*jsonschema.Schema{root})
	for i, segment := range loc {
		next, keep := step(current, segment)
		out[i] = mapKeySegment
		if keep {
			out[i] = segment
		}
		current = expand(next)
	}
	return out
}

// step returns the schemas describing the child segment of a value
// described by schemas, and whether segment may be shown verbatim: a
// declared property name, or an index that only an array schema matched. A
// map key that looks like a number is still the client's text.
func step(schemas []*jsonschema.Schema, segment string) ([]*jsonschema.Schema, bool) {
	var props, items, keys []*jsonschema.Schema
	n, err := strconv.Atoi(segment)
	index := err == nil && n >= 0 && strconv.Itoa(n) == segment

	for _, s := range schemas {
		prop, ok := s.Properties[segment]
		if ok {
			props = append(props, prop)
		}
		if index {
			items = append(items, itemSchema(s, n)...)
		}
		extra, ok := s.AdditionalProperties.(*jsonschema.Schema)
		if ok {
			keys = append(keys, extra)
		}
		for _, pattern := range s.PatternProperties {
			keys = append(keys, pattern)
		}
	}

	if len(props) > 0 {
		return props, true
	}
	return append(items, keys...), len(items) > 0 && len(keys) == 0
}

func itemSchema(s *jsonschema.Schema, n int) []*jsonschema.Schema {
	if n < len(s.PrefixItems) {
		return []*jsonschema.Schema{s.PrefixItems[n]}
	}
	if s.Items2020 != nil {
		return []*jsonschema.Schema{s.Items2020}
	}
	items, ok := s.Items.(*jsonschema.Schema)
	if ok {
		return []*jsonschema.Schema{items}
	}
	return nil
}

// expand adds every schema reachable through $ref, allOf, anyOf and oneOf.
func expand(schemas []*jsonschema.Schema) []*jsonschema.Schema {
	var out []*jsonschema.Schema
	seen := map[*jsonschema.Schema]bool{}

	var visit func(s *jsonschema.Schema)
	visit = func(s *jsonschema.Schema) {
		if s == nil || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
		visit(s.Ref)
		for _, list := range [][]*jsonschema.Schema{s.AllOf, s.AnyOf, s.OneOf} {
			for _, sub := range list {
				visit(sub)
			}
		}
	}

	for _, s := range schemas {
		visit(s)
	}
	return out
}

var unknownNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// unknownName renders an undeclared key reported by additionalProperties:
// false. It is the subject of the error, so plain names are kept; anything
// else (long, punctuation, control characters) becomes "*".
func unknownName(name string) string {
	if unknownNamePattern.MatchString(name) {
		return name
	}
	return mapKeySegment
}

// typeList joins type names with "or", without duplicates and with null last
// ("integer or null").
func typeList(types []string) string {
	var out []string
	nullable := false
	for _, t := range types {
		if t == "null" {
			nullable = true
			continue
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	if nullable {
		out = append(out, "null")
	}
	return strings.Join(out, " or ")
}

// jsonValue renders a schema value (never an instance value) for messages.
func jsonValue(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}

func ratString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// counted writes n before its noun: 1 character, 2 characters.
func counted(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
