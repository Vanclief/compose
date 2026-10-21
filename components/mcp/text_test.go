package mcp

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestSnake(t *testing.T) {
	cases := map[string]string{
		"listBillingProfiles": "list_billing_profiles",
		"Access Control":      "access_control",
		"getAPIKey":           "get_api_key",
		"Finance":             "finance",
		"list-units":          "list_units",
		"already_snake":       "already_snake",
		"HTTPServer":          "http_server",
		"v2Report":            "v2_report",
		"Users & Roles":       "users_roles",
		"Billing (beta)":      "billing_beta",
		"a--b__c":             "a_b_c",
		"_Private":            "private",
		"Año fiscal":          "año_fiscal",
	}
	for in, want := range cases {
		assert.Equal(t, want, snake(in), in)
	}
}

func TestTruncate(t *testing.T) {
	short := "one\ntwo"
	assert.Equal(t, short, truncate(short, 100))

	long := strings.Repeat("line\n", 100)
	out := truncate(long, 52)
	assert.True(t, strings.HasPrefix(out, "line\nline\n"))
	assert.Contains(t, out, "[output cut at 49 bytes of 500")
	assert.Equal(t, long, truncate(long, 0))

	// A newline far from the limit is not used: the long line is kept.
	log := "log: x\n" + strings.Repeat("a", 2000)
	out = truncate(log, 1000)
	assert.True(t, strings.HasPrefix(out, log[:1000]+"\n\n[output cut at 1000 bytes of 2007"), out[:40])

	// Without a nearby newline the cut steps back to a rune start.
	runes := strings.Repeat("é", 100)
	out = truncate(runes, 51)
	assert.True(t, strings.HasPrefix(out, strings.Repeat("é", 25)+"\n\n[output cut at 50 bytes of 200"))
	assert.True(t, utf8.ValidString(out))
}
