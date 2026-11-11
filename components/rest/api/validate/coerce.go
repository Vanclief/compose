package validate

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

// Kind describes the schema type a parameter is coerced to.
// Type is one of "string", "integer", "number", "boolean" or "array".
type Kind struct {
	Type  string
	Items *Kind
}

// MaxExactNumber is the length of the longest JSON number literal without an
// exponent (e or E) that the validator is handed as the client wrote it;
// longer ones, and ones with an exponent, are handed as canonical text.
// Exact text keeps bounds and multipleOf checked on the digits a json.Number
// or big.Int field receives, not on a rounded value. The validator parses
// every number with big.Rat, whose cost grows with an exponent (1e-999999
// takes milliseconds) but without one is linear in a length this caps.
const MaxExactNumber = 64

// Coerce converts raw parameter values to the JSON value the schema expects.
// Integers become json.Number in canonical text, those past the int64 and
// uint64 ranges only up to MaxExactNumber characters; numbers become json.Number
// as written when no longer than MaxExactNumber and without an exponent, else
// the shortest text of the nearest float64; a number that is no JSON literal
// (+5, 007) stays the raw string, where an integer takes it. Booleans accept
// exactly "true" and "false", arrays take repeated values or one
// comma-separated value. A
// value that does not parse is returned as the raw string so validation
// reports a type error. No values means absent and returns nil.
func Coerce(values []string, kind Kind) any {
	if len(values) == 0 {
		return nil
	}

	switch kind.Type {
	case "integer":
		n, err := strconv.ParseInt(values[0], 10, 64)
		if err == nil {
			// The canonical text: ParseInt accepts 007 and +5, which are not
			// JSON numbers.
			return json.Number(strconv.FormatInt(n, 10))
		}
		// A uint64 value at or above 2^63.
		u, err := strconv.ParseUint(values[0], 10, 64)
		if err == nil {
			return json.Number(strconv.FormatUint(u, 10))
		}
		// Past both ranges, for a big.Int: the length cap keeps the
		// validator's parsing cheap. Base 10 takes a sign and digits only.
		if len(values[0]) > MaxExactNumber {
			return values[0]
		}
		b, ok := new(big.Int).SetString(values[0], 10)
		if !ok {
			return values[0]
		}
		return json.Number(b.String())
	case "number":
		f, err := strconv.ParseFloat(values[0], 64)
		if err != nil || !json.Valid([]byte(values[0])) {
			return values[0]
		}
		if len(values[0]) <= MaxExactNumber && !strings.ContainsAny(values[0], "eE") {
			return json.Number(values[0])
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
	case "boolean":
		switch values[0] {
		case "true":
			return true
		case "false":
			return false
		}
		return values[0]
	case "array":
		items := values
		if len(values) == 1 {
			items = strings.Split(values[0], ",")
		}
		itemKind := Kind{Type: "string"}
		if kind.Items != nil {
			itemKind = *kind.Items
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = Coerce([]string{item}, itemKind)
		}
		return out
	}

	return values[0]
}
