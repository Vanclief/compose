package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/mail"

	"github.com/vanclief/ez"
)

type Email string

// NewEmail validates and returns an Email.
func NewEmail(s string) (Email, error) {
	err := validateEmail(s)
	if err != nil {
		return "", ez.Wrap(err)
	}

	return Email(s), nil
}

// Validate re-validates the value (useful after deserialization).
func (e Email) Validate() error {
	err := validateEmail(string(e))
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

func (e Email) String() string {
	return string(e)
}

// --- JSON/Text encoding ---

func (e Email) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(e))
}

func (e *Email) UnmarshalJSON(b []byte) error {
	var s string
	err := json.Unmarshal(b, &s)
	if err != nil {
		return ez.New(ez.EINVALID, "invalid JSON for email", err)
	}

	v, err := NewEmail(s)
	if err != nil {
		return ez.Wrap(err)
	}

	*e = v
	return nil
}

// Satisfy encoding.TextMarshaler / TextUnmarshaler

func (e Email) MarshalText() ([]byte, error) {
	return []byte(e), nil
}

func (e *Email) UnmarshalText(text []byte) error {
	v, err := NewEmail(string(text))
	if err != nil {
		return ez.Wrap(err)
	}

	*e = v
	return nil
}

// --- database/sql integration ---

func (e Email) Value() (driver.Value, error) {
	return string(e), nil
}

func (e *Email) Scan(value any) error {
	if value == nil {
		return ez.New(ez.EINVALID, "email cannot be NULL", nil)
	}

	switch v := value.(type) {
	case string:
		ev, err := NewEmail(v)
		if err != nil {
			return ez.Wrap(err)
		}
		*e = ev
		return nil

	case []byte:
		ev, err := NewEmail(string(v))
		if err != nil {
			return ez.Wrap(err)
		}
		*e = ev
		return nil

	default:
		msg := fmt.Sprintf("unsupported Scan type %T for Email", value)
		return ez.New(ez.EINVALID, msg, nil)
	}
}

func validateEmail(s string) error {
	if s == "" {
		return ez.New(ez.EINVALID, "email is empty", nil)
	}

	addr, err := mail.ParseAddress(s)
	if err != nil {
		return ez.New(ez.EINVALID, "invalid email format", err)
	}

	// Reject display-name formats; accept only addr-spec.
	if addr.Address != s {
		return ez.New(ez.EINVALID, "email must be in addr-spec format", nil)
	}

	return nil
}
