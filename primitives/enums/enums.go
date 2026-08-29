// Package enums validates string-backed enums and adapts them to JSON and
// database/sql.
//
// An enum that declares "" as a member is nullable: "" is its unset value,
// stored as SQL NULL by Value and read back from NULL by Scan, accepted as ""
// or null by Unmarshal, and emitted as "" by Marshal. Its database column must
// allow NULL. An enum that does not declare "" rejects both "" and NULL
// everywhere.
package enums

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/vanclief/ez"
)

func Set[Enum ~string](vals []Enum) map[Enum]struct{} {
	set := make(map[Enum]struct{}, len(vals))
	for _, value := range vals {
		set[value] = struct{}{}
	}
	return set
}

func Validate[Enum ~string](value Enum, allowed map[Enum]struct{}) error {
	if _, ok := allowed[value]; ok {
		return nil
	}

	errMsg := fmt.Sprintf("invalid enum value: %q, should be one of %v", value, keys(allowed))
	return ez.New(ez.EINVALID, errMsg, nil)
}

// tiny helper to print allowed values deterministically
func keys[Enum ~string](m map[Enum]struct{}) []string {
	out := make([]string, 0, len(m))
	for v := range m {
		out = append(out, string(v))
	}
	sort.Strings(out)
	return out
}

func Marshal[Enum ~string](value Enum, allowed map[Enum]struct{}) ([]byte, error) {
	err := Validate(value, allowed)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return json.Marshal(string(value))
}

func Unmarshal[Enum ~string](b []byte, out *Enum, allowed map[Enum]struct{}) error {
	var s string
	err := json.Unmarshal(b, &s)
	if err != nil {
		return ez.New(ez.EINVALID, "invalid JSON for enum", err)
	}

	value := Enum(s)
	err = Validate(value, allowed)
	if err != nil {
		return ez.Wrap(err)
	}

	*out = value
	return nil
}

// Value implements the database/sql/driver Valuer contract for string-backed enums.
// The zero value "" is written as SQL NULL (the inverse of Scan) and is accepted only if allowed contains "".
func Value[Enum ~string](value Enum, allowed map[Enum]struct{}) (driver.Value, error) {
	err := Validate(value, allowed)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	if value == "" {
		return nil, nil
	}

	return string(value), nil
}

// Scan implements the database/sql Scanner contract for string-backed enums.
// A SQL NULL (src == nil) scans as the zero value "" and is accepted only if allowed contains "".
func Scan[Enum ~string](src any, out *Enum, allowed map[Enum]struct{}) error {
	var s string
	switch x := src.(type) {
	case nil:
		s = ""
	case string:
		s = x
	case []byte:
		s = string(x)
	default:
		errMsg := fmt.Sprintf("unsupported SQL type: %T", src)
		return ez.New(ez.EINVALID, errMsg, nil)
	}

	value := Enum(s)
	err := Validate(value, allowed)
	if err != nil {
		return ez.Wrap(err)
	}

	*out = value
	return nil
}
