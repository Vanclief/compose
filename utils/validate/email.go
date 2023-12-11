package validate

import (
	"fmt"
	"regexp"

	"github.com/vanclief/ez"
)

var emailRegex = regexp.MustCompile("^[a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$")

func Email(email string) error {
	const op = "validate.Email"

	if !emailRegex.MatchString(email) {
		msg := fmt.Sprintf("The email %s is invalid", email)
		return ez.New(op, ez.EINVALID, msg, nil)
	}

	return nil
}
