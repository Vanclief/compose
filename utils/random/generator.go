package random

import (
	"math/rand"
	"time"
)

// String returns a pseudo-randomly generated string which should not be used
// for cryptographic purposes
func String(size int) string {
	const characters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	return generateRandomString(size, characters)
}

// String returns a pseudo-randomly generated string of numbers which should not be used
// for cryptographic purposes
func NumbersString(size int) string {
	const characters = "0123456789"

	return generateRandomString(size, characters)
}

func generateRandomString(size int, chars string) string {
	source := rand.NewSource(time.Now().UnixNano())
	rng := rand.New(source)

	str := make([]byte, size)
	for i := 0; i < size; i++ {
		randomNumber := rng.Intn(len(chars))
		str[i] = chars[randomNumber]
	}

	return string(str)
}
