package pagination

import (
	"encoding/base64"
	"encoding/json"

	"github.com/vanclief/ez"
)

const (
	MAX_PAGE_SIZE = 1000
	DEFAULT_LIMIT = 50
)

// Cursor holds the values needed for pagination
type Cursor struct {
	SortField   string      `json:"sort_field"`
	SortValue   interface{} `json:"sort_value"`
	UniqueField string      `json:"unique_field"`
	UniqueValue interface{} `json:"unique_value"`
}

// EncodeCursor creates a base64 encoded cursor from a Cursor struct
func EncodeCursor(c Cursor) (string, error) {
	const op = "pagination.EncodeCursor"

	jsonBytes, err := json.Marshal(c)
	if err != nil {
		return "", ez.New(op, ez.EINTERNAL, "failed to marshal cursor", err)
	}

	return base64.URLEncoding.EncodeToString(jsonBytes), nil
}

// DecodeCursor decodes a base64 encoded cursor string into a Cursor struct
func DecodeCursor(encodedCursor string) (*Cursor, error) {
	const op = "pagination.DecodeCursor"

	if encodedCursor == "" {
		return nil, nil
	}

	jsonBytes, err := base64.URLEncoding.DecodeString(encodedCursor)
	if err != nil {
		return nil, ez.New(op, ez.EINVALID, "invalid cursor format", err)
	}

	var cursor Cursor
	err = json.Unmarshal(jsonBytes, &cursor)
	if err != nil {
		return nil, ez.New(op, ez.EINVALID, "invalid cursor data", err)
	}

	return &cursor, nil
}
