package token

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestGenerateRejectsBadPrefix(t *testing.T) {
	bad := []string{"", "a", "abcdefghijklm", "App", "ab_c", "ab-c", "ab c"}
	for _, prefix := range bad {
		assert.False(t, ValidPrefix(prefix), prefix)

		_, err := Generate(prefix)
		require.Error(t, err, prefix)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), prefix)
	}
}

func TestGenerateAcceptsPrefixBoundaries(t *testing.T) {
	good := []string{"ab", "abcdefghijkl", "a1", "9z"}
	for _, prefix := range good {
		assert.True(t, ValidPrefix(prefix), prefix)

		tok, err := Generate(prefix)
		require.NoError(t, err, prefix)
		assert.True(t, strings.HasPrefix(tok.Value, prefix+"_"), prefix)
	}
}

func TestGenerateBodyAlphabetAndLength(t *testing.T) {
	tok, err := Generate("app")
	require.NoError(t, err)

	body := strings.TrimPrefix(tok.Value, "app_")
	require.Len(t, body, 43)
	for _, c := range body {
		isDigit := c >= '0' && c <= '9'
		isUpper := c >= 'A' && c <= 'Z'
		isLower := c >= 'a' && c <= 'z'
		assert.True(t, isDigit || isUpper || isLower, "unexpected character %q", c)
	}
	assert.True(t, Check("app", tok.Value))
}

func TestGenerateIsUnique(t *testing.T) {
	seen := make(map[string]bool, 10000)
	for i := 0; i < 10000; i++ {
		tok, err := Generate("app")
		require.NoError(t, err)
		require.False(t, seen[tok.Value], "duplicate token %s", tok.Value)
		seen[tok.Value] = true
	}
}

func TestGenerateFillsHashAndHint(t *testing.T) {
	tok, err := Generate("appat")
	require.NoError(t, err)

	assert.Equal(t, Hash(tok.Value), tok.Hash)
	assert.Equal(t, Hint(tok.Value), tok.Hint)
	assert.Len(t, tok.Hint, 4)
}

func TestCheck(t *testing.T) {
	tok, err := Generate("app")
	require.NoError(t, err)
	body := strings.TrimPrefix(tok.Value, "app_")

	assert.True(t, Check("app", tok.Value))

	assert.False(t, Check("apps", tok.Value), "wrong prefix")
	assert.False(t, Check("ap", tok.Value), "shorter prefix")
	assert.False(t, Check("app", "apps_"+body), "value has another prefix")
	assert.False(t, Check("app", "app"+body), "missing separator")
	assert.False(t, Check("app", "app_"+body[:42]), "42 characters")
	assert.False(t, Check("app", "app_"+body+"A"), "44 characters")
	assert.False(t, Check("app", "app_"+body[:42]+"-"), "dash")
	assert.False(t, Check("app", "app_"+body[:42]+"/"), "slash")
	assert.False(t, Check("app", "app_"+body[:42]+"="), "equals")
	assert.False(t, Check("app", "app_"+body[:20]+"_"+body[21:]), "extra underscore")
	assert.False(t, Check("app", ""), "empty")
}

func TestPrefix(t *testing.T) {
	prefix, ok := Prefix("app_abc")
	assert.True(t, ok)
	assert.Equal(t, "app", prefix)

	prefix, ok = Prefix("appat_abc_def")
	assert.True(t, ok)
	assert.Equal(t, "appat", prefix)

	prefix, ok = Prefix("app_")
	assert.True(t, ok)
	assert.Equal(t, "app", prefix)

	_, ok = Prefix("appabc")
	assert.False(t, ok)

	_, ok = Prefix("_abc")
	assert.False(t, ok)

	_, ok = Prefix("")
	assert.False(t, ok)
}

func TestHash(t *testing.T) {
	first := Hash("app_value")
	second := Hash("app_value")

	assert.Len(t, first, 32)
	assert.Equal(t, first, second)
	assert.NotEqual(t, first, Hash("app_other"))
}

func TestHint(t *testing.T) {
	assert.Equal(t, "", Hint(""))
	assert.Equal(t, "", Hint("abc"))
	assert.Equal(t, "abcd", Hint("abcd"))
	assert.Equal(t, "wxyz", Hint("app_wxyz"))
}

func TestEqual(t *testing.T) {
	a := Hash("app_one")

	assert.True(t, Equal(a, Hash("app_one")))
	assert.False(t, Equal(a, Hash("app_two")))
	assert.False(t, Equal(a, a[:31]))
	assert.False(t, Equal(a, nil))
}
