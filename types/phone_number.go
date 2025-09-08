package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/vanclief/ez"
)

type PhoneNumber string

var phoneRegex *regexp.Regexp

func init() {
	var err error
	// Anchored to avoid substring matches; pattern body is exactly your spec.
	phoneRegex, err = regexp.Compile(`^\+(9[976]\d|8[987530]\d|6[987]\d|5[90]\d|42\d|3[875]\d|2[98654321]\d|9[8543210]|8[6421]|6[6543210]|5[87654321]|4[987654310]|3[9643210]|2[70]|7|1)\d{10,14}$`)
	if err != nil {
		// No panic. Leave nil; validator will return EINTERNAL.
		phoneRegex = nil
	}
}

// NewPhoneNumber validates and returns a PhoneNumber.
func NewPhoneNumber(s string) (PhoneNumber, error) {
	const op = "validate.NewPhoneNumber"

	err := ValidatePhoneNumber(s)
	if err != nil {
		return "", ez.Wrap(op, err)
	}

	return PhoneNumber(s), nil
}

// Validate re-validates the value (useful after deserialization).
func (p PhoneNumber) Validate() error {
	const op = "validate.PhoneNumber.Validate"

	err := ValidatePhoneNumber(string(p))
	if err != nil {
		return ez.Wrap(op, err)
	}
	return nil
}

func (p PhoneNumber) String() string {
	return string(p)
}

// --- JSON/Text encoding ---

func (p PhoneNumber) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(p))
}

func (p *PhoneNumber) UnmarshalJSON(b []byte) error {
	const op = "validate.PhoneNumber.UnmarshalJSON"

	var s string
	err := json.Unmarshal(b, &s)
	if err != nil {
		return ez.New(op, ez.EINVALID, "invalid JSON for phone number", err)
	}

	v, err := NewPhoneNumber(s)
	if err != nil {
		return ez.Wrap(op, err)
	}

	*p = v
	return nil
}

func (p PhoneNumber) MarshalText() ([]byte, error) {
	return []byte(p), nil
}

func (p *PhoneNumber) UnmarshalText(text []byte) error {
	const op = "validate.PhoneNumber.UnmarshalText"

	v, err := NewPhoneNumber(string(text))
	if err != nil {
		return ez.Wrap(op, err)
	}

	*p = v
	return nil
}

// --- database/sql integration ---

func (p PhoneNumber) Value() (driver.Value, error) {
	return string(p), nil
}

func (p *PhoneNumber) Scan(value any) error {
	const op = "validate.PhoneNumber.Scan"

	if value == nil {
		return ez.New(op, ez.EINVALID, "phone number cannot be NULL", nil)
	}

	switch v := value.(type) {
	case string:
		pn, err := NewPhoneNumber(v)
		if err != nil {
			return ez.Wrap(op, err)
		}
		*p = pn
		return nil

	case []byte:
		pn, err := NewPhoneNumber(string(v))
		if err != nil {
			return ez.Wrap(op, err)
		}
		*p = pn
		return nil

	default:
		msg := fmt.Sprintf("unsupported Scan type %T for PhoneNumber", value)
		return ez.New(op, ez.EINVALID, msg, nil)
	}
}

// --- reusable validator ---

func ValidatePhoneNumber(s string) error {
	const op = "validate.ValidatePhoneNumber"

	if s == "" {
		return ez.New(op, ez.EINVALID, "phone number is empty", nil)
	}

	if phoneRegex == nil {
		return ez.New(op, ez.EINTERNAL, "phone validator not initialized", nil)
	}

	if !phoneRegex.MatchString(s) {
		msg := fmt.Sprintf("the phone number %s is invalid", s)
		return ez.New(op, ez.EINVALID, msg, nil)
	}

	return nil
}
