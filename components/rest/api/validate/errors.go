package validate

import (
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

// fieldsError is the cause of the ez.EINVALID returned by NewFieldsError.
type fieldsError struct {
	fields  []FieldError
	message string
}

func (e *fieldsError) Error() string {
	return e.message
}

// NewFieldsError builds a single ez.EINVALID error from fields, sorted by
// location (path, query, header, body) then name, with duplicates removed.
// The message joins "<name> <message>" with "; ", prefixing parameter names
// with their location. Returns nil when fields is empty.
func NewFieldsError(fields []FieldError) error {
	fields = normalize(fields)
	if len(fields) == 0 {
		return nil
	}

	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = displayName(f) + " " + f.Message
	}
	message := strings.Join(parts, "; ")

	return ez.New(ez.EINVALID, message, &fieldsError{fields: fields, message: message})
}

// Fields returns the structured field errors carried by err, or nil.
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
		names := strings.Compare(a.Name, b.Name)
		if names != 0 {
			return names
		}
		return strings.Compare(a.Message, b.Message)
	})
	return slices.Compact(out)
}

// collect walks the santhosh error tree and maps every leaf to FieldErrors.
// root is the compiled schema the instance was validated against; it tells
// declared property names apart from client-chosen map keys.
func collect(root *jsonschema.Schema, verr *jsonschema.ValidationError) []FieldError {
	switch verr.ErrorKind.(type) {
	case *kind.AnyOf, *kind.OneOf:
		return collectAlternatives(root, verr)
	}

	if len(verr.Causes) == 0 {
		return mapLeaf(root, verr)
	}

	var fields []FieldError
	for _, cause := range verr.Causes {
		fields = append(fields, collect(root, cause)...)
	}
	return fields
}

// collectAlternatives handles anyOf/oneOf. When every branch failed only on
// type at this location the wanted types are merged into one message
// ("must be string or null"). Otherwise branches that failed on type are
// dropped and the remaining, more specific errors are reported.
func collectAlternatives(root *jsonschema.Schema, verr *jsonschema.ValidationError) []FieldError {
	if len(verr.Causes) == 0 {
		return mapLeaf(root, verr)
	}

	var leaves []*jsonschema.ValidationError
	for _, cause := range verr.Causes {
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
		return []FieldError{newField(root, verr.InstanceLocation, "", "must be "+typeList(want))}
	}

	var fields []FieldError
	for _, leaf := range rest {
		fields = append(fields, collect(root, leaf)...)
	}
	return fields
}

func flatten(verr *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(verr.Causes) == 0 {
		return []*jsonschema.ValidationError{verr}
	}
	var leaves []*jsonschema.ValidationError
	for _, cause := range verr.Causes {
		leaves = append(leaves, flatten(cause)...)
	}
	return leaves
}

func mapLeaf(root *jsonschema.Schema, verr *jsonschema.ValidationError) []FieldError {
	loc := verr.InstanceLocation

	switch k := verr.ErrorKind.(type) {
	case *kind.Required:
		fields := make([]FieldError, len(k.Missing))
		for i, name := range k.Missing {
			fields[i] = newField(root, loc, name, "is required")
		}
		return fields
	case *kind.AdditionalProperties:
		fields := make([]FieldError, len(k.Properties))
		for i, name := range k.Properties {
			fields[i] = newField(root, loc, unknownName(name), "is not a known field")
		}
		return fields
	}

	return []FieldError{newField(root, loc, "", message(verr.ErrorKind))}
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
		return fmt.Sprintf("must be at least %d characters", k.Want)
	case *kind.MaxLength:
		return fmt.Sprintf("must be at most %d characters", k.Want)
	case *kind.MinItems:
		return fmt.Sprintf("must have at least %d items", k.Want)
	case *kind.MaxItems:
		return fmt.Sprintf("must have at most %d items", k.Want)
	case *kind.MinProperties:
		return fmt.Sprintf("must have at least %d properties", k.Want)
	case *kind.MaxProperties:
		return fmt.Sprintf("must have at most %d properties", k.Want)
	case *kind.UniqueItems:
		return "must not contain duplicates"
	case *kind.Pattern:
		return "must match the expected format"
	case *kind.Const:
		return "must equal the fixed value"
	}
	return "is invalid"
}

// newField splits an instance location into its request location and the
// dotted name inside it, appending child when set. child must come from the
// schema or be sanitised by the caller.
func newField(root *jsonschema.Schema, loc []string, child string, msg string) FieldError {
	segments := displayPath(root, loc)
	if child != "" {
		segments = append(segments, child)
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
// described by schemas, and whether segment may be shown verbatim.
func step(schemas []*jsonschema.Schema, segment string) ([]*jsonschema.Schema, bool) {
	var props, rest []*jsonschema.Schema
	n, err := strconv.Atoi(segment)
	index := err == nil && n >= 0 && strconv.Itoa(n) == segment

	for _, s := range schemas {
		prop, ok := s.Properties[segment]
		if ok {
			props = append(props, prop)
		}
		if index {
			rest = append(rest, itemSchema(s, n)...)
		}
		extra, ok := s.AdditionalProperties.(*jsonschema.Schema)
		if ok {
			rest = append(rest, extra)
		}
		for _, pattern := range s.PatternProperties {
			rest = append(rest, pattern)
		}
	}

	if len(props) > 0 {
		return props, true
	}
	return rest, index
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
