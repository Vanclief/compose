package reference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/vanclief/ez"
	"gopkg.in/yaml.v3"
)

// Parse decodes an OpenAPI 3.x document from YAML or JSON and normalizes the
// 3.0/3.1 differences described on Schema. Every failure is an ez.EINVALID
// error whose message says what is wrong with the input.
func Parse(data []byte) (*Document, error) {
	doc := &Document{}

	// Leading whitespace only picks the format; decoding sees it so error
	// line numbers match the input.
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 {
		return nil, ez.New(ez.EINVALID, "empty OpenAPI document", nil)
	}
	isJSON := trimmed[0] == '{'

	var err error
	if isJSON {
		err = json.Unmarshal(data, doc)
	} else {
		err = decodeYAML(data, doc)
	}
	if err != nil {
		return nil, ez.New(ez.EINVALID, "invalid OpenAPI document: "+causeText(data, err), err)
	}

	if doc.OpenAPI == "" {
		return nil, ez.New(ez.EINVALID, missingVersionMessage(data, isJSON), nil)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		return nil, ez.New(ez.EINVALID, "unsupported OpenAPI version "+strconv.Quote(doc.OpenAPI)+", expected 3.x", nil)
	}

	normalizeDocument(doc)

	return doc, nil
}

// maxYAMLDepth is the nesting yaml.v3 allows when parsing a document.
const maxYAMLDepth = 10000

// maxYAMLAliasNodes is how many nodes alias expansion may add to a YAML
// document. yaml.v3 checks the expansion ratio per Decode call, but Ordered
// and Schema decode each of their nodes separately, so the check never sees
// a whole billion-laughs document; decodeYAML counts once instead. yaml.v3
// lets a single Decode expand up to 400,000 nodes freely, and a real spec
// uses anchors far less than that.
const maxYAMLAliasNodes = 1_000_000

// decodeYAML decodes data into doc after checking that expanding its aliases
// stays within maxYAMLAliasNodes. A document without aliases has fewer
// nodes than bytes, so the budget is the input length plus that allowance.
func decodeYAML(data []byte, doc *Document) error {
	var root yaml.Node
	err := yaml.Unmarshal(data, &root)
	if err != nil {
		return ez.Wrap(err)
	}

	budget := len(data) + maxYAMLAliasNodes
	if !withinAliasBudget(&root, 0, &budget) {
		return ez.New(ez.EINVALID, "YAML alias expansion too large", nil)
	}

	err = root.Decode(doc)
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// withinAliasBudget walks n with every alias followed and reports whether it
// holds at most *budget nodes, taking each node it visits from *budget. depth
// stops at maxYAMLDepth so an alias inside its own anchor, such as &a [*a],
// fails instead of recursing until the budget runs out.
func withinAliasBudget(n *yaml.Node, depth int, budget *int) bool {
	*budget--
	if *budget < 0 || depth > maxYAMLDepth {
		return false
	}
	if n.Kind == yaml.AliasNode {
		return n.Alias == nil || withinAliasBudget(n.Alias, depth+1, budget)
	}
	for _, child := range n.Content {
		if !withinAliasBudget(child, depth+1, budget) {
			return false
		}
	}
	return true
}

// causeText describes a decoding error by its innermost message, without the
// ez operation prefixes, and adds the line of a JSON syntax error.
func causeText(data []byte, err error) string {
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		line := 1 + bytes.Count(data[:min(int(syntax.Offset), len(data))], []byte("\n"))
		return fmt.Sprintf("line %d: %s", line, syntax.Error())
	}

	for {
		e, ok := err.(*ez.Error)
		if !ok {
			return err.Error()
		}
		if e.Err == nil {
			return e.Message
		}
		err = e.Err
	}
}

// missingVersionMessage explains a document without an openapi field, calling
// out Swagger 2.0 documents, which use a swagger field instead.
func missingVersionMessage(data []byte, isJSON bool) string {
	var probe struct {
		Swagger string `json:"swagger" yaml:"swagger"`
	}
	if isJSON {
		_ = json.Unmarshal(data, &probe)
	} else {
		_ = yaml.Unmarshal(data, &probe)
	}
	if probe.Swagger != "" {
		return "Swagger " + probe.Swagger + " documents are not supported, convert the document to OpenAPI 3.x"
	}
	return "missing openapi version field, expected an OpenAPI 3.x document"
}

// normalizeDocument walks every schema once and rewrites 3.1 forms into the
// single shape the model promises: "null" moves from the type list into
// Nullable, {type: "null"} members of oneOf and anyOf become Nullable on the
// parent, null members of allOf, oneOf and anyOf are dropped, and 3.0
// boolean exclusive bounds become numeric bounds.
func normalizeDocument(doc *Document) {
	seen := map[*Schema]bool{}

	for _, key := range doc.Components.Schemas.Keys {
		normalizeSchema(doc.Components.Schemas.Map[key], seen)
	}
	for _, key := range doc.Components.Parameters.Keys {
		normalizeParameter(doc.Components.Parameters.Map[key], seen)
	}
	for _, key := range doc.Components.Headers.Keys {
		normalizeParameter(doc.Components.Headers.Map[key], seen)
	}
	for _, key := range doc.Components.RequestBodies.Keys {
		normalizeRequestBody(doc.Components.RequestBodies.Map[key], seen)
	}
	for _, key := range doc.Components.Responses.Keys {
		normalizeResponse(doc.Components.Responses.Map[key], seen)
	}

	for _, key := range doc.Paths.Keys {
		item := doc.Paths.Map[key]
		if item == nil {
			continue
		}
		normalizeParameters(item.Parameters, seen)
		for _, m := range item.Methods() {
			normalizeParameters(m.Operation.Parameters, seen)
			normalizeRequestBody(m.Operation.RequestBody, seen)
			for _, status := range m.Operation.Responses.Keys {
				normalizeResponse(m.Operation.Responses.Map[status], seen)
			}
		}
	}
}

func normalizeParameters(params []*Parameter, seen map[*Schema]bool) {
	for _, p := range params {
		normalizeParameter(p, seen)
	}
}

func normalizeParameter(p *Parameter, seen map[*Schema]bool) {
	if p == nil {
		return
	}
	normalizeSchema(p.Schema, seen)
	normalizeContent(p.Content, seen)
}

func normalizeRequestBody(b *RequestBody, seen map[*Schema]bool) {
	if b == nil {
		return
	}
	normalizeContent(b.Content, seen)
}

func normalizeResponse(res *Response, seen map[*Schema]bool) {
	if res == nil {
		return
	}
	normalizeContent(res.Content, seen)
	for _, key := range res.Headers.Keys {
		normalizeParameter(res.Headers.Map[key], seen)
	}
}

func normalizeContent(content Ordered[*MediaType], seen map[*Schema]bool) {
	for _, key := range content.Keys {
		mt := content.Map[key]
		if mt != nil {
			normalizeSchema(mt.Schema, seen)
		}
	}
}

func normalizeSchema(s *Schema, seen map[*Schema]bool) {
	if s == nil || seen[s] {
		return
	}
	seen[s] = true

	if s.Type.Has("null") {
		s.Nullable = true
		var types TypeList
		for _, t := range s.Type {
			if t != "null" {
				types = append(types, t)
			}
		}
		s.Type = types
	}

	if s.ExclusiveMinimum != nil && s.ExclusiveMinimum.Value == nil {
		if s.ExclusiveMinimum.Flag && s.Minimum != nil {
			s.ExclusiveMinimum = &Bound{Value: s.Minimum}
			s.Minimum = nil
		} else {
			s.ExclusiveMinimum = nil
		}
	}
	if s.ExclusiveMaximum != nil && s.ExclusiveMaximum.Value == nil {
		if s.ExclusiveMaximum.Flag && s.Maximum != nil {
			s.ExclusiveMaximum = &Bound{Value: s.Maximum}
			s.Maximum = nil
		} else {
			s.ExclusiveMaximum = nil
		}
	}

	for _, key := range s.Properties.Keys {
		normalizeSchema(s.Properties.Map[key], seen)
	}
	normalizeSchema(s.Items, seen)
	normalizeSchema(s.Not, seen)
	normalizeSchema(s.AdditionalSchema(), seen)
	s.AllOf = withoutNil(s.AllOf)
	s.OneOf = withoutNil(s.OneOf)
	s.AnyOf = withoutNil(s.AnyOf)
	for _, sub := range s.AllOf {
		normalizeSchema(sub, seen)
	}
	for _, sub := range s.OneOf {
		normalizeSchema(sub, seen)
	}
	for _, sub := range s.AnyOf {
		normalizeSchema(sub, seen)
	}

	normalizeNullMembers(s)
}

// normalizeNullMembers rewrites the 3.1 nullable idiom
// anyOf: [{$ref: Pet}, {type: "null"}] (or oneOf) into the 3.0 shape the
// renderer already understands. Members that only say "null" are removed and
// the parent becomes Nullable. When one member remains and the parent has
// nothing else that would conflict, a bare $ref member becomes the parent's
// Ref and any other member becomes a single-member allOf, so the schema reads
// as Pet, nullable, rather than as two variants.
func normalizeNullMembers(s *Schema) {
	oneOf, oneNull := withoutNullMembers(s.OneOf)
	anyOf, anyNull := withoutNullMembers(s.AnyOf)
	if !oneNull && !anyNull {
		return
	}
	s.Nullable = true
	s.OneOf = oneOf
	s.AnyOf = anyOf

	members := append(append([]*Schema(nil), oneOf...), anyOf...)
	if len(members) != 1 || len(s.Type) > 0 || s.Properties.Len() > 0 || s.Items != nil || s.Ref != "" || len(s.AllOf) > 0 {
		return
	}
	s.OneOf = nil
	s.AnyOf = nil

	member := members[0]
	if member.Ref != "" && reflect.DeepEqual(*member, Schema{Ref: member.Ref}) {
		s.Ref = member.Ref
		return
	}
	s.AllOf = []*Schema{member}
}

// withoutNullMembers returns members without the ones that only allow null,
// and whether any were removed. The input slice is never modified.
func withoutNullMembers(members []*Schema) ([]*Schema, bool) {
	var out []*Schema
	removed := false
	for _, m := range members {
		if m != nil && reflect.DeepEqual(*m, Schema{Nullable: true}) {
			removed = true
			continue
		}
		out = append(out, m)
	}
	if !removed {
		return members, false
	}
	return out, true
}

// withoutNil returns members without nil entries, which a document holds for
// a null list item such as oneOf: [null, {$ref: Pet}], so later passes never
// see them. The input slice is returned as is when it has none.
func withoutNil(members []*Schema) []*Schema {
	if !slices.Contains(members, nil) {
		return members
	}
	var out []*Schema
	for _, m := range members {
		if m != nil {
			out = append(out, m)
		}
	}
	return out
}
