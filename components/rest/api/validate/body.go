package validate

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/vanclief/ez"
)

// DecodeBytes decodes a JSON body into any, keeping numbers as json.Number
// and rejecting trailing data. An empty body returns nil, nil. Errors are
// ez.EINVALID and never echo the body. An object that repeats a key keeps
// only its last value (RejectRepeatedKeys refuses such a body).
func DecodeBytes(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}

	body, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, ez.New(ez.EINVALID, "body is not valid JSON", err)
	}

	return body, nil
}

// openObject is a JSON object RejectRepeatedKeys is inside: the keys seen in
// it, and whether its next token is a key.
type openObject struct {
	keys    map[string]bool
	wantKey bool
}

// RejectRepeatedKeys returns an ez.EINVALID error when an object of the JSON
// text raw repeats a key, at any depth and compared after unescaping.
// DecodeBytes, and so the validator, keep only the last value of a repeated
// key, while encoding/json decodes every one into the same struct, map or
// slice. The error never echoes the key. It holds one entry per open array
// and object, without recursion, and does not bound how deep they nest: the
// caller does.
func RejectRepeatedKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A number no float64 holds is not an error.
	dec.UseNumber()

	// The open arrays (nil) and objects, innermost last.
	var open []*openObject
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return ez.New(ez.EINVALID, "body is not valid JSON", err)
		}

		var parent *openObject
		if len(open) > 0 {
			parent = open[len(open)-1]
		}

		if parent != nil && parent.wantKey {
			key, ok := tok.(string)
			if !ok {
				// The object's closing brace.
				open = open[:len(open)-1]
				continue
			}
			if parent.keys[key] {
				return ez.New(ez.EINVALID, "body repeats an object key", nil)
			}
			parent.keys[key] = true
			parent.wantKey = false
			continue
		}

		// tok is the value of parent's last key, or opens it: a key or the
		// closing brace comes next.
		if parent != nil {
			parent.wantKey = true
		}
		switch tok {
		case json.Delim('{'):
			open = append(open, &openObject{keys: map[string]bool{}, wantKey: true})
		case json.Delim('['):
			open = append(open, nil)
		case json.Delim(']'):
			open = open[:len(open)-1]
		}
	}
}
