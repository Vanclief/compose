package validate

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Kind describes the schema type a parameter is coerced to.
// Type is one of "string", "integer", "number", "boolean" or "array".
type Kind struct {
	Type  string
	Items *Kind
}

// Coerce converts raw parameter values to the JSON value the schema expects.
// Integers and numbers become json.Number, booleans accept exactly "true" and
// "false", arrays take repeated values or one comma-separated value. A value
// that does not parse is returned as the raw string so validation reports a
// type error. No values means absent and returns nil.
func Coerce(values []string, kind Kind) any {
	if len(values) == 0 {
		return nil
	}

	switch kind.Type {
	case "integer":
		_, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil {
			return values[0]
		}
		return json.Number(values[0])
	case "number":
		_, err := strconv.ParseFloat(values[0], 64)
		if err != nil || !json.Valid([]byte(values[0])) {
			return values[0]
		}
		return json.Number(values[0])
	case "boolean":
		switch values[0] {
		case "true":
			return true
		case "false":
			return false
		}
		return values[0]
	case "array":
		items := values
		if len(values) == 1 {
			items = strings.Split(values[0], ",")
		}
		itemKind := Kind{Type: "string"}
		if kind.Items != nil {
			itemKind = *kind.Items
		}
		out := make([]any, len(items))
		for i, item := range items {
			out[i] = Coerce([]string{item}, itemKind)
		}
		return out
	}

	return values[0]
}
