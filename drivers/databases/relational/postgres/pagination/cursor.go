package pagination

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/vanclief/ez"
)

const (
	// MAX_PAGE_SIZE is the largest page; a larger limit is clamped to it.
	MAX_PAGE_SIZE = 1000
	// DEFAULT_LIMIT is the page size of a request without a limit.
	DEFAULT_LIMIT = 50
)

var signingKey atomic.Pointer[[]byte]

// Cursor holds the values needed for pagination. SortField and Direction pin
// the order the cursor was issued for, so it cannot be replayed under another
// sort. Cursors issued before Direction existed have it empty.
type Cursor struct {
	SortField   string      `json:"sort_field"`
	SortValue   interface{} `json:"sort_value"`
	UniqueField string      `json:"unique_field"`
	UniqueValue interface{} `json:"unique_value"`
	Direction   string      `json:"direction,omitempty"`
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
// verifying its signature when a signing key has been set with SetSigningKey.
//
// Numeric SortValue and UniqueValue are decoded without loss of precision:
// integers come back as int64 (so values above 2^53, such as snowflake IDs or
// unix nanoseconds, survive the round trip), integers above the int64 range
// as uint64 and other numbers as float64. Strings, bools and nil are returned
// unchanged. A value that is an object, an array, a number that fits none of
// these types or a string holding a NUL byte, which no Postgres text column
// can have produced and bun refuses to format, is ez.EINVALID.
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
	decoder := json.NewDecoder(bytes.NewReader(jsonBytes))
	decoder.UseNumber()
	err = decoder.Decode(&cursor)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "invalid cursor data", err)
	}

	cursor.SortValue, err = normalizeCursorValue(cursor.SortValue)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	cursor.UniqueValue, err = normalizeCursorValue(cursor.UniqueValue)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return &cursor, nil
}

// normalizeCursorValue converts a json.Number into int64 when it is an
// integer in int64 range, uint64 when it is an integer above that range that
// fits uint64, float64 otherwise. An object, an array, a number that fits
// none of them or a string holding a NUL byte, which only a forged cursor
// carries, is ez.EINVALID. Any other value is returned unchanged.
func normalizeCursorValue(v any) (any, error) {
	switch value := v.(type) {
	case map[string]any, []any:
		return nil, ez.New(ez.EINVALID, "invalid cursor data", nil)
	case string:
		if strings.ContainsRune(value, 0) {
			return nil, ez.New(ez.EINVALID, "invalid cursor data", nil)
		}
	}

	number, ok := v.(json.Number)
	if !ok {
		return v, nil
	}

	intValue, err := number.Int64()
	if err == nil {
		return intValue, nil
	}

	uintValue, err := strconv.ParseUint(number.String(), 10, 64)
	if err == nil {
		return uintValue, nil
	}

	floatValue, err := number.Float64()
	if err != nil {
		return nil, ez.New(ez.EINVALID, "invalid cursor data", err)
	}

	return floatValue, nil
}

func signCursor(key []byte, payload string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))

	return mac.Sum(nil)
}
