package api

import (
	"encoding"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/api/validate"
)

var (
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
	timeType            = reflect.TypeFor[time.Time]()
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

// errUnsupportedParam marks a Go type that checkParamType should have
// rejected at registration: a bug, never client input.
var errUnsupportedParam = errors.New("unsupported parameter type")

// setParams stores the raw parameter values into req. A conversion failure
// comes from client input the schema could not rule out (a date-time the
// schema accepts but time.Time rejects, an integer out of the field's range)
// and becomes one EINVALID naming each parameter, never echoing the value.
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
		if errors.Is(err, errUnsupportedParam) {
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
		return err
	}
	if v.Kind() == reflect.Pointer {
		v.Set(reflect.ValueOf(&t))
		return nil
	}
	v.Set(reflect.ValueOf(t))
	return nil
}

// fieldOf returns the field at index, allocating embedded pointers on the way.
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
// encoding.TextUnmarshaler.
func checkParamTypes(reqType reflect.Type, params []openapi.Param) error {
	var problems []string
	for _, p := range params {
		t := reqType.FieldByIndex(p.Index).Type
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
			return err
		}
		v.Set(elem)
		return nil
	}

	if isTextUnmarshaler(v.Type()) {
		return v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(values[0]))
	}

	if v.Kind() == reflect.Slice {
		items := splitValues(values)
		slice := reflect.MakeSlice(v.Type(), len(items), len(items))
		for i, item := range items {
			err := setValue(slice.Index(i), []string{item})
			if err != nil {
				return err
			}
		}
		v.Set(slice)
		return nil
	}

	return setScalar(v, values[0])
}

func setScalar(v reflect.Value, s string) error {
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("%w %s", errUnsupportedParam, v.Type())
	}
	return nil
}
