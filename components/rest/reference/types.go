// Package reference renders an OpenAPI 3.x document as a self-hosted API
// reference page: an alternative to Scalar and Redoc that ships inside the
// backend, with no CDN assets.
//
// The model below covers the subset of OpenAPI 3.0 and 3.1 that matters for
// documentation. Unknown fields are ignored, but for the schema keywords
// Schema.Unmodelled keeps. Values that differ between 3.0 and
// 3.1 (nullable versus "null" in the type list, exclusiveMinimum as a bool
// versus a number) are normalized by Parse so the renderer sees one shape.
package reference

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
	"gopkg.in/yaml.v3"
)

// Document is a parsed OpenAPI document.
type Document struct {
	OpenAPI    string                `json:"openapi" yaml:"openapi"`
	Info       Info                  `json:"info" yaml:"info"`
	Servers    []Server              `json:"servers" yaml:"servers"`
	Paths      Ordered[*PathItem]    `json:"paths" yaml:"paths"`
	Components Components            `json:"components" yaml:"components"`
	Security   []map[string][]string `json:"security" yaml:"security"`
	Tags       []Tag                 `json:"tags" yaml:"tags"`
}

// Info is the document header.
type Info struct {
	Title          string   `json:"title" yaml:"title"`
	Version        string   `json:"version" yaml:"version"`
	Summary        string   `json:"summary" yaml:"summary"`
	Description    string   `json:"description" yaml:"description"`
	TermsOfService string   `json:"termsOfService" yaml:"termsOfService"`
	Contact        *Contact `json:"contact" yaml:"contact"`
	License        *License `json:"license" yaml:"license"`
}

// Contact is info.contact.
type Contact struct {
	Name  string `json:"name" yaml:"name"`
	URL   string `json:"url" yaml:"url"`
	Email string `json:"email" yaml:"email"`
}

// License is info.license.
type License struct {
	Name string `json:"name" yaml:"name"`
	URL  string `json:"url" yaml:"url"`
}

// Server is one entry of servers.
type Server struct {
	URL         string                    `json:"url" yaml:"url"`
	Description string                    `json:"description" yaml:"description"`
	Variables   map[string]ServerVariable `json:"variables" yaml:"variables"`
}

// ServerVariable is one entry of server.variables.
type ServerVariable struct {
	Default     string   `json:"default" yaml:"default"`
	Enum        []string `json:"enum" yaml:"enum"`
	Description string   `json:"description" yaml:"description"`
}

// Expand returns the server URL with every {variable} replaced by its default.
func (s Server) Expand() string {
	out := s.URL
	for name, v := range s.Variables {
		out = strings.ReplaceAll(out, "{"+name+"}", v.Default)
	}
	return out
}

// Tag is one entry of tags. Operations are grouped under tags in the sidebar.
// Parent (OpenAPI 3.2) or, in older documents, the x-parent extension names
// the tag a child tag nests under; see Document.tagPath.
type Tag struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
	Parent      string `json:"parent" yaml:"parent"`
	XParent     string `json:"x-parent" yaml:"x-parent"`
}

// Components holds the reusable objects $ref points at.
type Components struct {
	Schemas         Ordered[*Schema]         `json:"schemas" yaml:"schemas"`
	Responses       Ordered[*Response]       `json:"responses" yaml:"responses"`
	Parameters      Ordered[*Parameter]      `json:"parameters" yaml:"parameters"`
	Examples        Ordered[*Example]        `json:"examples" yaml:"examples"`
	RequestBodies   Ordered[*RequestBody]    `json:"requestBodies" yaml:"requestBodies"`
	Headers         Ordered[*Header]         `json:"headers" yaml:"headers"`
	SecuritySchemes Ordered[*SecurityScheme] `json:"securitySchemes" yaml:"securitySchemes"`
}

// PathItem is one entry of paths.
type PathItem struct {
	Ref         string       `json:"$ref" yaml:"$ref"`
	Summary     string       `json:"summary" yaml:"summary"`
	Description string       `json:"description" yaml:"description"`
	Get         *Operation   `json:"get" yaml:"get"`
	Put         *Operation   `json:"put" yaml:"put"`
	Post        *Operation   `json:"post" yaml:"post"`
	Delete      *Operation   `json:"delete" yaml:"delete"`
	Options     *Operation   `json:"options" yaml:"options"`
	Head        *Operation   `json:"head" yaml:"head"`
	Patch       *Operation   `json:"patch" yaml:"patch"`
	Trace       *Operation   `json:"trace" yaml:"trace"`
	Parameters  []*Parameter `json:"parameters" yaml:"parameters"`
}

// Methods lists the operations of the path item in the canonical HTTP order.
func (p *PathItem) Methods() []MethodOperation {
	all := []MethodOperation{
		{"GET", p.Get}, {"POST", p.Post}, {"PUT", p.Put}, {"PATCH", p.Patch},
		{"DELETE", p.Delete}, {"HEAD", p.Head}, {"OPTIONS", p.Options}, {"TRACE", p.Trace},
	}
	var out []MethodOperation
	for _, m := range all {
		if m.Operation != nil {
			out = append(out, m)
		}
	}
	return out
}

// MethodOperation pairs an HTTP method with its operation.
type MethodOperation struct {
	Method    string
	Operation *Operation
}

// Operation is one HTTP method of a path.
type Operation struct {
	Tags        []string           `json:"tags" yaml:"tags"`
	Summary     string             `json:"summary" yaml:"summary"`
	Description string             `json:"description" yaml:"description"`
	OperationID string             `json:"operationId" yaml:"operationId"`
	Parameters  []*Parameter       `json:"parameters" yaml:"parameters"`
	RequestBody *RequestBody       `json:"requestBody" yaml:"requestBody"`
	Responses   Ordered[*Response] `json:"responses" yaml:"responses"`
	Deprecated  bool               `json:"deprecated" yaml:"deprecated"`
	// Internal is the x-internal extension: the operation is documented for
	// the team, not for API consumers.
	Internal bool `json:"x-internal" yaml:"x-internal"`
	// Destructive is the x-destructive extension: a non-DELETE operation
	// whose effect destroys data irreversibly, such as configuring a policy
	// that triggers deletion. It is meant for write operations: a GET or
	// HEAD operation stays read-only whether or not it carries the mark.
	Destructive bool     `json:"x-destructive" yaml:"x-destructive"`
	Servers     []Server `json:"servers" yaml:"servers"`
	// Security is nil when the operation inherits the document security, and
	// an empty slice when the operation is explicitly public.
	Security *[]map[string][]string `json:"security" yaml:"security"`
}

// Parameter is a path, query, header or cookie parameter.
type Parameter struct {
	Ref         string              `json:"$ref" yaml:"$ref"`
	Name        string              `json:"name" yaml:"name"`
	In          string              `json:"in" yaml:"in"`
	Description string              `json:"description" yaml:"description"`
	Required    bool                `json:"required" yaml:"required"`
	Deprecated  bool                `json:"deprecated" yaml:"deprecated"`
	Style       string              `json:"style" yaml:"style"`
	Explode     *bool               `json:"explode" yaml:"explode"`
	Schema      *Schema             `json:"schema" yaml:"schema"`
	Example     any                 `json:"example" yaml:"example"`
	Examples    Ordered[*Example]   `json:"examples" yaml:"examples"`
	Content     Ordered[*MediaType] `json:"content" yaml:"content"`
}

// Header is a response header; the same shape as Parameter without name and in.
type Header = Parameter

// RequestBody is operation.requestBody.
type RequestBody struct {
	Ref         string              `json:"$ref" yaml:"$ref"`
	Description string              `json:"description" yaml:"description"`
	Required    bool                `json:"required" yaml:"required"`
	Content     Ordered[*MediaType] `json:"content" yaml:"content"`
}

// MediaType is one content type of a body or response.
type MediaType struct {
	Schema   *Schema           `json:"schema" yaml:"schema"`
	Example  any               `json:"example" yaml:"example"`
	Examples Ordered[*Example] `json:"examples" yaml:"examples"`
}

// Example is a named example.
type Example struct {
	Ref           string `json:"$ref" yaml:"$ref"`
	Summary       string `json:"summary" yaml:"summary"`
	Description   string `json:"description" yaml:"description"`
	Value         any    `json:"value" yaml:"value"`
	ExternalValue string `json:"externalValue" yaml:"externalValue"`
}

// Response is one status code of operation.responses.
type Response struct {
	Ref         string              `json:"$ref" yaml:"$ref"`
	Description string              `json:"description" yaml:"description"`
	Headers     Ordered[*Header]    `json:"headers" yaml:"headers"`
	Content     Ordered[*MediaType] `json:"content" yaml:"content"`
}

// SecurityScheme is one entry of components.securitySchemes.
type SecurityScheme struct {
	Type         string `json:"type" yaml:"type"`
	Description  string `json:"description" yaml:"description"`
	Name         string `json:"name" yaml:"name"`
	In           string `json:"in" yaml:"in"`
	Scheme       string `json:"scheme" yaml:"scheme"`
	BearerFormat string `json:"bearerFormat" yaml:"bearerFormat"`
}

// Discriminator is schema.discriminator.
type Discriminator struct {
	PropertyName string            `json:"propertyName" yaml:"propertyName"`
	Mapping      map[string]string `json:"mapping" yaml:"mapping"`
}

// Schema is a JSON Schema as used by OpenAPI 3.0 and 3.1.
//
// Type always holds the non-null types; Nullable is true when 3.0 said
// nullable: true, when 3.1 listed "null" in the type array, or when a oneOf
// or anyOf member was {type: "null"}. NullMember is true in the last case
// only: null is then valid whatever the enum or const say, while the other
// two add null to the type and leave the enum and const deciding. NullOnly
// is true when 3.1 listed "null" as the only type, or when the oneOf and
// anyOf members were all {type: "null"} beside no type, $ref or allOf,
// which leaves Type empty as for a schema that states no type. ConstNull is
// true when Parse read const: null, which leaves Const nil as for no const.
// Exclusive bounds always hold the numeric form.
type Schema struct {
	Ref           string           `json:"$ref" yaml:"$ref"`
	Title         string           `json:"title" yaml:"title"`
	Description   string           `json:"description" yaml:"description"`
	Type          TypeList         `json:"type" yaml:"type"`
	Format        string           `json:"format" yaml:"format"`
	Nullable      bool             `json:"nullable" yaml:"nullable"`
	NullMember    bool             `json:"-" yaml:"-"`
	NullOnly      bool             `json:"-" yaml:"-"`
	Enum          []any            `json:"enum" yaml:"enum"`
	Const         any              `json:"const" yaml:"const"`
	ConstNull     bool             `json:"-" yaml:"-"`
	Default       any              `json:"default" yaml:"default"`
	Example       any              `json:"example" yaml:"example"`
	Examples      []any            `json:"examples" yaml:"examples"`
	Deprecated    bool             `json:"deprecated" yaml:"deprecated"`
	ReadOnly      bool             `json:"readOnly" yaml:"readOnly"`
	WriteOnly     bool             `json:"writeOnly" yaml:"writeOnly"`
	Properties    Ordered[*Schema] `json:"properties" yaml:"properties"`
	Required      []string         `json:"required" yaml:"required"`
	Items         *Schema          `json:"items" yaml:"items"`
	AllOf         []*Schema        `json:"allOf" yaml:"allOf"`
	OneOf         []*Schema        `json:"oneOf" yaml:"oneOf"`
	AnyOf         []*Schema        `json:"anyOf" yaml:"anyOf"`
	Not           *Schema          `json:"not" yaml:"not"`
	Discriminator *Discriminator   `json:"discriminator" yaml:"discriminator"`

	// AdditionalProperties is nil when unset, which JSON Schema reads as
	// allowed. Use AdditionalSchema to get the schema of extra properties.
	AdditionalProperties *AdditionalProperties `json:"additionalProperties" yaml:"additionalProperties"`

	Minimum          *float64 `json:"minimum" yaml:"minimum"`
	Maximum          *float64 `json:"maximum" yaml:"maximum"`
	ExclusiveMinimum *Bound   `json:"exclusiveMinimum" yaml:"exclusiveMinimum"`
	ExclusiveMaximum *Bound   `json:"exclusiveMaximum" yaml:"exclusiveMaximum"`
	MultipleOf       *float64 `json:"multipleOf" yaml:"multipleOf"`
	MinLength        *int     `json:"minLength" yaml:"minLength"`
	MaxLength        *int     `json:"maxLength" yaml:"maxLength"`
	Pattern          string   `json:"pattern" yaml:"pattern"`
	MinItems         *int     `json:"minItems" yaml:"minItems"`
	MaxItems         *int     `json:"maxItems" yaml:"maxItems"`
	UniqueItems      bool     `json:"uniqueItems" yaml:"uniqueItems"`
	MinProperties    *int     `json:"minProperties" yaml:"minProperties"`
	MaxProperties    *int     `json:"maxProperties" yaml:"maxProperties"`

	// Extensions holds the specification extensions (keys starting with
	// "x-") as decoded values, or nil when the schema has none. It is not
	// written back by json.Marshal.
	Extensions map[string]any `json:"-" yaml:"-"`

	// Unmodelled holds the keywords of the schema that no other field holds
	// and that are neither extensions nor annotationKeywords, such as
	// patternProperties, if or prefixItems, sorted by name, or nil when the
	// schema has none. Diff compares them by value only, see Diff. It is not
	// written back by json.Marshal.
	Unmodelled []RawKeyword `json:"-" yaml:"-"`
}

// RawKeyword is a schema keyword Parse does not model: its name and its
// value as compact JSON with object keys sorted, so two values that differ
// only in key order, spacing or the spelling of a number of at most 15
// significant digits within the normal float64 range are the same text; any
// other number is written as in the document, or as its value where YAML
// spells it in a form JSON has not. A YAML mapping key
// is its text as written, and a YAML value JSON cannot hold, such as .nan,
// is written as a string.
type RawKeyword struct {
	Name  string
	Value string
}

// annotationKeywords are the schema keywords Parse drops, as it drops
// extensions, though no Schema field holds them: they describe or identify
// a schema and do not change the values it accepts.
var annotationKeywords = []string{"$comment", "$id", "$schema", "$anchor", "externalDocs", "xml", "patternDescription"}

// fieldKeys holds the key of each Schema field Parse decodes.
var fieldKeys = func() []string {
	var out []string
	t := reflect.TypeOf(Schema{})
	for i := range t.NumField() {
		key := t.Field(i).Tag.Get("json")
		if key != "-" {
			out = append(out, key)
		}
	}
	return out
}()

// unmodelled reports whether Parse keeps key, of a YAML schema mapping, in
// Unmodelled: it is not the key of a Schema field, an extension or one of
// the annotationKeywords.
func unmodelled(key string) bool {
	return !slices.Contains(fieldKeys, key) && !slices.Contains(annotationKeywords, key) && !strings.HasPrefix(key, "x-")
}

// unmodelledJSON is unmodelled for a key of a JSON schema object, which
// encoding/json decodes into the field whose key it matches regardless of
// case, as maxlength into MaxLength.
func unmodelledJSON(key string) bool {
	return unmodelled(key) && !slices.ContainsFunc(fieldKeys, func(field string) bool {
		return strings.EqualFold(field, key)
	})
}

// jsonKeyword is the keyword name with the JSON value, see RawKeyword.
func jsonKeyword(name string, value json.RawMessage) (RawKeyword, error) {
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	if err != nil {
		return RawKeyword{}, ez.Wrap(err)
	}
	var buf bytes.Buffer
	appendJSON(&buf, comparedNumbers(v))
	return RawKeyword{Name: name, Value: buf.String()}, nil
}

// comparedNumbers replaces, in place, each json.Number in v, decoded with
// UseNumber, by comparedNumber of its text, and returns v.
func comparedNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		return comparedNumber(string(t))
	case map[string]any:
		for key, item := range t {
			t[key] = comparedNumbers(item)
		}
	case []any:
		for i, item := range t {
			t[i] = comparedNumbers(item)
		}
	}
	return v
}

// minNormal is the smallest positive float64 with full precision; below it
// two decimals of 15 significant digits can round to the same float64.
const minNormal = 0x1p-1022

// comparedNumber is the number written as text, in a value Unmodelled
// keeps, as RawKeyword compares it: a float64 where text has at most 15
// significant digits and lies in the normal float64 range, where no two such
// decimals give the same float64, so 1, 1.0 and 1e0 are one value; otherwise
// the text as written, a json.Number, which appendScalar writes as a string
// where it is not a JSON number.
func comparedNumber(text string) any {
	mantissa := text
	i := strings.IndexAny(text, "eE")
	if i >= 0 {
		mantissa = text[:i]
	}
	digits := strings.Replace(strings.TrimLeft(mantissa, "+-"), ".", "", 1)
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return json.Number(text)
	}
	significant := strings.Trim(digits, "0")
	if len(significant) > 15 {
		return json.Number(text)
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || (significant != "" && math.Abs(f) < minNormal) {
		return json.Number(text)
	}
	if f == 0 {
		return 0.0
	}
	return f
}

// yamlKeyword is the keyword name with the YAML value node, see
// RawKeyword. The node is written as it stands rather than decoded, so a
// value yaml.v3 refuses to decode, as with a key given twice or more
// aliases than its ratio allows, is kept too: aliases are followed, under
// Parse within the node and text budgets decodeYAML sets for the whole
// document, and anywhere until the value is written longer than
// maxYAMLAliasText, which is an error, as is an alias inside its own
// anchor; a mapping's keys are written as their text, in order of that
// text, keys of the same text in document order, and a merge key as a key
// like any other, while a key that is not a scalar, which JSON cannot
// hold, is an error; numbers are written by comparedNumber.
func yamlKeyword(name string, node *yaml.Node) (RawKeyword, error) {
	var buf bytes.Buffer
	err := appendNode(&buf, name, node, map[*yaml.Node]bool{})
	if err != nil {
		return RawKeyword{}, ez.Wrap(err)
	}
	return RawKeyword{Name: name, Value: buf.String()}, nil
}

// appendNode writes node, in the value of the keyword name, as compact
// JSON, see yamlKeyword. visiting holds the anchors being written, so an
// alias to one of them is refused. buf holds that value alone, and once it
// is longer than maxYAMLAliasText after a node is written the expansion
// stops with an error, so a billion-laughs value that yaml.v3's own alias
// check never sees, as appendNode follows the aliases itself, cannot
// exhaust memory where a Schema is decoded from YAML outside Parse. Under
// Parse, whose budgets decodeYAML checks first, it refuses a document only
// where the value of one such keyword alone is written longer than that.
func appendNode(buf *bytes.Buffer, name string, node *yaml.Node, visiting map[*yaml.Node]bool) error {
	if node != nil && node.Kind == yaml.AliasNode {
		target := node.Alias
		if visiting[target] {
			return ez.New(ez.EINVALID, fmt.Sprintf("line %d: anchor %q contains itself", node.Line, node.Value), nil)
		}
		visiting[target] = true
		err := appendNode(buf, name, target, visiting)
		delete(visiting, target)
		if err != nil {
			return ez.Wrap(err)
		}
		return nil
	}
	if node == nil {
		buf.WriteString("null")
		return nil
	}
	switch node.Kind {
	case yaml.SequenceNode:
		buf.WriteByte('[')
		for i, item := range node.Content {
			if i > 0 {
				buf.WriteByte(',')
			}
			err := appendNode(buf, name, item, visiting)
			if err != nil {
				return ez.Wrap(err)
			}
		}
		buf.WriteByte(']')
	case yaml.MappingNode:
		type pair struct {
			key   string
			value *yaml.Node
		}
		pairs := make([]pair, 0, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := aliasTarget(node.Content[i])
			if key.Kind != yaml.ScalarNode {
				return ez.New(ez.EINVALID, fmt.Sprintf("line %d: schema keyword %s has a key that is not a scalar, which JSON cannot hold", node.Content[i].Line, name), nil)
			}
			pairs = append(pairs, pair{key: key.Value, value: node.Content[i+1]})
		}
		slices.SortStableFunc(pairs, func(a, b pair) int {
			return strings.Compare(a.key, b.key)
		})
		buf.WriteByte('{')
		for i, p := range pairs {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendScalar(buf, p.key)
			buf.WriteByte(':')
			err := appendNode(buf, name, p.value, visiting)
			if err != nil {
				return ez.Wrap(err)
			}
		}
		buf.WriteByte('}')
	case yaml.ScalarNode:
		appendYAMLScalar(buf, node)
	default:
		buf.WriteString("null")
	}
	if buf.Len() > maxYAMLAliasText {
		return ez.New(ez.EINVALID, fmt.Sprintf("schema keyword %s is longer than %d bytes written as JSON with its YAML aliases expanded", name, maxYAMLAliasText), nil)
	}
	return nil
}

// appendYAMLScalar writes the scalar node as yaml.v3 decodes it, a float
// by writeFloat. A string, and a null, bool, int or float with no explicit
// tag, is written from its resolved tag and text, and !!binary, whose bytes
// JSON cannot hold, as its text; any other scalar goes through Decode.
func appendYAMLScalar(buf *bytes.Buffer, node *yaml.Node) {
	tag := node.ShortTag()
	if tag == "!!str" || tag == "!!binary" {
		appendScalar(buf, node.Value)
		return
	}
	if node.Style&yaml.TaggedStyle == 0 {
		switch tag {
		case "!!null":
			buf.WriteString("null")
			return
		case "!!bool":
			buf.WriteString(strconv.FormatBool(strings.EqualFold(node.Value, "true")))
			return
		case "!!int":
			n, err := strconv.ParseInt(strings.ReplaceAll(node.Value, "_", ""), 0, 64)
			if err == nil {
				buf.WriteString(strconv.FormatInt(n, 10))
				return
			}
		case "!!float":
			f, err := strconv.ParseFloat(strings.ReplaceAll(node.Value, "_", ""), 64)
			if err == nil {
				writeFloat(buf, f, node.Value)
				return
			}
		}
	}
	var v any
	err := node.Decode(&v)
	if err != nil {
		// A scalar its tag does not fit, such as !!int abc: its text is
		// all there is to compare.
		appendScalar(buf, node.Value)
		return
	}
	f, ok := v.(float64)
	if ok {
		writeFloat(buf, f, node.Value)
		return
	}
	appendScalar(buf, v)
}

// writeFloat writes the YAML float f, written text: by comparedNumber where
// f is finite and text, without the underscores YAML allows between digits,
// is a JSON number or comparedNumber reads it, and as f otherwise, a
// negative zero as 0 and NaN and the infinities as strings.
func writeFloat(buf *bytes.Buffer, f float64, text string) {
	if f == 0 {
		f = 0
	}
	if math.IsInf(f, 0) || math.IsNaN(f) {
		appendScalar(buf, f)
		return
	}
	v := comparedNumber(strings.ReplaceAll(text, "_", ""))
	number, ok := v.(json.Number)
	if ok && !json.Valid([]byte(number)) {
		v = f
	}
	appendScalar(buf, v)
}

// aliasTarget is the node the alias node points at, or node itself where it
// is no alias.
func aliasTarget(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return node.Alias
	}
	return node
}

// appendMerged appends to out the keywords Unmodelled keeps that the merge
// key values merges bring into a mapping, each a mapping, an alias to one
// or a sequence of those. As YAML merge defines, a key in seen, which the
// mapping or an earlier merged mapping holds, is skipped, and the keys of a
// merged mapping win over those its own merge keys bring in.
func appendMerged(out []RawKeyword, merges []*yaml.Node, seen map[string]bool) ([]RawKeyword, error) {
	for _, merge := range merges {
		merge = aliasTarget(merge)
		mappings := []*yaml.Node{merge}
		if merge.Kind == yaml.SequenceNode {
			mappings = merge.Content
		}
		for _, m := range mappings {
			m = aliasTarget(m)
			if m.Kind != yaml.MappingNode {
				continue
			}
			var nested []*yaml.Node
			for i := 0; i+1 < len(m.Content); i += 2 {
				if m.Content[i].ShortTag() == "!!merge" {
					nested = append(nested, m.Content[i+1])
					continue
				}
				key := aliasTarget(m.Content[i]).Value
				if seen[key] {
					continue
				}
				seen[key] = true
				if !unmodelled(key) {
					continue
				}
				keyword, err := yamlKeyword(key, m.Content[i+1])
				if err != nil {
					return nil, ez.Wrap(err)
				}
				out = append(out, keyword)
			}
			var err error
			out, err = appendMerged(out, nested, seen)
			if err != nil {
				return nil, ez.Wrap(err)
			}
		}
	}
	return out, nil
}

// sortKeywords sorts keywords by name, then by value.
func sortKeywords(keywords []RawKeyword) {
	slices.SortFunc(keywords, func(a, b RawKeyword) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Value, b.Value))
	})
}

// schemaFields has the fields of Schema without its methods, so the custom
// unmarshalers can decode the object form without recursing into themselves.
type schemaFields Schema

// UnmarshalJSON accepts a schema object or a 3.1 boolean schema: true is the
// empty schema (anything) and false is {"not": {}} (nothing).
func (s *Schema) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if string(trimmed) == "true" || string(trimmed) == "false" {
		*s = booleanSchema(string(trimmed) == "true")
		return nil
	}
	err := json.Unmarshal(data, (*schemaFields)(s))
	if err != nil {
		return ez.Wrap(err)
	}
	var raw map[string]json.RawMessage
	err = json.Unmarshal(data, &raw)
	if err != nil {
		return ez.Wrap(err)
	}
	s.ConstNull = string(bytes.TrimSpace(raw["const"])) == "null"
	for key, value := range raw {
		if unmodelledJSON(key) {
			keyword, err := jsonKeyword(key, value)
			if err != nil {
				return ez.Wrap(err)
			}
			s.Unmodelled = append(s.Unmodelled, keyword)
			continue
		}
		if !strings.HasPrefix(key, "x-") {
			continue
		}
		var v any
		err = json.Unmarshal(value, &v)
		if err != nil {
			return ez.Wrap(err)
		}
		if s.Extensions == nil {
			s.Extensions = map[string]any{}
		}
		s.Extensions[key] = v
	}
	sortKeywords(s.Unmodelled)
	return nil
}

// UnmarshalYAML accepts a schema mapping or a 3.1 boolean schema. It
// refuses a NaN, which YAML writes .nan, as the value of minimum, maximum,
// exclusiveMinimum, exclusiveMaximum or multipleOf: no number compares with
// it, so it bounds nothing and would hide the bound of another allOf member
// where Flatten keeps the tightest. JSON has no NaN.
func (s *Schema) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!bool" {
		var allowed bool
		err := node.Decode(&allowed)
		if err != nil {
			return ez.Wrap(err)
		}
		*s = booleanSchema(allowed)
		return nil
	}
	err := node.Decode((*schemaFields)(s))
	if err != nil {
		return ez.Wrap(err)
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	err = refuseNaN(s, node)
	if err != nil {
		return ez.Wrap(err)
	}
	// A merge key may bring in keywords Unmodelled keeps.
	var merges []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := aliasTarget(node.Content[i]).Value
		if key == "const" {
			s.ConstNull = s.Const == nil
		}
		if node.Content[i].ShortTag() == "!!merge" {
			merges = append(merges, node.Content[i+1])
			continue
		}
		if unmodelled(key) {
			keyword, err := yamlKeyword(key, node.Content[i+1])
			if err != nil {
				return ez.Wrap(err)
			}
			s.Unmodelled = append(s.Unmodelled, keyword)
			continue
		}
		if !strings.HasPrefix(key, "x-") {
			continue
		}
		var v any
		err = node.Content[i+1].Decode(&v)
		if err != nil {
			return ez.Wrap(err)
		}
		if s.Extensions == nil {
			s.Extensions = map[string]any{}
		}
		s.Extensions[key] = v
	}
	if len(merges) > 0 {
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			seen[aliasTarget(node.Content[i]).Value] = true
		}
		s.Unmodelled, err = appendMerged(s.Unmodelled, merges, seen)
		if err != nil {
			return ez.Wrap(err)
		}
	}
	sortKeywords(s.Unmodelled)
	return nil
}

// refuseNaN returns an error when s, decoded from the mapping node, holds a
// NaN in one of the numeric keywords, naming the line of the keyword in the
// mapping, or, where a merge key brought the keyword in, of the mapping:
// the line of its first key, or of its opening brace in flow style.
func refuseNaN(s *Schema, node *yaml.Node) error {
	for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
		number := numericKeyword(s, key)
		if number == nil || !math.IsNaN(*number) {
			continue
		}
		line := node.Line
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				line = node.Content[i].Line
			}
		}
		return ez.New(ez.EINVALID, fmt.Sprintf("line %d: %s must be a number, not NaN", line, key), nil)
	}
	return nil
}

// numericKeyword returns the number s holds for the keyword key, one of
// minimum, maximum, exclusiveMinimum, exclusiveMaximum and multipleOf, or
// nil when key is another keyword or s holds none.
func numericKeyword(s *Schema, key string) *float64 {
	switch key {
	case "minimum":
		return s.Minimum
	case "maximum":
		return s.Maximum
	case "multipleOf":
		return s.MultipleOf
	case "exclusiveMinimum":
		if s.ExclusiveMinimum != nil {
			return s.ExclusiveMinimum.Value
		}
	case "exclusiveMaximum":
		if s.ExclusiveMaximum != nil {
			return s.ExclusiveMaximum.Value
		}
	}
	return nil
}

func booleanSchema(allowed bool) Schema {
	if allowed {
		return Schema{}
	}
	return Schema{Not: &Schema{}}
}

// AdditionalSchema returns the schema for additional properties, or nil when
// they are a free-form or forbidden.
func (s *Schema) AdditionalSchema() *Schema {
	if s.AdditionalProperties == nil {
		return nil
	}
	return s.AdditionalProperties.Schema
}

// IsRequired reports whether name is listed in required.
func (s *Schema) IsRequired(name string) bool {
	for _, r := range s.Required {
		if r == name {
			return true
		}
	}
	return false
}

// AdditionalProperties decodes either a boolean or a schema.
type AdditionalProperties struct {
	Allowed bool
	Schema  *Schema
}

// UnmarshalJSON accepts true, false or a schema object.
func (a *AdditionalProperties) UnmarshalJSON(data []byte) error {
	var allowed bool
	err := json.Unmarshal(data, &allowed)
	if err == nil {
		a.Allowed = allowed
		a.Schema = nil
		return nil
	}

	var schema Schema
	err = json.Unmarshal(data, &schema)
	if err != nil {
		return ez.Wrap(err)
	}
	a.Allowed = true
	a.Schema = &schema
	return nil
}

// UnmarshalYAML accepts true, false or a schema mapping.
func (a *AdditionalProperties) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var allowed bool
		err := node.Decode(&allowed)
		if err != nil {
			return ez.Wrap(err)
		}
		a.Allowed = allowed
		a.Schema = nil
		return nil
	}

	var schema Schema
	err := node.Decode(&schema)
	if err != nil {
		return ez.Wrap(err)
	}
	a.Allowed = true
	a.Schema = &schema
	return nil
}

// TypeList decodes "type" as either a string (3.0) or an array (3.1).
type TypeList []string

// UnmarshalJSON accepts a string or an array of strings. A JSON null leaves
// the list empty.
func (t *TypeList) UnmarshalJSON(data []byte) error {
	if string(bytes.TrimSpace(data)) == "null" {
		*t = nil
		return nil
	}

	var single string
	err := json.Unmarshal(data, &single)
	if err == nil {
		*t = TypeList{single}
		return nil
	}

	var many []string
	err = json.Unmarshal(data, &many)
	if err != nil {
		return ez.Wrap(err)
	}
	*t = TypeList(many)
	return nil
}

// UnmarshalYAML accepts a scalar or a sequence of scalars.
func (t *TypeList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		*t = TypeList{node.Value}
		return nil
	}

	var many []string
	err := node.Decode(&many)
	if err != nil {
		return ez.Wrap(err)
	}
	*t = TypeList(many)
	return nil
}

// Has reports whether the list contains name.
func (t TypeList) Has(name string) bool {
	for _, v := range t {
		if v == name {
			return true
		}
	}
	return false
}

// Primary returns the first non-null type, or "" when the schema is untyped.
func (t TypeList) Primary() string {
	for _, v := range t {
		if v != "null" {
			return v
		}
	}
	return ""
}

// Bound decodes exclusiveMinimum and exclusiveMaximum, which are booleans in
// 3.0 (qualifying minimum and maximum) and numbers in 3.1.
type Bound struct {
	Flag  bool
	Value *float64
}

// UnmarshalJSON accepts a bool or a number.
func (b *Bound) UnmarshalJSON(data []byte) error {
	var flag bool
	err := json.Unmarshal(data, &flag)
	if err == nil {
		b.Flag = flag
		b.Value = nil
		return nil
	}

	var value float64
	err = json.Unmarshal(data, &value)
	if err != nil {
		return ez.Wrap(err)
	}
	b.Flag = false
	b.Value = &value
	return nil
}

// UnmarshalYAML accepts a bool or a number.
func (b *Bound) UnmarshalYAML(node *yaml.Node) error {
	var flag bool
	err := node.Decode(&flag)
	if err == nil {
		b.Flag = flag
		b.Value = nil
		return nil
	}

	var value float64
	err = node.Decode(&value)
	if err != nil {
		return ez.New(ez.EINVALID, fmt.Sprintf("line %d: exclusive bound must be a bool or a number", node.Line), err)
	}
	b.Flag = false
	b.Value = &value
	return nil
}
