package enums

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vanclief/ez"
)

type color string

const (
	colorNone color = ""
	colorRed  color = "red"
	colorBlue color = "blue"
)

// unchanged is a sentinel that is never a member of any set below, so a test
// can prove that a failed Scan/Unmarshal did not touch out.
const unchanged color = "unchanged"

var (
	withEmpty    = Set([]color{colorNone, colorRed, colorBlue})
	withoutEmpty = Set([]color{colorRed, colorBlue})
)

// checkResult asserts the error (or lack of it) and the final value of out.
// wantErr is a substring of err.Error(); "" means no error is expected.
func checkResult(t *testing.T, err error, wantErr string, got, want color) {
	t.Helper()

	if got != want {
		t.Errorf("out = %q, want %q", got, want)
	}

	if wantErr == "" {
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		return
	}

	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantErr)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("error %q does not contain %q", err.Error(), wantErr)
	}
	if ez.ErrorCode(err) != ez.EINVALID {
		t.Errorf("error code = %q, want %q", ez.ErrorCode(err), ez.EINVALID)
	}
}

func TestScan(t *testing.T) {
	tests := []struct {
		name    string
		src     any
		allowed map[color]struct{}
		want    color
		wantErr string
	}{
		{name: "nil with empty member", src: nil, allowed: withEmpty, want: colorNone},
		{name: "nil without empty member", src: nil, allowed: withoutEmpty, want: unchanged, wantErr: "invalid enum value"},
		{name: "valid string", src: "red", allowed: withoutEmpty, want: colorRed},
		{name: "valid bytes", src: []byte("blue"), allowed: withoutEmpty, want: colorBlue},
		{name: "unknown string", src: "nope", allowed: withoutEmpty, want: unchanged, wantErr: "invalid enum value"},
		{name: "unsupported type", src: 42, allowed: withoutEmpty, want: unchanged, wantErr: "unsupported SQL type: int"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := unchanged
			err := Scan(tt.src, &out, tt.allowed)
			checkResult(t, err, tt.wantErr, out, tt.want)
		})
	}
}

func TestUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		b       []byte
		allowed map[color]struct{}
		want    color
		wantErr string
	}{
		{name: "valid", b: []byte(`"red"`), allowed: withoutEmpty, want: colorRed},
		{name: "unknown value", b: []byte(`"nope"`), allowed: withoutEmpty, want: unchanged, wantErr: "invalid enum value"},
		{name: "malformed json", b: []byte(`42`), allowed: withoutEmpty, want: unchanged, wantErr: "cannot unmarshal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := unchanged
			err := Unmarshal(tt.b, &out, tt.allowed)
			checkResult(t, err, tt.wantErr, out, tt.want)
		})
	}
}

// TestUnmarshalKeepsJSONErrorInspectable pins that wrapping the JSON error
// with ez does not hide it from errors.As (relies on ez >= v1.6.0 Unwrap).
func TestUnmarshalKeepsJSONErrorInspectable(t *testing.T) {
	out := unchanged
	err := Unmarshal([]byte(`42`), &out, withoutEmpty)

	var jsonErr *json.UnmarshalTypeError
	if !errors.As(err, &jsonErr) {
		t.Fatalf("errors.As could not find *json.UnmarshalTypeError in: %v", err)
	}
}

func TestValue(t *testing.T) {
	tests := []struct {
		name    string
		value   color
		allowed map[color]struct{}
		want    driver.Value
		wantErr string
	}{
		{name: "member", value: colorRed, allowed: withoutEmpty, want: "red"},
		{name: "empty with empty member is NULL", value: colorNone, allowed: withEmpty, want: nil},
		{name: "empty without empty member", value: colorNone, allowed: withoutEmpty, want: nil, wantErr: "invalid enum value"},
		{name: "unknown", value: "nope", allowed: withoutEmpty, want: nil, wantErr: "invalid enum value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Value(tt.value, tt.allowed)
			if got != tt.want {
				t.Errorf("Value = %#v, want %#v", got, tt.want)
			}
			checkResult(t, err, tt.wantErr, unchanged, unchanged)
		})
	}
}

// TestScanValueRoundTripKeepsNull pins that a SQL NULL survives a read-then-write
// unchanged: Scan(nil) yields "", and Value("") yields NULL again, not an empty SQL string.
func TestScanValueRoundTripKeepsNull(t *testing.T) {
	var out color
	err := Scan(nil, &out, withEmpty)
	if err != nil {
		t.Fatalf("Scan(nil): %v", err)
	}

	got, err := Value(out, withEmpty)
	if err != nil {
		t.Fatalf("Value(%q): %v", out, err)
	}
	if got != nil {
		t.Fatalf("round trip wrote %#v, want SQL NULL (nil)", got)
	}
}

// TestUnmarshalNullMatchesScanNil locks in that JSON null and SQL NULL agree:
// both resolve to the zero value "" and are accepted iff "" is a member.
func TestUnmarshalNullMatchesScanNil(t *testing.T) {
	tests := []struct {
		name    string
		allowed map[color]struct{}
		want    color
		wantErr string
	}{
		{name: "with empty member", allowed: withEmpty, want: colorNone},
		{name: "without empty member", allowed: withoutEmpty, want: unchanged, wantErr: "invalid enum value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jsonOut := unchanged
			err := Unmarshal([]byte("null"), &jsonOut, tt.allowed)
			checkResult(t, err, tt.wantErr, jsonOut, tt.want)

			sqlOut := unchanged
			err = Scan(nil, &sqlOut, tt.allowed)
			checkResult(t, err, tt.wantErr, sqlOut, tt.want)

			if jsonOut != sqlOut {
				t.Errorf("JSON null gave %q but SQL NULL gave %q", jsonOut, sqlOut)
			}
		})
	}
}
