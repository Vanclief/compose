package validate

import (
	"bytes"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/vanclief/ez"
)

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
