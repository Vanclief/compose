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
	const op = "enums.Validate"

	if _, ok := allowed[value]; ok {
		return nil
	}

	errMsg := fmt.Sprintf("invalid enum value: %q, should be one of %v", value, keys(allowed))
	return ez.New(op, ez.EINVALID, errMsg, nil)
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
	const op = "enums.Marshal"

	err := Validate(value, allowed)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return json.Marshal(string(value))
}

func Unmarshal[Enum ~string](b []byte, out *Enum, allowed map[Enum]struct{}) error {
	const op = "enums.Unmarshal"

	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	value := Enum(s)
	err := Validate(value, allowed)
	if err != nil {
		return ez.Wrap(op, err)
	}

	*out = value
	return nil
}

func Value[Enum ~string](value Enum, allowed map[Enum]struct{}) (driver.Value, error) {
	const op = "enums.Value"

	err := Validate(value, allowed)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}
	return string(value), nil
}

func Scan[Enum ~string](src any, out *Enum, allowed map[Enum]struct{}) error {
	const op = "enums.Scan"

	switch x := src.(type) {
	case string:
		value := Enum(x)

		err := Validate(value, allowed)
		if err != nil {
			return ez.Wrap(op, err)
		}

		*out = value
		return nil
	case []byte:
		s := string(x)
		value := Enum(s)

		err := Validate(value, allowed)
		if err != nil {
			return ez.Wrap(op, err)
		}

		*out = value
		return nil
	default:
		errMsg := fmt.Sprintf("unsupported SQL type: %T", src)
		return ez.New(op, ez.EINVALID, errMsg, nil)
	}
}
