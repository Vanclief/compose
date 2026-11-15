package validate

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	localized "golang.org/x/text/message"
)

// maxExponent bounds the exponent CanonicalNumber reads; any larger one puts
// a number far past the float64 range either way.
const maxExponent = 1 << 40

// CanonicalNumber returns the text the validator is handed for s, a JSON
// number literal or a base-10 integer with leading zeros. The validator
// parses every number with big.Rat, whose cost grows with the length of the
// text and with its exponent, so it gets short text that compares with
// every number a document states as s does: those are bounds and enum
// values, each a float64, an int64 or a uint64, so of at most 20
// significant digits and less than 10^309 in magnitude. The text is
//
//   - s itself, when it has at most MaxExactNumber characters and no
//     exponent;
//   - else the exact value of s as digits and an exponent, when it has at
//     most MaxExactNumber significant digits, the first of them from the
//     10^308 place down to the 10^-324 place;
//   - else, for more significant digits in that span, the first
//     MaxExactNumber of them followed by a 5, and a 5 in the tenths where
//     that alone would make an integer of a number that is not one: no
//     number of MaxExactNumber significant digits or fewer lies between that
//     text and s;
//   - else 1e309 when s is at least 10^309 in magnitude, past every float64
//     (or 10^309 and a half, when s is no integer), or 1e-325 when it is
//     under 10^-324, closer to 0 than every float64 but 0, with the sign of
//     s.
//
// The text is an integer exactly when s is. multipleOf gives on it the
// answer it gives on s for the first two forms and for 1e-325; on the others
// it cannot for every multipleOf, and a validator from NewRequestCompiler
// refuses such a number where its schema has multipleOf, unless that is 1,
// or 1/k and the number an integer (see multipleOfDigits).
func CanonicalNumber(s string) json.Number {
	if len(s) <= MaxExactNumber && !strings.ContainsAny(s, "eE") {
		return json.Number(s)
	}

	sign, digits, scale := splitNumber(s)
	if digits == "" {
		return "0"
	}
	// The power of ten of the first digit.
	lead := scale + int64(len(digits)) - 1

	switch {
	case lead >= 309 && scale < 0:
		return json.Number(sign + "1" + strings.Repeat("0", 309) + ".5")
	case lead >= 309:
		return json.Number(sign + "1e309")
	case lead <= -325:
		return json.Number(sign + "1e-325")
	case len(digits) <= MaxExactNumber:
		return json.Number(sign + digits + exponentText(scale))
	}

	head := digits[:MaxExactNumber] + "5"
	headScale := lead - MaxExactNumber
	if scale < 0 && headScale >= 0 {
		return json.Number(sign + head + strings.Repeat("0", int(headScale)) + "5e-1")
	}
	return json.Number(sign + head + exponentText(headScale))
}

// splitNumber returns the sign of s, a JSON number literal or a base-10
// integer with leading zeros, and its value as digits × 10^scale, digits
// without leading or trailing zeros ("" for zero).
func splitNumber(s string) (sign string, digits string, scale int64) {
	mantissa := s
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = mantissa[1:]
	}
	var exponent int64
	i := strings.IndexAny(mantissa, "eE")
	if i >= 0 {
		exponent = parseExponent(mantissa[i+1:])
		mantissa = mantissa[:i]
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")

	digits = strings.TrimLeft(whole+fraction, "0")
	trimmed := strings.TrimRight(digits, "0")
	scale = exponent - int64(len(fraction)) + int64(len(digits)-len(trimmed))
	return sign, trimmed, scale
}

// multipleOfChecks reports whether multipleOf, checked on n, a text
// CanonicalNumber returned, gives the answer it gives on the number n was
// made from. It does when n has at most MaxExactNumber significant digits
// and is under 10^309 in magnitude, as n is then that number, and for
// 1e-325, which stands for a number closer to 0 than every multipleOf a
// document can state, a positive float64: neither is a multiple of any of
// them. It does not for the first MaxExactNumber digits and a 5
// of a longer number, nor for 1e309 or 10^309 and a half, which do not hold
// the remaining digits of the number they stand for.
func multipleOfChecks(n json.Number) bool {
	s := string(n)
	if len(s) <= MaxExactNumber && !strings.ContainsAny(s, "eE") {
		return true
	}
	_, digits, scale := splitNumber(s)
	lead := scale + int64(len(digits)) - 1
	return digits == "" || (len(digits) <= MaxExactNumber && lead < 309)
}

// parseExponent reads the exponent of a JSON number, an optional sign and
// digits, holding its magnitude at maxExponent.
func parseExponent(s string) int64 {
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimLeft(s, "+-")
	var n int64
	for i := 0; i < len(s) && n < maxExponent; i++ {
		n = n*10 + int64(s[i]-'0')
	}
	n = min(n, maxExponent)
	if negative {
		return -n
	}
	return n
}

func exponentText(scale int64) string {
	if scale == 0 {
		return ""
	}
	return "e" + strconv.FormatInt(scale, 10)
}

// multipleOfVocabulary adds to every schema with multipleOf the check
// multipleOfDigits makes beside santhosh's own.
var multipleOfVocabulary = &jsonschema.Vocabulary{
	URL:     baseURL + "multipleOf-digits",
	Compile: compileMultipleOfDigits,
}

func compileMultipleOfDigits(ctx *jsonschema.CompilerContext, obj map[string]any) (jsonschema.SchemaExt, error) {
	value, ok := obj["multipleOf"].(json.Number)
	if !ok {
		return nil, nil
	}
	want, ok := new(big.Rat).SetString(string(value))
	if !ok {
		return nil, nil
	}
	return &multipleOfDigits{want: want}, nil
}

// multipleOfDigits refuses a number on which multipleOf cannot be checked
// (see multipleOfChecks), whichever answer santhosh's multipleOf gave on
// its text. Every integer is a multiple of 1 and of any 1/k, and the text is
// an integer exactly when the number is, so the text gives the true answer
// under multipleOf 1 for any number, and under 1/k for an integer: those are
// left to santhosh's multipleOf. Only a schema with multipleOf has it, so no
// other number pays for the check.
type multipleOfDigits struct {
	want *big.Rat
}

func (d *multipleOfDigits) Validate(ctx *jsonschema.ValidatorContext, v any) {
	n, ok := v.(json.Number)
	if !ok || multipleOfChecks(n) {
		return
	}
	unitFraction := d.want.Num().IsInt64() && d.want.Num().Int64() == 1
	if unitFraction && (d.want.IsInt() || isInteger(n)) {
		return
	}
	ctx.AddError(&tooManyDigits{Want: d.want})
}

// isInteger reports whether n, a text CanonicalNumber returned, is an
// integer.
func isInteger(n json.Number) bool {
	_, digits, scale := splitNumber(string(n))
	return digits == "" || scale >= 0
}

// tooManyDigits is the error multipleOfDigits reports.
type tooManyDigits struct {
	Want *big.Rat
}

func (*tooManyDigits) KeywordPath() []string {
	return []string{"multipleOf"}
}

func (k *tooManyDigits) LocalizedString(p *localized.Printer) string {
	return p.Sprintf("multipleOf: too many digits to check against %v", k.Want.RatString())
}
