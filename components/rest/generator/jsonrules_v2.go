//go:build goexperiment.jsonv2

package generator

import (
	"strings"
	"unicode"
)

// The JSON rules of encoding/json on encoding/json/v2, which a toolchain with the jsonv2
// experiment builds: Go 1.27 by default, as Go 1.27.2's encoding/json writes. jsonrules_v1.go
// declares the same names for encoding/json's own implementation.

// encodingJSONV2 reports the rules of encoding/json on encoding/json/v2.
const encodingJSONV2 = true

// jsonMethods are the methods encoding/json writes a value with as the JSON they return, and
// textMethods those it writes a value with as a JSON string, in the order it prefers them; a
// method of jsonMethods wins over one of textMethods.
var (
	jsonMethods = []string{"MarshalJSONTo", "MarshalJSON"}
	textMethods = []string{"AppendText", "MarshalText"}
)

// jsonNumberMethods are json.Number's marshal methods: encoding/json writes a Number with its
// MarshalJSONTo, which a struct embedding one gets.
var jsonNumberMethods = map[string]bool{"MarshalJSONTo": true}

// jsonTagKey is the key a json tag's name, the text before its first comma, gives a field: the
// name when encoding/json takes it, "" when it uses the field's Go name instead. It takes any
// name without a backslash, quote or backtick; of one with them, the leading run of letters,
// digits and underscores when the name starts with a letter or an underscore, else nothing.
func jsonTagKey(name string) string {
	if !strings.ContainsAny(name, "\\'\"`") {
		return name
	}
	end := strings.IndexFunc(name, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if end <= 0 {
		return ""
	}
	first := []rune(name[:end])[0]
	if unicode.IsNumber(first) {
		return ""
	}

	return name[:end]
}
