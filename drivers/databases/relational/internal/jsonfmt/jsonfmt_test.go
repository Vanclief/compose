package jsonfmt

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/dialect/pgdialect"
)

func TestAppendJSON(t *testing.T) {
	tests := []struct {
		name  string
		jsonb string
		want  string
	}{
		{name: "plain", jsonb: `{"a":"b"}`, want: `'{"a":"b"}'`},
		{name: "quote doubled", jsonb: `{"a":"it's"}`, want: `'{"a":"it''s"}'`},
		{name: "escaped quote", jsonb: `{"a":"\""}`, want: `'{"a":"\""}'`},
		{name: "backslash before a quote cannot break out", jsonb: `{"a":"\\'"}`, want: `'{"a":"\\''"}'`},
		{name: "lone backslash before a quote still doubles it", jsonb: `x\'y`, want: `'x\''y'`},
		{name: "odd run of backslashes before a quote", jsonb: `x\\\'y`, want: `'x\\\''y'`},
		{name: "trailing backslash and quote", jsonb: `\'`, want: `'\'''`},
		{name: "NUL escape stored as its text", jsonb: `{"a":"x\u0000y"}`, want: `'{"a":"x\\u0000y"}'`},
		{name: "the text \\u0000 is kept as encoded", jsonb: `{"a":"x\\u0000y"}`, want: `'{"a":"x\\u0000y"}'`},
		{name: "backslash pairs then a NUL escape", jsonb: `{"a":"\\\\\u0000"}`, want: `'{"a":"\\\\\\u0000"}'`},
		{name: "raw NUL byte dropped", jsonb: "{\"a\":\"x\x00y\"}", want: `'{"a":"xy"}'`},
		{name: "trailing backslash", jsonb: `{"a":"x"}\`, want: `'{"a":"x"}\'`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := string(AppendJSON(nil, []byte(tc.jsonb)))
			require.Equal(t, tc.want, got)
		})
	}
}

// TestAppendJSONMatchesBunExceptForTheTextU0000 pins what the correction
// changes: bun v1.3.0 and AppendJSON agree on every document without a \\ pair
// before u0000, and differ only there.
func TestAppendJSONMatchesBunExceptForTheTextU0000(t *testing.T) {
	bun := pgdialect.New()

	same := []string{`{"a":"b"}`, `{"a":"it's"}`, `{"a":"\""}`, `{"a":"\\'"}`, `x\'y`, `x\\\'y`, `\'`, `{"a":"x\u0000y"}`, `{"a":"\\\\\u0000"}`, `{"a":"\\n"}`, `[1,"\\"]`}
	for _, doc := range same {
		require.Equal(t, string(bun.AppendJSON(nil, []byte(doc))), string(AppendJSON(nil, []byte(doc))), doc)
	}

	encoded, err := json.Marshal(map[string]string{"a": `x\u0000y`})
	require.NoError(t, err)
	require.Equal(t, `{"a":"x\\u0000y"}`, string(encoded))
	require.Equal(t, `'{"a":"x\\\u0000y"}'`, string(bun.AppendJSON(nil, encoded)), "bun v1.3.0 mis-escapes the text \\u0000")
	require.Equal(t, `'{"a":"x\\u0000y"}'`, string(AppendJSON(nil, encoded)))
}
