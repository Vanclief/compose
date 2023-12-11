package validate

import (
	"fmt"
	"regexp"

	"github.com/vanclief/ez"
)

var phoneRegex = regexp.MustCompile("\\+(9[976]\\d|8[987530]\\d|6[987]\\d|5[90]\\d|42\\d|3[875]\\d|2[98654321]\\d|9[8543210]|8[6421]|6[6543210]|5[87654321]|4[987654310]|3[9643210]|2[70]|7|1)\\d{10,14}$")

// PhoneNumber checks if a phone number is valid
func PhoneNumber(number string) error {
	const op = "validate.PhoneNumber"

	if !phoneRegex.MatchString(number) {
		msg := fmt.Sprintf("The phone number %s is invalid", number)
		return ez.New(op, ez.EINVALID, msg, nil)
	}

	return nil
}
