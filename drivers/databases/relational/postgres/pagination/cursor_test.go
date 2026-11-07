package pagination

import (
	"encoding/base64"
	"math"
	"strings"
	"testing"

	"github.com/vanclief/ez"
)

func setTestSigningKey(t *testing.T, key []byte) {
	t.Helper()

	SetSigningKey(key)
	t.Cleanup(func() {
		SetSigningKey(nil)
	})
}

func testCursor() Cursor {
	return Cursor{
		SortField:   "id",
		SortValue:   int64(42),
		UniqueField: "id",
		UniqueValue: int64(42),
		Direction:   "desc",
	}
}

func TestCursorRoundTripUnsigned(t *testing.T) {
	encoded, err := EncodeCursor(testCursor())
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	if strings.Contains(encoded, ".") {
		t.Fatalf("expected unsigned cursor without signature, got %q", encoded)
	}

	decoded, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}

	if *decoded != testCursor() {
		t.Fatalf("expected %+v, got %+v", testCursor(), *decoded)
	}
}

func TestCursorRoundTripSigned(t *testing.T) {
	setTestSigningKey(t, []byte("test-signing-key"))

	encoded, err := EncodeCursor(testCursor())
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	if !strings.Contains(encoded, ".") {
		t.Fatalf("expected signed cursor, got %q", encoded)
	}

	decoded, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}

	if *decoded != testCursor() {
		t.Fatalf("expected %+v, got %+v", testCursor(), *decoded)
	}
}

func TestCursorRoundTripPreservesValueTypes(t *testing.T) {
	tests := []struct {
		name        string
		sortValue   interface{}
		uniqueValue interface{}
	}{
		{name: "int64 above 2^53", sortValue: int64(1700000000123456789), uniqueValue: "abc"},
		{name: "uint64 above MaxInt64", sortValue: uint64(9223372036854775809), uniqueValue: uint64(math.MaxUint64)},
		{name: "float", sortValue: float64(1.5), uniqueValue: "abc"},
		{name: "string", sortValue: "2026-10-21T00:00:00Z", uniqueValue: "abc"},
		{name: "nil", sortValue: nil, uniqueValue: nil},
	}

	for _, signed := range []bool{false, true} {
		for _, tc := range tests {
			name := tc.name
			if signed {
				name = "signed " + name
			}

			t.Run(name, func(t *testing.T) {
				if signed {
					setTestSigningKey(t, []byte("test-signing-key"))
				}

				original := Cursor{
					SortField:   "created_at",
					SortValue:   tc.sortValue,
					UniqueField: "id",
					UniqueValue: tc.uniqueValue,
					Direction:   "asc",
				}

				encoded, err := EncodeCursor(original)
				if err != nil {
					t.Fatalf("encode cursor: %v", err)
				}

				decoded, err := DecodeCursor(encoded)
				if err != nil {
					t.Fatalf("decode cursor: %v", err)
				}

				if *decoded != original {
					t.Fatalf("expected %#v, got %#v", original, *decoded)
				}
			})
		}
	}
}

func TestDecodeCursorEmptyReturnsNil(t *testing.T) {
	setTestSigningKey(t, []byte("test-signing-key"))

	decoded, err := DecodeCursor("")
	if err != nil {
		t.Fatalf("decode empty cursor: %v", err)
	}

	if decoded != nil {
		t.Fatalf("expected nil cursor, got %+v", *decoded)
	}
}

func TestDecodeCursorRejectsInvalidSignatures(t *testing.T) {
	setTestSigningKey(t, []byte("test-signing-key"))

	signed, err := EncodeCursor(testCursor())
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	_, signature, _ := strings.Cut(signed, ".")
	forgedPayload := base64.URLEncoding.EncodeToString([]byte(`{"sort_field":"id","sort_value":1,"unique_field":"id","unique_value":1}`))

	SetSigningKey(nil)
	unsigned, err := EncodeCursor(testCursor())
	if err != nil {
		t.Fatalf("encode unsigned cursor: %v", err)
	}

	SetSigningKey([]byte("other-signing-key"))
	otherKey, err := EncodeCursor(testCursor())
	if err != nil {
		t.Fatalf("encode cursor with other key: %v", err)
	}

	SetSigningKey([]byte("test-signing-key"))

	tests := []struct {
		name   string
		cursor string
	}{
		{name: "tampered payload", cursor: forgedPayload + "." + signature},
		{name: "unsigned cursor", cursor: unsigned},
		{name: "signed with other key", cursor: otherKey},
		{name: "malformed signature", cursor: forgedPayload + ".!!!"},
		{name: "empty signature", cursor: forgedPayload + "."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeCursor(tc.cursor)
			if err == nil {
				t.Fatal("expected invalid signature error")
			}

			if ez.ErrorCode(err) != ez.EINVALID {
				t.Fatalf("expected %s, got %s", ez.EINVALID, ez.ErrorCode(err))
			}

			if ez.ErrorMessage(err) != "invalid cursor signature" {
				t.Fatalf("expected invalid cursor signature, got %q", ez.ErrorMessage(err))
			}
		})
	}
}

func TestDecodeCursorRejectsValuesAQueryCannotTake(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "object sort value", payload: `{"sort_field":"id","sort_value":{"a":1},"unique_field":"id","unique_value":1}`},
		{name: "array sort value", payload: `{"sort_field":"id","sort_value":[1,2],"unique_field":"id","unique_value":1}`},
		{name: "number out of range sort value", payload: `{"sort_field":"id","sort_value":1e400,"unique_field":"id","unique_value":1}`},
		{name: "object unique value", payload: `{"sort_field":"id","sort_value":1,"unique_field":"id","unique_value":{}}`},
		{name: "array unique value", payload: `{"sort_field":"id","sort_value":1,"unique_field":"id","unique_value":[]}`},
		{name: "number out of range unique value", payload: `{"sort_field":"id","sort_value":1,"unique_field":"id","unique_value":-1e400}`},
		// bun v1.3.0 writes a formatting error into the SQL for a string with a NUL byte.
		{name: "NUL byte in sort value", payload: `{"sort_field":"name","sort_value":"a\u0000b","unique_field":"id","unique_value":1}`},
		{name: "NUL byte in unique value", payload: `{"sort_field":"id","sort_value":1,"unique_field":"code","unique_value":"\u0000"}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeCursor(base64.URLEncoding.EncodeToString([]byte(tc.payload)))
			if ez.ErrorCode(err) != ez.EINVALID {
				t.Fatalf("expected %s, got %v", ez.EINVALID, err)
			}

			if ez.ErrorMessage(err) != "invalid cursor data" {
				t.Fatalf("expected invalid cursor data, got %q", ez.ErrorMessage(err))
			}
		})
	}
}

func TestDecodeCursorKeepsScalarValues(t *testing.T) {
	payload := `{"sort_field":"id","sort_value":true,"unique_field":"id","unique_value":1e20}`

	cursor, err := DecodeCursor(base64.URLEncoding.EncodeToString([]byte(payload)))
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}

	if cursor.SortValue != true || cursor.UniqueValue != float64(1e20) {
		t.Fatalf("expected true and 1e20, got %#v and %#v", cursor.SortValue, cursor.UniqueValue)
	}
}

func TestDecodeCursorNumberTypes(t *testing.T) {
	tests := []struct {
		name   string
		number string
		want   interface{}
	}{
		{name: "negative", number: "-5", want: int64(-5)},
		{name: "MaxInt64", number: "9223372036854775807", want: int64(math.MaxInt64)},
		{name: "MaxInt64 plus one", number: "9223372036854775808", want: uint64(9223372036854775808)},
		{name: "MaxInt64 plus two", number: "9223372036854775809", want: uint64(9223372036854775809)},
		{name: "MaxUint64", number: "18446744073709551615", want: uint64(math.MaxUint64)},
		{name: "above MaxUint64", number: "18446744073709551616", want: float64(18446744073709551616)},
		{name: "below MinInt64", number: "-9223372036854775809", want: float64(-9223372036854775809)},
		{name: "fraction above MaxInt64", number: "9223372036854775809.5", want: float64(9223372036854775809.5)},
		{name: "float", number: "1.5", want: float64(1.5)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := `{"sort_field":"id","sort_value":` + tc.number + `,"unique_field":"id","unique_value":` + tc.number + `}`

			cursor, err := DecodeCursor(base64.URLEncoding.EncodeToString([]byte(payload)))
			if err != nil {
				t.Fatalf("decode cursor: %v", err)
			}

			if cursor.SortValue != tc.want || cursor.UniqueValue != tc.want {
				t.Fatalf("expected %#v, got %#v and %#v", tc.want, cursor.SortValue, cursor.UniqueValue)
			}
		})
	}
}

func TestCursorIntegralFloatAboveMaxInt64DecodesAsUint64OfTheSameFloat(t *testing.T) {
	// json encodes an integral float64 below 1e21 as its shortest digits
	// without an exponent, here 9223372036854776000, so it decodes as that
	// integer, which converts back to the same float64.
	original := float64(1 << 63)

	encoded, err := EncodeCursor(Cursor{SortField: "score", SortValue: original, UniqueField: "id", UniqueValue: int64(1)})
	if err != nil {
		t.Fatalf("encode cursor: %v", err)
	}

	decoded, err := DecodeCursor(encoded)
	if err != nil {
		t.Fatalf("decode cursor: %v", err)
	}

	value, ok := decoded.SortValue.(uint64)
	if !ok || value != 9223372036854776000 || float64(value) != original {
		t.Fatalf("expected uint64 9223372036854776000, got %#v", decoded.SortValue)
	}
}
