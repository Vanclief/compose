package random

import (
	"strings"
	"sync"
	"testing"
)

// TestConcurrentCallsReturnDistinctStrings calls both functions from many
// goroutines at once. With 32 characters the chance of any repeat among 1000
// strings is below 2^-80 even for digits, so a repeat means calls share a
// seed.
func TestConcurrentCallsReturnDistinctStrings(t *testing.T) {
	const calls = 1000
	const size = 32

	tests := []struct {
		name     string
		generate func(int) string
		chars    string
	}{
		{name: "String", generate: String, chars: "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"},
		{name: "NumbersString", generate: NumbersString, chars: "0123456789"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := make([]string, calls)

			var wg sync.WaitGroup
			for i := range results {
				wg.Go(func() {
					results[i] = tt.generate(size)
				})
			}
			wg.Wait()

			seen := map[string]bool{}
			for _, s := range results {
				if len(s) != size {
					t.Fatalf("expected %d characters, got %q", size, s)
				}
				for _, c := range s {
					if !strings.ContainsRune(tt.chars, c) {
						t.Fatalf("unexpected character %q in %q", c, s)
					}
				}
				if seen[s] {
					t.Fatalf("%q returned twice", s)
				}
				seen[s] = true
			}
		})
	}
}
