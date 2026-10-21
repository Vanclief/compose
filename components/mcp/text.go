package mcp

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// normalizeTerm is the lookup key of a term name or alias: lowercase with
// surrounding whitespace removed, so "Package", "package" and "paquete"
// resolve the same way.
func normalizeTerm(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// snake converts CamelCase, "Title Case" and kebab-case names to snake_case:
// "listBillingProfiles" -> "list_billing_profiles", "Access Control" ->
// "access_control", "getAPIKey" -> "get_api_key". Every run of characters
// that are not letters or digits becomes one "_": "Users & Roles" ->
// "users_roles".
func snake(s string) string {
	var b strings.Builder
	runes := []rune(strings.TrimSpace(s))
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
				b.WriteByte('_')
			}
			continue
		}
		if unicode.IsUpper(r) && i > 0 {
			prev := runes[i-1]
			nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				if !strings.HasSuffix(b.String(), "_") {
					b.WriteByte('_')
				}
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return strings.Trim(b.String(), "_")
}

// truncate cuts text to limit bytes and appends a hint that tells the agent
// how to ask for less. The cut is on a line boundary when one falls within
// the last 10% of the limit, else at the last rune start, so a long line is
// kept mostly whole and never split mid-rune. Text within the limit is
// returned unchanged.
func truncate(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := limit
	newline := strings.LastIndexByte(text[:limit], '\n')
	if newline > 0 && newline >= limit-limit/10 {
		cut = newline
	} else {
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
	}
	return text[:cut] + fmt.Sprintf("\n\n[output cut at %d bytes of %d: narrow the request with a module, a query, fewer names or a smaller limit]", cut, len(text))
}
