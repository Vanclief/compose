package enums

import (
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
