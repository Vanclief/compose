package logger

import (
	"bytes"
	"encoding/json"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// valueError is an error whose Error method has a value receiver.
type valueError struct {
	message string
}

func (e valueError) Error() string {
	return e.message
}

// pointerError is an error whose Error method has a pointer receiver.
type pointerError struct {
	message string
}

func (e *pointerError) Error() string {
	return e.message
}

// marshalingStringer has value-receiver String and MarshalZerologObject methods,
// so calling either through a nil pointer panics.
type marshalingStringer struct {
	name string
}

func (s marshalingStringer) String() string {
	return s.name
}

func (s marshalingStringer) MarshalZerologObject(e *zerolog.Event) {
	e.Str("name", s.name)
}

// panickingStringer has a String method that always panics.
type panickingStringer struct{}

func (s *panickingStringer) String() string {
	panic("string failed")
}

// marshalingValue has a value-receiver MarshalZerologObject and no String, so
// zerolog calling it through a nil pointer panics.
type marshalingValue struct {
	name string
}

func (v marshalingValue) MarshalZerologObject(e *zerolog.Event) {
	e.Str("name", v.name)
}

// nilSafeMarshaler has a pointer-receiver MarshalZerologObject that handles a
// nil receiver.
type nilSafeMarshaler struct {
	name string
}

func (m *nilSafeMarshaler) MarshalZerologObject(e *zerolog.Event) {
	if m == nil {
		e.Str("name", "none")
		return
	}
	e.Str("name", m.name)
}

// marshalingError is an error whose MarshalZerologObject has a value receiver,
// which zerolog calls before it checks the error for nil.
type marshalingError struct {
	message string
}

func (e marshalingError) Error() string {
	return e.message
}

func (e marshalingError) MarshalZerologObject(event *zerolog.Event) {
	event.Str("message", e.message)
}

// readingMarshaler has a pointer-receiver MarshalZerologObject that reads its
// receiver without checking it.
type readingMarshaler struct {
	name string
}

func (m *readingMarshaler) MarshalZerologObject(e *zerolog.Event) {
	e.Str("name", m.name)
}

// nilPanicker has a String method that panics with nil.
type nilPanicker struct{}

func (p *nilPanicker) String() string {
	panic(nil)
}

// logWith logs one line through a logger built with With(key, value) and
// returns the decoded JSON fields.
func logWith(t *testing.T, key string, value any) map[string]any {
	t.Helper()

	var buf bytes.Buffer
	NewZero(zerolog.New(&buf)).With(key, value).Info().Msg("message")

	var fields map[string]any
	err := json.Unmarshal(buf.Bytes(), &fields)
	require.NoError(t, err)

	return fields
}

func TestZeroLoggerWithLogsANilPointerToAValueReceiverStringerAsNull(t *testing.T) {
	testCases := []struct {
		name  string
		key   string
		value any
	}{
		{"nil uuid pointer", "id", (*uuid.UUID)(nil)},
		{"nil time pointer", "at", (*time.Time)(nil)},
		{"nil pointer to a value receiver zerolog marshaler", "v", (*marshalingStringer)(nil)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fields := logWith(t, tc.key, tc.value)

			value, ok := fields[tc.key]
			require.True(t, ok)
			require.Nil(t, value)
		})
	}
}

func TestZeroLoggerWithLogsANilPointerWhoseStringPanicsAsNull(t *testing.T) {
	fields := logWith(t, "url", (*url.URL)(nil))

	value, ok := fields["url"]
	require.True(t, ok)
	require.Nil(t, value)
}

func TestZeroLoggerWithLogsANilPointerToAValueReceiverObjectMarshalerAsNull(t *testing.T) {
	fields := logWith(t, "v", (*marshalingValue)(nil))

	value, ok := fields["v"]
	require.True(t, ok)
	require.Nil(t, value)
}

func TestZeroLoggerWithLogsANilPointerWhoseObjectMarshalerPanicsAsNull(t *testing.T) {
	fields := logWith(t, "v", (*readingMarshaler)(nil))

	value, ok := fields["v"]
	require.True(t, ok)
	require.Nil(t, value)

	fields = logWith(t, "v", &readingMarshaler{name: "set"})
	require.Equal(t, map[string]any{"name": "set"}, fields["v"])
}

func TestZeroLoggerWithLeavesOutANilErrorWhoseObjectMarshalerPanics(t *testing.T) {
	fields := logWith(t, "err", (*marshalingError)(nil))

	_, ok := fields["err"]
	require.False(t, ok)
	require.Equal(t, "message", fields["message"])
}

func TestZeroLoggerWithKeepsTheFieldsAroundANilPointerThatPanics(t *testing.T) {
	var buf bytes.Buffer
	NewZero(zerolog.New(&buf)).With("before", "a", "url", (*url.URL)(nil), "after", "b").Info().Msg("message")

	var fields map[string]any
	err := json.Unmarshal(buf.Bytes(), &fields)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"level": "info", "before": "a", "url": nil, "after": "b", "message": "message"}, fields)
}

func TestZeroLoggerWithLogsANilPointerThatPanicsWithNilAsNull(t *testing.T) {
	testCases := []struct {
		name     string
		panicNil string
	}{
		{"panic(nil) is a PanicNilError", "0"},
		{"panic(nil) recovers as nil", "1"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GODEBUG", "panicnil="+tc.panicNil)

			var buf bytes.Buffer
			NewZero(zerolog.New(&buf)).With("before", "a", "v", (*nilPanicker)(nil), "after", "b").Info().Msg("message")

			var fields map[string]any
			err := json.Unmarshal(buf.Bytes(), &fields)
			require.NoError(t, err)
			require.Equal(t, map[string]any{"level": "info", "before": "a", "v": nil, "after": "b", "message": "message"}, fields)
		})
	}
}

func TestZeroLoggerWithCallsAPointerReceiverObjectMarshalerOnANilPointer(t *testing.T) {
	fields := logWith(t, "v", (*nilSafeMarshaler)(nil))
	require.Equal(t, map[string]any{"name": "none"}, fields["v"])

	fields = logWith(t, "v", &nilSafeMarshaler{name: "set"})
	require.Equal(t, map[string]any{"name": "set"}, fields["v"])
}

func TestZeroLoggerWithLogsOtherNilPointersAsNull(t *testing.T) {
	type plain struct {
		Name string
	}

	fields := logWith(t, "v", (*plain)(nil))

	value, ok := fields["v"]
	require.True(t, ok)
	require.Nil(t, value)
}

func TestZeroLoggerWithDoesNotCatchAPanicFromANonNilStringer(t *testing.T) {
	require.Panics(t, func() {
		logWith(t, "url", &panickingStringer{})
	})
}

func TestZeroLoggerWithLeavesOutATypedNilError(t *testing.T) {
	testCases := []struct {
		name  string
		value any
	}{
		{"nil value receiver error", (*valueError)(nil)},
		{"nil pointer receiver error", (*pointerError)(nil)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			fields := logWith(t, "err", tc.value)

			_, ok := fields["err"]
			require.False(t, ok)
		})
	}
}

func TestZeroLoggerWithCallsAPointerReceiverStringOnANilPointer(t *testing.T) {
	fields := logWith(t, "n", (*big.Int)(nil))
	require.Equal(t, "<nil>", fields["n"])
}

func TestZeroLoggerWithLogsAUUIDPointerAsItsString(t *testing.T) {
	id := uuid.MustParse("6f1c1b8e-2f4a-4d55-9a3e-0f6f3a2b9c11")

	fields := logWith(t, "id", &id)
	require.Equal(t, id.String(), fields["id"])
}

func TestZeroLoggerWithLogsAnErrorAsItsMessage(t *testing.T) {
	fields := logWith(t, "err", &valueError{message: "boom"})
	require.Equal(t, "boom", fields["err"])
}
