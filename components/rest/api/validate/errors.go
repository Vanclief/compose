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

// collector gathers the FieldErrors of a santhosh error tree in the order
// NewFieldsError sorts them by, at most MaxFields distinct ones.
type collector struct {
	seen   map[FieldError]bool
	fields []FieldError
	// full is set at the first distinct field past MaxFields, where the walk
	// stops, so its cost is bounded by the cap and not by how many values
	// failed.
	full bool
}

// collect walks the santhosh error tree and maps its leaves to FieldErrors
// until more than MaxFields distinct fields failed. It returns the fields
// found, and true when it stopped at the cap. root is the compiled schema
// the instance was validated against; it tells declared property names
// apart from client-chosen map keys.
func collect(root *jsonschema.Schema, verr *jsonschema.ValidationError) ([]FieldError, bool) {
	c := &collector{seen: map[FieldError]bool{}}
	c.location([]*jsonschema.ValidationError{verr}, nil, expand([]*jsonschema.Schema{root}))
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

// entry is what failed at a location or under it: a field of the location
// itself (name ""), a field named after one of its keys (a missing required
// property, an unknown key), or an error node at or under a key. A node is
// named as fields show its key: followed by a dot when the fields it leads
// to are under the key, as their names continue so.
type entry struct {
	rank  int32
	index int32
	name  string
	field *FieldError
	node  *jsonschema.ValidationError
}

// compareEntries orders the entries of one location as NewFieldsError
// orders the fields they lead to: by request location at the top, then by
// name with a run of digits compared as a number, fields of one name by
// message and before the nodes of that name. With the dot, the fields of a
// key "a" itself come before a sibling "a-b" and those under it after,
// where "a.x" sorts. Two cases differ, both on declared property names: a
// name holding a dot ("a.b") comes after the nodes under the key it
// starts with ("a"), where its field sorts among theirs, and the nodes of
// names equal as numbers ("01" and "1") are ordered as the names, where
// their fields are ordered by the rest of their names.
func compareEntries(a, b entry) int {
	if a.rank != b.rank {
		return cmp.Compare(a.rank, b.rank)
	}
	// Equal names are usually one string, made once from the client's key,
	// which can be long: == compares it at once where compareNames walks
	// it.
	if a.name != b.name {
		return compareNames(a.name, b.name)
	}
	// A field named with a dot ("x.", a declared name) can meet the nodes
	// under a key "x": the field comes first.
	if (a.field == nil) != (b.field == nil) {
		if a.field != nil {
			return -1
		}
		return 1
	}
	if a.field != nil && a.field.Message != b.field.Message {
		return strings.Compare(a.field.Message, b.field.Message)
	}
	return cmp.Compare(a.index, b.index)
}

// location reports the fields of nodes, the error nodes at one instance
// location, in order: display is the location as fields show it, schemas
// the schemas describing its value. It gathers what failed at the location
// and the nodes at or under each of its keys, sorts them, and walks the
// names one after the other, until the cap. The map keys all show as "*",
// so the nodes of all of them are walked as one.
func (c *collector) location(nodes []*jsonschema.ValidationError, display []string, schemas []*jsonschema.Schema) {
	g := gathering{display: display, schemas: schemas, entries: make([]entry, 0, len(nodes))}
	for _, n := range nodes {
		g.gather(n)
	}
	entries := g.entries
	if !slices.IsSortedFunc(entries, compareEntries) {
		slices.SortFunc(entries, compareEntries)
	}

	depth := len(display)
	for i := 0; i < len(entries) && !c.full; {
		if entries[i].field != nil {
			c.add(*entries[i].field)
			i++
			continue
		}

		name := entries[i].name
		var under []*jsonschema.ValidationError
		var next []*jsonschema.Schema
		shown := mapKeySegment
		last := ""
		for i < len(entries) && entries[i].field == nil && entries[i].name == name {
			n := entries[i].node
			under = append(under, n)
			i++
			key := n.InstanceLocation[depth]
			if len(under) > 1 && key == last {
				continue
			}
			last = key
			more, keep := step(schemas, key)
			if keep {
				shown = key
			}
			for _, s := range more {
				if !slices.Contains(next, s) {
					next = append(next, s)
				}
			}
		}
		c.location(under, append(display[:depth:depth], shown), expand(next))
	}
}

// gathering collects the entries of one location.
type gathering struct {
	display []string
	schemas []*jsonschema.Schema
	entries []entry
	// key and name are the last key met and the name, with the dot, of the
	// nodes under it: the nodes of one key usually come one after the
	// other, and the key can be long to look up.
	key  string
	name string
}

func (g *gathering) push(e entry) {
	// Only the first segment is the request location; deeper ones rank
	// alike.
	if len(g.display) == 0 {
		e.rank = int32(locationRank(strings.TrimSuffix(e.name, ".")))
	}
	e.index = int32(len(g.entries))
	g.entries = append(g.entries, e)
}

func (g *gathering) pushField(name string, f FieldError) {
	g.push(entry{name: name, field: &f})
}

// gather adds n: a node at or under one of the location's keys, or what n
// and the nodes under it at the location failed on.
func (g *gathering) gather(n *jsonschema.ValidationError) {
	depth := len(g.display)
	if len(n.InstanceLocation) > depth {
		key := n.InstanceLocation[depth]
		if g.name == "" || key != g.key {
			g.key = key
			g.name = mapKeySegment + "."
			_, keep := step(g.schemas, key)
			if keep {
				g.name = key + "."
			}
		}
		g.atKey(n)
		return
	}
	switch n.ErrorKind.(type) {
	case *kind.AnyOf, *kind.OneOf:
		if len(n.Causes) > 0 {
			want, rest := alternatives(n)
			if len(rest) == 0 {
				g.pushField("", newField(g.display, "", "must be "+typeList(want)))
			}
			for _, leaf := range rest {
				g.gather(leaf)
			}
			return
		}
	}
	if len(n.Causes) > 0 {
		for _, cause := range causes(n) {
			g.gather(cause)
		}
		return
	}

	switch k := n.ErrorKind.(type) {
	case *kind.Required:
		for _, name := range k.Missing {
			g.pushField(name, newField(g.display, name, "is required"))
		}
	case *kind.AdditionalProperties:
		for _, name := range k.Properties {
			shown := unknownName(name)
			g.pushField(shown, newField(g.display, shown, "is not a known field"))
		}
	default:
		g.pushField("", newField(g.display, "", message(n.ErrorKind)))
	}
}

// atKey adds n, a node at or under the key of g.name, split as gather
// splits the nodes at the key: what fails at the key itself goes under the
// key's name, what fails under it under the name with the dot.
func (g *gathering) atKey(n *jsonschema.ValidationError) {
	under := entry{name: g.name, node: n}
	at := entry{name: g.name[:len(g.name)-1], node: n}
	if len(n.InstanceLocation) > len(g.display)+1 {
		g.push(under)
		return
	}
	switch n.ErrorKind.(type) {
	case *kind.AnyOf, *kind.OneOf:
		if len(n.Causes) > 0 {
			_, rest := alternatives(n)
			if len(rest) == 0 {
				g.push(at)
				return
			}
			for _, leaf := range rest {
				g.atKey(leaf)
			}
			return
		}
	}
	if len(n.Causes) > 0 {
		for _, cause := range causes(n) {
			g.atKey(cause)
		}
		return
	}

	switch n.ErrorKind.(type) {
	case *kind.Required, *kind.AdditionalProperties:
		g.push(under)
	default:
		g.push(at)
	}
}

// alternatives splits the leaves under an anyOf or oneOf node: the types
// wanted where a branch failed only on type at the node's location, and
// the other, more specific failures. When there are none of those the
// wanted types make one message ("must be string or null").
func alternatives(verr *jsonschema.ValidationError) ([]string, []*jsonschema.ValidationError) {
	var leaves []*jsonschema.ValidationError
	for _, cause := range verr.Causes {
		leaves = appendLeaves(leaves, cause)
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
	return want, rest
}

// appendLeaves appends the nodes without causes under verr to leaves.
func appendLeaves(leaves []*jsonschema.ValidationError, verr *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(verr.Causes) == 0 {
		return append(leaves, verr)
	}
	for _, cause := range causes(verr) {
		leaves = appendLeaves(leaves, cause)
	}
	return leaves
}

// causes returns the causes of verr without the multipleOf failure of a
// number that also failed tooManyDigits, a cause at the same location: the
// multipleOf answer came from a text that does not hold the number's
// remaining digits. A multipleOf failure at another location, of another
// field or item, stays. The locations are looked up in a map, so an array of
// many items of both kinds costs one pass.
func causes(verr *jsonschema.ValidationError) []*jsonschema.ValidationError {
	digits := map[string]bool{}
	for _, cause := range verr.Causes {
		_, ok := cause.ErrorKind.(*tooManyDigits)
		if ok {
			digits[locationKey(cause.InstanceLocation)] = true
		}
	}
	if len(digits) == 0 {
		return verr.Causes
	}
	return slices.DeleteFunc(slices.Clone(verr.Causes), func(cause *jsonschema.ValidationError) bool {
		_, ok := cause.ErrorKind.(*kind.MultipleOf)
		if !ok {
			return false
		}
		return digits[locationKey(cause.InstanceLocation)]
	})
}

// locationKey returns a map key for an instance location, a different key
// for each location.
func locationKey(location []string) string {
	return fmt.Sprintf("%q", location)
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
	case *tooManyDigits:
		return "has too many digits to be checked as a multiple of " + ratString(k.Want)
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

	// The schemas of a value are a handful: a linear search is cheaper
	// than a map.
	var visit func(s *jsonschema.Schema)
	visit = func(s *jsonschema.Schema) {
		if s == nil || slices.Contains(out, s) {
			return
		}
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
