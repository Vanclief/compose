package types

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vanclief/ez"
	"golang.org/x/text/language"
)

// Language represents supported languages
type Language string

const (
	English Language = "en"
	Spanish Language = "es"
)

const DEFAULT_LOCALE = "en-US"

func (l Language) IsValid() bool {
	switch l {
	case English, Spanish:
		return true
	default:
		return false
	}
}

// Locale is a string type with validation
type Locale string

// NewLocaleString creates a validated locale string from a single BCP 47 tag
// ("es-MX") or a full Accept-Language header ("es-MX,es;q=0.9,en;q=0.8").
// Tags are tried in the user's preference order: the first one with a
// supported language wins, keeping its region when it has an explicit one.
// Unsupported or malformed input falls back to DEFAULT_LOCALE instead of
// returning an error.
func NewLocaleString(locale string) (Locale, error) {
	locale = strings.TrimSpace(locale)
	if locale == "" {
		return DEFAULT_LOCALE, nil
	}

	tags, _, err := language.ParseAcceptLanguage(locale)
	if err != nil {
		return DEFAULT_LOCALE, nil
	}

	for _, tag := range tags {
		base, _ := tag.Base()

		lang := Language(base.String())
		if !lang.IsValid() {
			continue
		}

		// Only keep regions the user actually sent, not ones x/text inferred
		region, confidence := tag.Region()
		if confidence == language.Exact {
			return Locale(fmt.Sprintf("%s-%s", lang, region)), nil
		}

		switch lang {
		case English:
			return Locale("en-US"), nil
		case Spanish:
			return Locale("es-MX"), nil
		}
	}

	return DEFAULT_LOCALE, nil
}

// Language returns the language part of the locale
func (l Locale) Language() Language {
	parts := strings.Split(string(l), "-")
	return Language(parts[0])
}

// Region returns the region part of the locale
func (l Locale) Region() string {
	parts := strings.Split(string(l), "-")
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

// String returns the locale string
func (l Locale) String() string {
	return string(l)
}

// MarshalJSON implements json.Marshaler
func (l Locale) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(l))
}

// UnmarshalJSON implements json.Unmarshaler
func (l *Locale) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return ez.Wrap(err)
	}

	validated, err := NewLocaleString(str)
	if err != nil {
		return ez.Wrap(err)
	}

	*l = validated
	return nil
}
