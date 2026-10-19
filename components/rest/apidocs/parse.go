package apidocs

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/vanclief/ez"
	"gopkg.in/yaml.v3"
)

// Parse decodes an OpenAPI 3.x document from YAML or JSON and normalizes the
// 3.0/3.1 differences described on Schema.
func Parse(data []byte) (*Document, error) {
	doc := &Document{}

	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 {
		return nil, ez.New(ez.EINVALID, "empty OpenAPI document", nil)
	}

	var err error
	if trimmed[0] == '{' {
		err = json.Unmarshal(trimmed, doc)
	} else {
		err = yaml.Unmarshal(trimmed, doc)
	}
	if err != nil {
		return nil, ez.New(ez.EINVALID, "invalid OpenAPI document", err)
	}

	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		return nil, ez.New(ez.EINVALID, "unsupported OpenAPI version "+strconvQuote(doc.OpenAPI)+", expected 3.x", nil)
	}

	normalizeDocument(doc)

	return doc, nil
}

func strconvQuote(s string) string {
	return `"` + s + `"`
}

// normalizeDocument walks every schema once and rewrites 3.1 forms into the
// single shape the model promises: "null" moves from the type list into
// Nullable, and 3.0 boolean exclusive bounds become numeric bounds.
func normalizeDocument(doc *Document) {
	seen := map[*Schema]bool{}

	for _, key := range doc.Components.Schemas.Keys {
		normalizeSchema(doc.Components.Schemas.Map[key], seen)
	}
	for _, key := range doc.Components.Parameters.Keys {
		normalizeSchema(doc.Components.Parameters.Map[key].Schema, seen)
	}
	for _, key := range doc.Components.Headers.Keys {
		normalizeSchema(doc.Components.Headers.Map[key].Schema, seen)
	}
	for _, key := range doc.Components.RequestBodies.Keys {
		normalizeContent(doc.Components.RequestBodies.Map[key].Content, seen)
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
			if m.Operation.RequestBody != nil {
				normalizeContent(m.Operation.RequestBody.Content, seen)
			}
			for _, status := range m.Operation.Responses.Keys {
				normalizeResponse(m.Operation.Responses.Map[status], seen)
			}
		}
	}
}

func normalizeParameters(params []*Parameter, seen map[*Schema]bool) {
	for _, p := range params {
		if p == nil {
			continue
		}
		normalizeSchema(p.Schema, seen)
		normalizeContent(p.Content, seen)
	}
}

func normalizeResponse(res *Response, seen map[*Schema]bool) {
	if res == nil {
		return
	}
	normalizeContent(res.Content, seen)
	for _, key := range res.Headers.Keys {
		normalizeSchema(res.Headers.Map[key].Schema, seen)
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
	for _, sub := range s.AllOf {
		normalizeSchema(sub, seen)
	}
	for _, sub := range s.OneOf {
		normalizeSchema(sub, seen)
	}
	for _, sub := range s.AnyOf {
		normalizeSchema(sub, seen)
	}
}
