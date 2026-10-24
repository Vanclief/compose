// Package token generates and checks opaque bearer tokens.
//
// A token has the shape "<prefix>_<body>": prefix is 2 to 12 characters from
// [a-z0-9] and body is 43 base62 characters drawn uniformly with crypto/rand,
// which carries 256 bits of entropy. Applications store only the SHA-256 Hash
// of a token and look it up by that digest.
//
// The prefix tells token kinds apart. With an application prefix such as "app":
//
//   - API key: "app_"
//   - OAuth access token: "appat_"
//   - OAuth refresh token: "apprt_"
//   - session: "apps_"
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"regexp"
	"strings"

	"github.com/vanclief/ez"
)

const (
	alphabet   = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	bodyLength = 43
	hintLength = 4
	// rejectFrom is the largest multiple of 62 that fits in a byte. Bytes at or
	// above it are discarded so every alphabet character is equally likely.
	rejectFrom = 248
)

var prefixPattern = regexp.MustCompile(`^[a-z0-9]{2,12}$`)

// Token is a freshly generated credential. Value is shown to the user once.
// Hash is what the application stores. Hint is the last four characters of
// Value, safe to show in lists.
type Token struct {
	Value string
	Hash  []byte
	Hint  string
}

// ValidPrefix reports whether prefix matches ^[a-z0-9]{2,12}$. Packages that
// derive token prefixes from configuration validate them with this rule.
func ValidPrefix(prefix string) bool {
	return prefixPattern.MatchString(prefix)
}

// Generate returns a token "<prefix>_<body>". body is 43 characters from the
// base62 alphabet [0-9A-Za-z], drawn uniformly with crypto/rand, so it holds
// 256 bits of entropy. prefix must match ^[a-z0-9]{2,12}$ and may itself
// contain no "_". Returns ez.EINVALID for a bad prefix, ez.EINTERNAL when the
// random source fails.
func Generate(prefix string) (Token, error) {
	if !ValidPrefix(prefix) {
		return Token{}, ez.New(ez.EINVALID, "token: prefix must match ^[a-z0-9]{2,12}$", nil)
	}

	body := make([]byte, 0, bodyLength)
	buf := make([]byte, 64)
	for len(body) < bodyLength {
		_, err := rand.Read(buf)
		if err != nil {
			return Token{}, ez.New(ez.EINTERNAL, "token: random source failed", err)
		}
		for _, b := range buf {
			if b >= rejectFrom {
				continue
			}
			body = append(body, alphabet[b%62])
			if len(body) == bodyLength {
				break
			}
		}
	}

	value := prefix + "_" + string(body)
	return Token{
		Value: value,
		Hash:  Hash(value),
		Hint:  Hint(value),
	}, nil
}

// Hash returns the SHA-256 digest of value. It is deterministic and unsalted:
// values carry 256 bits of entropy so neither brute force nor precomputation
// applies, and the digest is what the store looks up by.
func Hash(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

// Prefix returns the part before the first "_", and false when value has no
// "_" or the prefix is empty.
func Prefix(value string) (string, bool) {
	prefix, _, found := strings.Cut(value, "_")
	if !found || prefix == "" {
		return "", false
	}
	return prefix, true
}

// Check reports whether value has the given prefix and a well-formed body:
// the separator, exactly 43 base62 characters, nothing else. Use it before
// hashing so garbage never reaches the database.
func Check(prefix, value string) bool {
	body, found := strings.CutPrefix(value, prefix+"_")
	if !found || len(body) != bodyLength {
		return false
	}
	for i := 0; i < len(body); i++ {
		if strings.IndexByte(alphabet, body[i]) < 0 {
			return false
		}
	}
	return true
}

// Hint returns the last four characters of value, or "" when it is shorter.
func Hint(value string) string {
	if len(value) < hintLength {
		return ""
	}
	return value[len(value)-hintLength:]
}

// Equal compares two hashes in constant time.
func Equal(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}
