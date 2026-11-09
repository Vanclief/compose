package types

import (
	"encoding/json"
	"reflect"

	"github.com/vanclief/ez"
)

// Optional is a request body field that records whether the key was
// present. Absent: Set is false. null: Set and Null are true. A value:
// Set is true and Value holds it. Body fields only, never a parameter.
//
// The api reflector documents Optional[T] as T's schema made nullable,
// through the OptionalType method.
type Optional[T any] struct {
	Value T
	Set   bool
	Null  bool
}

// Some returns an Optional holding v.
func Some[T any](v T) Optional[T] {
	return Optional[T]{Value: v, Set: true}
}

// Null returns an Optional set to null.
func Null[T any]() Optional[T] {
	return Optional[T]{Set: true, Null: true}
}

// Get returns Value and whether a non-null value was set.
func (o Optional[T]) Get() (T, bool) {
	return o.Value, o.Set && !o.Null
}

// IsZero reports whether the key was absent, so a field tagged omitzero is
// dropped when marshalling.
func (o Optional[T]) IsZero() bool {
	return !o.Set
}

// MarshalJSON writes null when the Optional is unset or null, else Value.
func (o Optional[T]) MarshalJSON() ([]byte, error) {
	if !o.Set || o.Null {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}

// UnmarshalJSON records the key as present: null sets Null, anything else
// decodes into Value. encoding/json only calls it for keys in the body, so
// an absent key leaves Set false. A decode error is EINVALID, like the other
// types in this package.
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*o = Optional[T]{Set: true, Null: true}
		return nil
	}

	var v T
	err := json.Unmarshal(b, &v)
	if err != nil {
		return ez.New(ez.EINVALID, "invalid JSON for the optional value", err)
	}

	*o = Optional[T]{Value: v, Set: true}
	return nil
}

// OptionalType returns T. The api reflector finds Optional by this method,
// so this package does not depend on the api packages.
func (Optional[T]) OptionalType() reflect.Type {
	return reflect.TypeFor[T]()
}
