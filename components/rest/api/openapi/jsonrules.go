package openapi

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strconv"
	"sync"
)

// The rules of the encoding/json this binary is built with, asked of encoding/json itself. On
// encoding/json/v2, Go 1.27's default, it also writes a value with MarshalJSONTo and AppendText,
// reads one with UnmarshalJSONFrom, and reads the name of a json tag by other rules.

var (
	textAppenderType = reflect.TypeFor[encoding.TextAppender]()
	errorType        = reflect.TypeFor[error]()
)

// appendTextProbe tells whether encoding/json writes a value with its AppendText.
type appendTextProbe struct{}

func (appendTextProbe) AppendText(b []byte) ([]byte, error) {
	return append(b, "text"...), nil
}

// writesAppendText reports whether encoding/json writes a value with AppendText.
var writesAppendText = func() bool {
	data, err := json.Marshal(appendTextProbe{})
	return err == nil && string(data) == `"text"`
}()

// jsonStreamParam is the parameter of the method json.Number, or a pointer to one, declares as
// name: the *jsontext.Encoder of MarshalJSONTo or the *jsontext.Decoder of UnmarshalJSONFrom on
// encoding/json/v2, which json.Number has; nil where it does not have the method.
func jsonStreamParam(number reflect.Type, name string) reflect.Type {
	method, ok := number.MethodByName(name)
	if !ok || method.Type.NumIn() != 2 {
		return nil
	}
	return method.Type.In(1)
}

var (
	jsonEncoderType = jsonStreamParam(reflect.TypeFor[json.Number](), "MarshalJSONTo")
	jsonDecoderType = jsonStreamParam(reflect.TypeFor[*json.Number](), "UnmarshalJSONFrom")
)

// hasStreamMethod reports whether t declares the method name taking param and returning an
// error, as encoding/json/v2's MarshalerTo and UnmarshalerFrom do. Always false where param is
// nil.
func hasStreamMethod(t reflect.Type, name string, param reflect.Type) bool {
	if param == nil {
		return false
	}
	method, ok := t.MethodByName(name)
	if !ok {
		return false
	}
	fn := method.Type
	in := 1
	if t.Kind() != reflect.Interface {
		// A concrete type's method takes its receiver first.
		in = 2
	}
	return fn.NumIn() == in && fn.In(in-1) == param && fn.NumOut() == 1 && fn.Out(0) == errorType
}

// encodesOwnJSON reports whether t has a method encoding/json writes it with instead of by its
// kind: MarshalJSON or MarshalText, and on encoding/json/v2 MarshalJSONTo or AppendText.
func encodesOwnJSON(t reflect.Type) bool {
	if t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) {
		return true
	}
	if writesAppendText && t.Implements(textAppenderType) {
		return true
	}
	return hasStreamMethod(t, "MarshalJSONTo", jsonEncoderType)
}

// decodesOwnJSON reports whether t has a method encoding/json reads it with instead of by its
// kind, other than UnmarshalText: UnmarshalJSON, and on encoding/json/v2 UnmarshalJSONFrom.
func decodesOwnJSON(t reflect.Type) bool {
	return t.Implements(jsonUnmarshalerType) || hasStreamMethod(t, "UnmarshalJSONFrom", jsonDecoderType)
}

// tagNameKeys caches jsonTagKey by tag name.
var tagNameKeys sync.Map

// jsonTagKey is the key encoding/json gives a field whose json tag starts with name, the text
// before its first comma: name, or a part of it, when encoding/json takes it, "" when it uses the
// field's Go name instead. encoding/json is asked: it writes a struct whose field has the tag
// under two Go names, and the key it writes is the tag's when it is the same for both.
func jsonTagKey(name string) string {
	if name == "" {
		return ""
	}
	cached, ok := tagNameKeys.Load(name)
	if ok {
		return cached.(string)
	}

	key := ""
	first := taggedKey("A", name)
	if first != "" && first == taggedKey("B", name) {
		key = first
	}
	tagNameKeys.Store(name, key)
	return key
}

// taggedKey is the key encoding/json writes for a field goName of a struct with the json tag
// name, "" when it writes none.
func taggedKey(goName string, name string) string {
	value := name
	if name == "-" {
		// json:"-" drops the field, json:"-," names it "-".
		value = "-,"
	}
	tag := "json:" + strconv.Quote(value)
	typ := reflect.StructOf([]reflect.StructField{{Name: goName, Type: reflect.TypeFor[bool](), Tag: reflect.StructTag(tag)}})
	data, err := json.Marshal(reflect.New(typ).Elem().Interface())
	if err != nil {
		return ""
	}
	var object map[string]bool
	err = json.Unmarshal(data, &object)
	if err != nil || len(object) != 1 {
		return ""
	}
	for key := range object {
		return key
	}
	return ""
}
