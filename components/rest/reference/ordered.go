package reference

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/vanclief/ez"
	"gopkg.in/yaml.v3"
)

// Ordered is a map that remembers the order keys appeared in the document.
// OpenAPI tooling such as Redoc renders properties, paths and responses in
// authoring order, and so do we.
type Ordered[T any] struct {
	Keys []string
	Map  map[string]T
}

// Get returns the value for key.
func (o *Ordered[T]) Get(key string) (T, bool) {
	if o == nil || o.Map == nil {
		var zero T
		return zero, false
	}
	v, ok := o.Map[key]
	return v, ok
}

// Set appends key when new and stores value.
func (o *Ordered[T]) Set(key string, value T) {
	if o.Map == nil {
		o.Map = map[string]T{}
	}
	_, exists := o.Map[key]
	if !exists {
		o.Keys = append(o.Keys, key)
	}
	o.Map[key] = value
}

// Len returns the number of entries.
func (o *Ordered[T]) Len() int {
	if o == nil {
		return 0
	}
	return len(o.Keys)
}

// UnmarshalYAML keeps mapping order. Merge keys (<<: *anchor) are expanded in
// place; keys written in the mapping itself win over merged ones, and earlier
// merged mappings win over later ones, as the YAML merge spec says.
func (o *Ordered[T]) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return ez.New(ez.EINVALID, fmt.Sprintf("line %d: expected a mapping", node.Line), nil)
	}

	o.Keys = nil
	o.Map = map[string]T{}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].ShortTag() == "!!merge" {
			err := o.merge(node.Content[i+1])
			if err != nil {
				return ez.Wrap(err)
			}
			continue
		}

		var key string
		err := node.Content[i].Decode(&key)
		if err != nil {
			return ez.Wrap(err)
		}

		var value T
		err = node.Content[i+1].Decode(&value)
		if err != nil {
			return ez.Wrap(err)
		}

		o.Set(key, value)
	}

	return nil
}

// merge adds the entries of a merge key value (a mapping, an alias to one, or
// a sequence of those) that are not already set.
func (o *Ordered[T]) merge(node *yaml.Node) error {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}

	if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			err := o.merge(item)
			if err != nil {
				return ez.Wrap(err)
			}
		}
		return nil
	}

	var merged Ordered[T]
	err := merged.UnmarshalYAML(node)
	if err != nil {
		return ez.Wrap(err)
	}
	for _, key := range merged.Keys {
		_, exists := o.Map[key]
		if !exists {
			o.Set(key, merged.Map[key])
		}
	}
	return nil
}

// UnmarshalJSON keeps object key order. A JSON null decodes as empty.
func (o *Ordered[T]) UnmarshalJSON(data []byte) error {
	o.Keys = nil
	o.Map = map[string]T{}

	if string(bytes.TrimSpace(data)) == "null" {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(data))

	tok, err := dec.Token()
	if err != nil {
		return ez.Wrap(err)
	}
	if tok != json.Delim('{') {
		return ez.New(ez.EINVALID, "expected a JSON object", nil)
	}

	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return ez.Wrap(err)
		}
		key, ok := tok.(string)
		if !ok {
			return ez.New(ez.EINVALID, "expected a JSON object key", nil)
		}

		var value T
		err = dec.Decode(&value)
		if err != nil {
			return ez.Wrap(err)
		}

		o.Set(key, value)
	}

	return nil
}

// MarshalJSON writes entries in order.
func (o Ordered[T]) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.Keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := json.Marshal(key)
		if err != nil {
			return nil, ez.Wrap(err)
		}
		v, err := json.Marshal(o.Map[key])
		if err != nil {
			return nil, ez.Wrap(err)
		}
		buf.Write(k)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
