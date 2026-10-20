package validate

import (
	"bytes"
	"io"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/vanclief/ez"
)

// DecodeBody reads r and decodes it with DecodeBytes. Errors are ez.EINVALID
// and never echo the body.
func DecodeBody(r io.Reader) (any, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "body could not be read", err)
	}
	return DecodeBytes(raw)
}

// DecodeBytes decodes a JSON body into any, keeping numbers as json.Number
// and rejecting trailing data. An empty body returns nil, nil. Errors are
// ez.EINVALID and never echo the body.
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
