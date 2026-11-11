package random

import (
	"math/rand"
)

// String returns a pseudo-randomly generated string which should not be used
// for cryptographic purposes
func String(size int) string {
	const characters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	return generateRandomString(size, characters)
}

// NumbersString returns a pseudo-randomly generated string of digits which
// should not be used for cryptographic purposes
func NumbersString(size int) string {
	const characters = "0123456789"

	return generateRandomString(size, characters)
}

// generateRandomString draws from math/rand's top-level source, which Go seeds
// randomly at startup and which is safe for concurrent use, so calls made at
// the same moment do not repeat each other.
func generateRandomString(size int, chars string) string {
	str := make([]byte, size)
	for i := 0; i < size; i++ {
		str[i] = chars[rand.Intn(len(chars))]
	}

	return string(str)
}
