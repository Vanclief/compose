package enums

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/vanclief/ez"
)

// registry keeps every enum's allowed values by Go type, in declaration order,
// so the OpenAPI reflector can emit one named enum component per Go type.
var registry = struct {
	sync.RWMutex
	values map[reflect.Type][]string
}{values: map[reflect.Type][]string{}}

// Set builds the allowed-values set for an enum type and records the values
// by type for OpenAPI generation. Calling it more than once for the same type
// (for example a full set and a request-only subset) records the union in
// first-seen order, so the published enum component always lists every value
// the type can carry. Narrow a single field with the `enum:"a,b"` struct tag.
func Set[Enum ~string](vals []Enum) map[Enum]struct{} {
	set := make(map[Enum]struct{}, len(vals))
	for _, value := range vals {
		set[value] = struct{}{}
	}

	t := reflect.TypeFor[Enum]()

	registry.Lock()
	defer registry.Unlock()

	seen := make(map[string]bool, len(registry.values[t]))
	for _, value := range registry.values[t] {
		seen[value] = true
	}
	for _, value := range vals {
		if seen[string(value)] {
			continue
		}
		seen[string(value)] = true
		registry.values[t] = append(registry.values[t], string(value))
	}

	return set
}

// Values returns the allowed values recorded by Set for the given type, in
// declaration order, and whether the type is a registered enum.
func Values(t reflect.Type) ([]string, bool) {
	registry.RLock()
	values, ok := registry.values[t]
	registry.RUnlock()
	if !ok {
		return nil, false
	}

	out := make([]string, len(values))
	copy(out, values)
	return out, true
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
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	value := Enum(s)
	err := Validate(value, allowed)
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

func Scan[Enum ~string](src any, out *Enum, allowed map[Enum]struct{}) error {
	switch x := src.(type) {
	case string:
		value := Enum(x)

		err := Validate(value, allowed)
		if err != nil {
			return ez.Wrap(err)
		}

		*out = value
		return nil
	case []byte:
		s := string(x)
		value := Enum(s)

		err := Validate(value, allowed)
		if err != nil {
			return ez.Wrap(err)
		}

		*out = value
		return nil
	default:
		errMsg := fmt.Sprintf("unsupported SQL type: %T", src)
		return ez.New(ez.EINVALID, errMsg, nil)
	}
}
