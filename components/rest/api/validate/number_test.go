package validate

import (
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalNumber(t *testing.T) {
	ones := strings.Repeat("1", 64)
	cases := []struct {
		in, want string
	}{
		// Short and without an exponent: as written.
		{"0", "0"},
		{"-1.50", "-1.50"},
		{"1" + strings.Repeat("0", 63), "1" + strings.Repeat("0", 63)},
		// The exact value, as digits and an exponent.
		{"1e3", "1e3"},
		{"-2.50E+3", "-25e2"},
		{"0.000e7", "0"},
		{"-0.0", "-0.0"},
		{"1.0000000000000000001e0", "10000000000000000001e-19"},
		{"1" + strings.Repeat("0", 64), "1e64"},
		{"0." + strings.Repeat("9", 64), strings.Repeat("9", 64) + "e-64"},
		{"1.7976931348623157e308", "17976931348623157e292"},
		{"9.99e308", "999e306"},
		{"5e-324", "5e-324"},
		{"1e-324", "1e-324"},
		// More significant digits: the first 64 and a 5.
		{ones + "1", ones + "5"},
		{"1" + strings.Repeat("0", 63) + "1", "1" + strings.Repeat("0", 63) + "5"},
		{ones + "1e10", ones + "5e10"},
		{"-1." + strings.Repeat("0", 64) + "1", "-1" + strings.Repeat("0", 63) + "5e-64"},
		{"0." + strings.Repeat("9", 70), strings.Repeat("9", 64) + "5e-65"},
		// Not an integer, though its first 64 digits and a 5 would be one.
		{ones + "11.5", ones + "5" + "0" + "5e-1"},
		{ones + "1.5", ones + "55e-1"},
		// Past the float64 range, or closer to 0 than any float64.
		{"1e309", "1e309"},
		{"-1e999999", "-1e309"},
		{"1e99999999999999999999999999", "1e309"},
		{"1" + strings.Repeat("0", 400) + ".5", "1" + strings.Repeat("0", 309) + ".5"},
		{"1e-325", "1e-325"},
		{"-1e-999999", "-1e-325"},
		{"1e-99999999999999999999999999", "1e-325"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, string(CanonicalNumber(c.in)), c.in)
	}
}

// The text CanonicalNumber gives a number compares with every number of up
// to 64 significant digits as the number does, and is an integer when the
// number is: checked against the numbers next to it, which are the ones a
// rounded value gets wrong.
func TestCanonicalNumberKeepsComparisons(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	for range 3000 {
		// The number is sign digits × 10^scale.
		var b strings.Builder
		b.WriteByte('1' + byte(random.IntN(9)))
		for range random.IntN(100) {
			b.WriteByte('0' + byte(random.IntN(10)))
		}
		digits := b.String()
		scale := random.IntN(240) - 150
		sign := ""
		if random.IntN(2) == 0 {
			sign = "-"
		}

		// Written with the point after its first point digits, and an
		// exponent unless that is 0 and a coin says to leave it out.
		point := 1 + random.IntN(len(digits))
		in := sign + digits[:point]
		if point < len(digits) {
			in += "." + digits[point:]
		}
		exponent := scale + len(digits) - point
		if exponent != 0 || random.IntN(2) == 0 {
			in += "e" + strconv.Itoa(exponent)
		}

		exact, ok := new(big.Rat).SetString(sign + digits + "e" + strconv.Itoa(scale))
		require.True(t, ok, in)
		got, ok := new(big.Rat).SetString(string(CanonicalNumber(in)))
		require.True(t, ok, in)
		require.Equal(t, exact.IsInt(), got.IsInt(), in)

		// The number cut to k significant digits and the next number of k
		// digits, either side of it; the nearest float64 in its shortest
		// text.
		f, _ := exact.Float64()
		near := []string{strconv.FormatFloat(f, 'g', -1, 64)}
		for k := 1; k <= min(64, len(digits)); k++ {
			cut, ok := new(big.Int).SetString(digits[:k], 10)
			require.True(t, ok)
			at := "e" + strconv.Itoa(scale+len(digits)-k)
			near = append(near, sign+cut.String()+at, sign+cut.Add(cut, big.NewInt(1)).String()+at)
		}
		for _, bound := range near {
			b, ok := new(big.Rat).SetString(bound)
			require.True(t, ok, bound)
			require.Equal(t, exact.Cmp(b), got.Cmp(b), "%s against %s", in, bound)
		}
	}
}

// A body of one long number or one long exponent costs time and memory in
// proportion to its length.
func TestCanonicalNumberCost(t *testing.T) {
	long := "1" + strings.Repeat("7", 1<<20)
	exponent := "1e" + strings.Repeat("9", 1<<20)
	for _, in := range []string{long, exponent, "-" + long + ".5", "0." + long} {
		allocs := testing.AllocsPerRun(5, func() {
			CanonicalNumber(in)
		})
		require.Less(t, allocs, 10.0)
	}
	require.Equal(t, "1e309", string(CanonicalNumber(long)))
	require.Equal(t, "1"+strings.Repeat("7", 63)+"5e-65", string(CanonicalNumber("0."+long)))
}
