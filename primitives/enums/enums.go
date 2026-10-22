// Package enums validates string-backed enums and adapts them to JSON and
// database/sql.
//
// An enum that declares "" as a member treats "" as its unset value: Scan
// reads SQL NULL as "", Unmarshal reads JSON null as "", and Value and Marshal
// write "" as the empty string. A non-pointer enum that does not declare ""
// rejects "", SQL NULL and JSON null; a *Enum field takes NULL and null as nil
// without calling Scan or Unmarshal.
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
		// Returned bare: encoding/json adds the struct field only to a bare
		// *json.UnmarshalTypeError, and the REST binder logs that detail and
		// answers a generic "Invalid request body"
		return err
	}

	value := Enum(s)
	err = Validate(value, allowed)
	if err != nil {
		return ez.Wrap(err)
	}

	*out = value
	return nil
}

func Value[Enum ~string](value Enum, allowed map[Enum]struct{}) (driver.Value, error) {
	err := Validate(value, allowed)
	if err != nil {
		return nil, ez.Wrap(err)
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
