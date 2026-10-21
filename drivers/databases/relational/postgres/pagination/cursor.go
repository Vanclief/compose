package pagination

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync/atomic"

	"github.com/vanclief/ez"
)

const (
	MAX_PAGE_SIZE = 1000
	DEFAULT_LIMIT = 50
)

var signingKey atomic.Pointer[[]byte]

// Cursor holds the values needed for pagination
type Cursor struct {
	SortField   string      `json:"sort_field"`
	SortValue   interface{} `json:"sort_value"`
	UniqueField string      `json:"unique_field"`
	UniqueValue interface{} `json:"unique_value"`
}

// SetSigningKey enables HMAC-SHA256 signing of cursors so clients cannot forge
// pagination positions. Once a key is set, EncodeCursor appends a signature and
// DecodeCursor rejects cursors with a missing or invalid signature.
//
// Enabling signing (or changing the key) invalidates every cursor issued before
// the key was set. Passing an empty key disables signing and restores unsigned
// cursors.
func SetSigningKey(key []byte) {
	if len(key) == 0 {
		signingKey.Store(nil)
		return
	}

	keyCopy := make([]byte, len(key))
	copy(keyCopy, key)
	signingKey.Store(&keyCopy)
}

// EncodeCursor creates a base64 encoded cursor from a Cursor struct, signed
// when a signing key has been set with SetSigningKey
func EncodeCursor(c Cursor) (string, error) {
	jsonBytes, err := json.Marshal(c)
	if err != nil {
		return "", ez.New(ez.EINTERNAL, "failed to marshal cursor", err)
	}

	payload := base64.URLEncoding.EncodeToString(jsonBytes)

	key := signingKey.Load()
	if key == nil {
		return payload, nil
	}

	signature := base64.URLEncoding.EncodeToString(signCursor(*key, payload))

	return payload + "." + signature, nil
}

// DecodeCursor decodes a base64 encoded cursor string into a Cursor struct,
// verifying its signature when a signing key has been set with SetSigningKey
func DecodeCursor(encodedCursor string) (*Cursor, error) {
	if encodedCursor == "" {
		return nil, nil
	}

	payload := encodedCursor

	key := signingKey.Load()
	if key != nil {
		var encodedSignature string
		var found bool
		payload, encodedSignature, found = strings.Cut(encodedCursor, ".")
		if !found {
			return nil, ez.New(ez.EINVALID, "invalid cursor signature", nil)
		}

		signature, err := base64.URLEncoding.DecodeString(encodedSignature)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "invalid cursor signature", nil)
		}

		if !hmac.Equal(signature, signCursor(*key, payload)) {
			return nil, ez.New(ez.EINVALID, "invalid cursor signature", nil)
		}
	}

	jsonBytes, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "invalid cursor format", err)
	}

	var cursor Cursor
	err = json.Unmarshal(jsonBytes, &cursor)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "invalid cursor data", err)
	}

	return &cursor, nil
}

func signCursor(key []byte, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))

	return mac.Sum(nil)
}
