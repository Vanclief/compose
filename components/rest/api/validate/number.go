package validate

import (
	"encoding/json"
	"strconv"
	"strings"
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
// The text is an integer exactly when s is. multipleOf holds on it as on s
// for the first two forms only.
func CanonicalNumber(s string) json.Number {
	if len(s) <= MaxExactNumber && !strings.ContainsAny(s, "eE") {
		return json.Number(s)
	}

	sign := ""
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

	// s is digits × 10^scale, digits without leading or trailing zeros.
	digits := strings.TrimLeft(whole+fraction, "0")
	trimmed := strings.TrimRight(digits, "0")
	scale := exponent - int64(len(fraction)) + int64(len(digits)-len(trimmed))
	digits = trimmed
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
