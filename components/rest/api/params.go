package api

import (
	"encoding"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/api/validate"
)

var (
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	timeType            = reflect.TypeFor[time.Time]()
	bigIntType          = reflect.TypeFor[big.Int]()
)

// kindOf derives the coercion kind of a parameter schema, resolving $refs
// (enum components are strings).
func kindOf(s *huma.Schema, comps map[string]*huma.Schema) validate.Kind {
	for depth := 0; s != nil && s.Ref != "" && depth < 16; depth++ {
		s = comps[strings.TrimPrefix(s.Ref, openapi.RefPrefix)]
	}
	if s == nil {
		return validate.Kind{Type: "string"}
	}

	switch s.Type {
	case huma.TypeInteger, huma.TypeNumber, huma.TypeBoolean:
		return validate.Kind{Type: s.Type}
	case huma.TypeArray:
		items := kindOf(s.Items, comps)
		return validate.Kind{Type: "array", Items: &items}
	}
	return validate.Kind{Type: "string"}
}

// splitValues mirrors validate.Coerce for arrays: repeated keys, or one
// comma-separated value.
func splitValues(values []string) []string {
	if len(values) == 1 {
		return strings.Split(values[0], ",")
	}
	return values
}

// setParams stores the raw parameter values into req. Every conversion error
// is an ez error classified by its code. EINTERNAL (a Go type
// checkParamTypes should have rejected at registration) is the server's and
// returned as EINTERNAL. Any other failure comes from client input the
// schema could not rule out (a date-time the schema accepts but time.Time
// rejects, an integer out of the field's range, text a TextUnmarshaler
// refuses, whatever error it returns) and becomes one EINVALID naming each
// parameter, never echoing the value.
func setParams(req reflect.Value, params []openapi.Param, values [][]string) error {
	var fields []validate.FieldError
	for i, p := range params {
		if len(values[i]) == 0 {
			continue
		}
		field := fieldOf(req, p.Index)

		var err error
		if p.In == "header" && derefType(field.Type()) == timeType {
			err = setHTTPTime(field, values[i][0])
		} else {
			err = setValue(field, values[i])
		}
		if ez.ErrorCode(err) == ez.EINTERNAL {
			return ez.New(ez.EINTERNAL, fmt.Sprintf("could not set %s parameter %s", p.In, p.Name), err)
		}
		if err != nil {
			fields = append(fields, validate.FieldError{In: p.In, Name: p.Name, Message: "is invalid"})
		}
	}
	return validate.NewFieldsError(fields)
}

// setHTTPTime parses a time.Time header as an HTTP-date, the
// date-time-http format the spec documents for it.
func setHTTPTime(v reflect.Value, s string) error {
	t, err := http.ParseTime(s)
	if err != nil {
		return ez.New(ez.EINVALID, "not an HTTP-date", err)
	}
	if v.Kind() == reflect.Pointer {
		v.Set(reflect.ValueOf(&t))
		return nil
	}
	v.Set(reflect.ValueOf(t))
	return nil
}

// fieldOf returns the field at index, allocating embedded pointers on the
// way (serve has allocated them, see embeddedPointers; registration refuses
// a parameter inside one with a JSON name); a pointer field at index itself
// stays as it is.
func fieldOf(v reflect.Value, index []int) reflect.Value {
	for _, idx := range index {
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				v.Set(reflect.New(v.Type().Elem()))
			}
			v = v.Elem()
		}
		v = v.Field(idx)
	}
	return v
}

// checkParamTypes rejects parameter fields setValue cannot fill from text:
// []byte, maps, arrays, nested slices and structs that do not implement
// encoding.TextUnmarshaler; and parameters reached through an unexported
// embedded pointer, which setParams cannot allocate. It also rejects a slice
// of pointers: the document would publish its items as nullable, which no
// parameter can carry; and a slice type with a JSON decoder of its own and
// no text decoder, which setValue would fill item by item without running
// it.
func checkParamTypes(reqType reflect.Type, params []openapi.Param) error {
	var problems []string
	for _, p := range params {
		embedded := unexportedEmbeddedPointer(reqType, p.Index)
		if embedded != nil {
			problems = append(problems, fmt.Sprintf(
				"%s parameter %s: reached through the unexported embedded pointer %s, which cannot be allocated (export the type or embed it by value)",
				p.In, p.Name, embedded))
			continue
		}
		t := reqType.FieldByIndex(p.Index).Type
		slice := derefType(t)
		if slice.Kind() == reflect.Slice && !isTextUnmarshaler(slice) && slice.Elem().Kind() == reflect.Pointer {
			problems = append(problems, fmt.Sprintf(
				"%s parameter %s: type %s is a slice of pointers, which is not supported: a parameter item cannot be null (use a slice of values)",
				p.In, p.Name, t))
			continue
		}
		if slice.Kind() == reflect.Slice && !isTextUnmarshaler(slice) && reflect.PointerTo(slice).Implements(jsonUnmarshalerType) {
			problems = append(problems, fmt.Sprintf(
				"%s parameter %s: type %s is a slice with its own UnmarshalJSON, which a parameter's items would not go through (give it an UnmarshalText, or use a plain slice)",
				p.In, p.Name, t))
			continue
		}
		if !paramTypeSupported(t) {
			problems = append(problems, fmt.Sprintf(
				"%s parameter %s: type %s is not supported (use a string, bool or number kind, an encoding.TextUnmarshaler, or a slice of those)",
				p.In, p.Name, t))
		}
	}
	if len(problems) > 0 {
		return ez.New(ez.EINVALID, strings.Join(problems, "; "), nil)
	}
	return nil
}

// unexportedEmbeddedPointer returns the type of the first unexported
// embedded pointer on the way to the field of t at index, or nil.
func unexportedEmbeddedPointer(t reflect.Type, index []int) reflect.Type {
	for _, idx := range index[:len(index)-1] {
		f := derefType(t).Field(idx)
		if f.Type.Kind() == reflect.Pointer && !f.IsExported() {
			return f.Type
		}
		t = f.Type
	}
	return nil
}

func paramTypeSupported(t reflect.Type) bool {
	t = derefType(t)
	if isTextUnmarshaler(t) {
		return true
	}
	if t.Kind() != reflect.Slice {
		return isScalarKind(t.Kind())
	}

	elem := derefType(t.Elem())
	if isTextUnmarshaler(elem) {
		return true
	}
	return elem.Kind() != reflect.Uint8 && isScalarKind(elem.Kind())
}

func isTextUnmarshaler(t reflect.Type) bool {
	return reflect.PointerTo(t).Implements(textUnmarshalerType)
}

func isScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func setValue(v reflect.Value, values []string) error {
	if v.Kind() == reflect.Pointer {
		elem := reflect.New(v.Type().Elem())
		err := setValue(elem.Elem(), values)
		if err != nil {
			return ez.Wrap(err)
		}
		v.Set(elem)
		return nil
	}

	// In base 10, as validate.Coerce read it: big.Int's own text decoder
	// takes a leading 0 as octal, so 010 would validate as 10 and arrive as 8.
	if v.Type() == bigIntType {
		_, ok := v.Addr().Interface().(*big.Int).SetString(values[0], 10)
		if !ok {
			return ez.New(ez.EINVALID, "not an integer", nil)
		}
		return nil
	}

	// The app's decoder refuses client input, whatever error it returns.
	if isTextUnmarshaler(v.Type()) {
		err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(values[0]))
		if err != nil {
			return ez.New(ez.EINVALID, "refused by its text decoder", err)
		}
		return nil
	}

	// A scalar type with only a JSON decoder gets the value as the JSON
	// literal a body would carry, so the type refuses and normalises a
	// parameter as it does a body field: an app enum checks its members,
	// types.Locale normalises (it has no UnmarshalText, which would also
	// change how its map keys decode).
	if v.Kind() != reflect.Slice && reflect.PointerTo(v.Type()).Implements(jsonUnmarshalerType) {
		literal, err := jsonLiteral(v.Type(), values[0])
		if err != nil {
			return ez.Wrap(err)
		}
		err = v.Addr().Interface().(json.Unmarshaler).UnmarshalJSON(literal)
		if err != nil {
			return ez.New(ez.EINVALID, "refused by its JSON decoder", err)
		}
		return nil
	}

	if v.Kind() == reflect.Slice {
		items := splitValues(values)
		slice := reflect.MakeSlice(v.Type(), len(items), len(items))
		for i, item := range items {
			err := setValue(slice.Index(i), []string{item})
			if err != nil {
				return ez.Wrap(err)
			}
		}
		v.Set(slice)
		return nil
	}

	err := setScalar(v, values[0])
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// jsonLiteral returns s, the text of a parameter of the scalar type t, as
// the JSON literal of t's kind: a quoted string, or the boolean or number
// setScalar reads from s. A float keeps the client's text, as a body would
// hand it over: validate.Coerce took it only as a JSON number.
func jsonLiteral(t reflect.Type, s string) ([]byte, error) {
	plain := reflect.New(t).Elem()
	err := setScalar(plain, s)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	switch plain.Kind() {
	case reflect.Bool:
		return strconv.AppendBool(nil, plain.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(nil, plain.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.AppendUint(nil, plain.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return []byte(s), nil
	}

	literal, err := json.Marshal(plain.String())
	if err != nil {
		return nil, ez.New(ez.EINVALID, "not text JSON can carry", err)
	}
	return literal, nil
}

func setScalar(v reflect.Value, s string) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return ez.New(ez.EINVALID, "not a boolean", err)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return ez.New(ez.EINVALID, "not an integer in range", err)
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// ParseUint refuses the one leading + that ParseInt and
		// validate.Coerce accept.
		n, err := strconv.ParseUint(strings.TrimPrefix(s, "+"), 10, v.Type().Bits())
		if err != nil {
			return ez.New(ez.EINVALID, "not an unsigned integer in range", err)
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return ez.New(ez.EINVALID, "not a number in range", err)
		}
		v.SetFloat(f)
	default:
		// checkParamTypes rejects the type at registration: a bug, never
		// client input.
		return ez.New(ez.EINTERNAL, "unsupported parameter type "+v.Type().String(), nil)
	}
	return nil
}
