package pagination

import (
	"encoding/base64"
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
		SortValue:   float64(42),
		UniqueField: "id",
		UniqueValue: float64(42),
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
