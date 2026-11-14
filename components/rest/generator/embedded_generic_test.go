package generator

import (
	"reflect"
	"strings"
	"testing"
)

type GenList[T any] []T

type GenDict[K comparable, V any] map[K]V

type GenNum[T any] int

type GenStruct[T any] struct {
	V T `json:"v"`
}

type GenIndex map[string]int

type genIndexHolder struct {
	GenIndex
	X int `json:"x"`
}

type genListHolder struct {
	GenList[int]
	X int `json:"x"`
}

type genDictHolder struct {
	GenDict[string, int]
	X int `json:"x"`
}

type genNumHolder struct {
	GenNum[string]
	X int `json:"x"`
}

type genPointerHolder struct {
	*GenList[int]
	X int `json:"x"`
}

type genStructHolder struct {
	GenStruct[int]
	X int `json:"x"`
}

const embeddedGenericSource = `package things

type List[T any] []T

type Dict[K comparable, V any] map[K]V

type Num[T any] int

type Struct[T any] struct {
	V T ` + "`json:\"v\"`" + `
}

type Index map[string]int

type IndexHolder struct {
	Index
	X int ` + "`json:\"x\"`" + `
}

type ListHolder struct {
	List[int]
	X int ` + "`json:\"x\"`" + `
}

type DictHolder struct {
	Dict[string, int]
	X int ` + "`json:\"x\"`" + `
}

type NumHolder struct {
	Num[string]
	X int ` + "`json:\"x\"`" + `
}

type PointerHolder struct {
	*List[int]
	X int ` + "`json:\"x\"`" + `
}

type StructHolder struct {
	Struct[int]
	X int ` + "`json:\"x\"`" + `
}
`

// encoding/json writes an embedded instantiation of a generic type that is not a struct as a
// field named after the generic type, as it writes any embedded non-struct, and promotes the
// fields of one that is a struct. A type described in Config.TypeSchemas is expanded by the kind
// its declaration has, not the one its description says: a map described as an object is a field.
func TestEmbeddedGenericNonStructsAreFields(t *testing.T) {
	described := map[string]map[string]any{
		"example.com/app/things.List":   {"type": "array", "items": map[string]any{"type": "integer"}, "nullable": true},
		"example.com/app/things.Dict":   {"type": "object", "additionalProperties": map[string]any{"type": "integer"}, "nullable": true},
		"example.com/app/things.Num":    {"type": "integer"},
		"example.com/app/things.Struct": {"type": "object", "properties": map[string]any{"v": map[string]any{}}},
		"example.com/app/things.Index":  {"type": "object", "additionalProperties": map[string]any{"type": "integer"}, "nullable": true},
	}
	cases := []struct {
		name   string
		values []any
		writes any
		field  string
	}{
		{"ListHolder", []any{&genListHolder{}, &genListHolder{GenList: GenList[int]{1}, X: 2}}, map[string]any{"GenList": nil, "x": float64(0)}, "List"},
		{"DictHolder", []any{&genDictHolder{}, &genDictHolder{GenDict: GenDict[string, int]{"a": 1}}}, map[string]any{"GenDict": nil, "x": float64(0)}, "Dict"},
		{"NumHolder", []any{&genNumHolder{GenNum: 3}}, map[string]any{"GenNum": float64(3), "x": float64(0)}, "Num"},
		{"PointerHolder", []any{&genPointerHolder{}, &genPointerHolder{GenList: &GenList[int]{1}}}, map[string]any{"GenList": nil, "x": float64(0)}, "List"},
		{"IndexHolder", []any{&genIndexHolder{}, &genIndexHolder{GenIndex: GenIndex{"a": 1}}}, map[string]any{"GenIndex": nil, "x": float64(0)}, "Index"},
		{"StructHolder", []any{&genStructHolder{GenStruct: GenStruct[int]{V: 1}}}, map[string]any{"v": float64(1), "x": float64(0)}, ""},
	}
	for _, c := range cases {
		written := marshalled(t, c.values[0])
		if !reflect.DeepEqual(written, c.writes) {
			t.Fatalf("%s: expected encoding/json to write %v, got %v", c.name, c.writes, written)
		}

		builder := buildDescribed(t, embeddedGenericSource, described, c.name)
		err := builder.unresolvedError()
		if err != nil {
			t.Errorf("%s: expected its schema, got %v", c.name, err)
			continue
		}
		schema := builder.schemas["things_"+c.name]
		if c.field != "" {
			properties, _ := schema["properties"].(map[string]any)
			if properties[c.field] == nil || schema["allOf"] != nil {
				t.Errorf("%s: expected a property %s, got %v", c.name, c.field, schema)
			}
		}
		for _, value := range c.values {
			written := marshalled(t, value).(map[string]any)
			// The Go types here are named Gen..., the source's without the prefix.
			for key, field := range written {
				name, renamed := strings.CutPrefix(key, "Gen")
				if renamed {
					delete(written, key)
					written[name] = field
				}
			}
			assertConforms(t, builder, schema, written)
		}
	}

	undescribed := buildThings(t, embeddedGenericSource, "ListHolder")
	err := undescribed.unresolvedError()
	if err == nil || !strings.Contains(err.Error(), "generic instantiation List[int] is not supported; describe it with Config.TypeSchemas") {
		t.Errorf("expected List[int] to need a description, got %v", err)
	}
}
