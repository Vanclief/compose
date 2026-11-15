package spectest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/vanclief/ez"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"

	"github.com/vanclief/compose/components/rest/api/validate"
)

// documentURL is where the document is loaded; it is not fetched.
const documentURL = "mem:///spec.json"

// Schema fails t when value does not validate against the component schema
// #/components/schemas/<component> of the OpenAPI document spec, with one
// message naming the component and, for each failure, the JSON pointer of
// the failing value in the JSON and the reason, at most validate.MaxFields
// of them.
//
// value is marshalled with encoding/json, the encoder the handlers answer
// with; a json.RawMessage or a []byte is taken as the JSON it holds, so a
// test can pass a recorded response body. JSON whose object repeats a key
// fails, as apitest refuses it.
//
// The value is validated as a response, by JSON Schema 2020-12 with the
// document's $refs resolved within it, recursive ones included: type,
// required, properties, additionalProperties, items, enum, const, allOf,
// oneOf, anyOf, not, the number, string, array and object bounds, pattern,
// and the formats the JSON Schema library knows (date-time, date, time,
// uuid, email, ipv4, ipv6, uri and others); readOnly properties are
// allowed. In an OpenAPI 3.0 document, the schemas of components.schemas are
// read as 2020-12: nullable: true admits null whatever else the schema says,
// which is how the generator means it on a reference wrapped in allOf, and a
// boolean exclusiveMinimum or exclusiveMaximum makes its bound exclusive.
//
// Where the value is reached from the component through properties, items,
// prefixItems, an additionalProperties schema, allOf, and $refs to whole
// components (#/components/schemas/<name>), an object whose schema declares
// properties, itself or through those allOf and $refs, is checked as closed,
// as encoding/json writes no key its type lacks: a key the schema does not
// declare, as a property or in required, fails, though the generator writes
// no additionalProperties. It stays open where its schema states
// additionalProperties, patternProperties, unevaluatedProperties,
// propertyNames or dependentRequired, composes by oneOf, anyOf, if,
// dependentSchemas or $dynamicRef, holds a discriminator, as the base of an allOf hierarchy does, holds in that chain
// a free-form object (type object with no properties, required, $ref or
// allOf) or a $ref that is not to a whole component, or declares no
// properties, as a map or a free-form object does. Where the value is reached
// that way, open or closed, a writeOnly property, marked on the property or
// on a schema its allOf or its $ref to a component leads to, must be absent
// and is not required, unless a component a $ref leads to requires it and
// does not, itself or through its own allOf and $refs, mark it writeOnly.
// Everything under oneOf,
// anyOf, not, if, then, else, contains, propertyNames, patternProperties,
// dependentSchemas or $defs, and behind a $ref that is not to a whole
// component, is checked as the document writes it: no key fails for being
// undeclared, and writeOnly changes nothing.
//
// Not checked: formats the library does not know, such as int32, int64,
// double, byte, binary or decimal (an integer is still checked as an
// integer); discriminator, which picks no branch of a oneOf or anyOf and
// otherwise only leaves its object open; whether the schema is the one the
// operation's response declares; a writeOnly property reached through anyOf
// or oneOf, as in the 3.1 nullable form, which stays required where required
// lists it; and the fields a value leaves at their zero value as other
// values, which a test covers by passing such values. Read otherwise than
// OpenAPI 3.0 reads them: every keyword beside a $ref is applied, nullable
// among them, where 3.0 ignores them; a schema a $ref leads to outside
// components.schemas, such as one under components.responses, is compiled
// as written, without the 3.0 reading of nullable and exclusive bounds; and
// a $ref whose pointer passes through a nullable schema of
// components.schemas in a 3.0 document cannot be compiled.
func Schema(t testing.TB, spec []byte, component string, value any) {
	t.Helper()

	var raw []byte
	switch v := value.(type) {
	case json.RawMessage:
		raw = v
	case []byte:
		raw = v
	default:
		var err error
		raw, err = json.Marshal(value)
		if err != nil {
			t.Errorf("spectest: %s: marshal the value: %v", component, err)
			return
		}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		t.Errorf("spectest: %s: the value is empty, not JSON", component)
		return
	}
	instance, err := validate.DecodeBytes(raw)
	if err != nil {
		t.Errorf("spectest: %s: %s", component, ez.ErrorMessage(err))
		return
	}
	err = validate.RejectRepeatedKeys(raw)
	if err != nil {
		t.Errorf("spectest: %s: %s", component, ez.ErrorMessage(err))
		return
	}

	schema, err := compileComponent(spec, component)
	if err != nil {
		t.Errorf("spectest: %s: %s", component, ez.ErrorMessage(err))
		return
	}

	err = schema.Validate(instance)
	if err == nil {
		return
	}
	verr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		t.Errorf("spectest: %s: validation failed: %v", component, err)
		return
	}

	var failures []string
	printer := message.NewPrinter(language.English)
	for _, leaf := range leaves(nil, verr) {
		extra, ok := leaf.ErrorKind.(*kind.AdditionalProperties)
		if !ok {
			failures = append(failures, "at "+strconv.Quote(pointer(leaf.InstanceLocation))+": "+reason(leaf.ErrorKind, printer))
			continue
		}
		for _, key := range extra.Properties {
			at := pointer(append(slices.Clone(leaf.InstanceLocation), key))
			failures = append(failures, "at "+strconv.Quote(at)+": the document does not declare this key: the field's json name differs from the document's, or the document lacks the field")
		}
	}
	// The library's order follows map iteration; sorted, a message is the
	// same from run to run.
	slices.Sort(failures)
	failures = slices.Compact(failures)
	more := len(failures) - validate.MaxFields
	if more > 0 {
		failures = append(failures[:validate.MaxFields], "and "+strconv.Itoa(more)+" more")
	}
	t.Errorf("spectest: the value does not match component %s:\n  %s", component, strings.Join(failures, "\n  "))
}

// compileComponent loads the document and compiles the named component of
// it in the response direction.
func compileComponent(spec []byte, component string) (*jsonschema.Schema, error) {
	doc, err := decodeDocument(spec)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	if doc["openapi"] == nil {
		return nil, ez.New(ez.EINVALID, "the document is not OpenAPI 3.x: it has no openapi field, or the field is null", nil)
	}
	version, ok := doc["openapi"].(string)
	if !ok {
		read, _ := json.Marshal(doc["openapi"]) // cannot fail: the value was decoded from JSON
		return nil, ez.New(ez.EINVALID, "the document is not OpenAPI 3.x: its openapi field is "+string(read)+", not a string; quote it", nil)
	}
	if !strings.HasPrefix(version, "3.") {
		return nil, ez.New(ez.EINVALID, "the document is not OpenAPI 3.x: its openapi field is "+strconv.Quote(version), nil)
	}
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	_, ok = schemas[component]
	if !ok {
		return nil, ez.New(ez.ENOTFOUND, "no such component in #/components/schemas, which holds "+strconv.Itoa(len(schemas))+" schemas", nil)
	}

	// The closed copies are made from the schemas as written, before
	// responseSchema moves a nullable one under else.
	c := &closer{schemas: schemas, closed: map[string]any{}}
	check := c.copy(map[string]any{"$ref": "#/components/schemas/" + pointerToken(component)}, instance, nil)
	doc[checkKey] = check
	doc[closedKey] = c.closed
	openapi30 := strings.HasPrefix(version, "3.0")
	for _, s := range schemas {
		responseSchema(s, openapi30)
	}
	for _, s := range c.closed {
		responseSchema(s, openapi30)
	}

	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	err = compiler.AddResource(documentURL, doc)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "could not load the document", err)
	}

	schema, err := compiler.Compile(documentURL + "#/" + checkKey)
	if err != nil {
		return nil, ez.New(ez.EINVALID, "could not compile the component: "+err.Error(), err)
	}
	return schema, nil
}

// decodeDocument decodes JSON as the JSON Schema library does, numbers as
// json.Number, and anything else as YAML, re-encoded as JSON so mapping keys
// such as an unquoted 200 become strings. A leading UTF-8 byte order mark is
// dropped first, so JSON that starts with one is still read as JSON.
func decodeDocument(spec []byte) (map[string]any, error) {
	spec = bytes.TrimPrefix(spec, []byte("\xef\xbb\xbf"))
	var decoded any
	if json.Valid(spec) {
		var err error
		decoded, err = jsonschema.UnmarshalJSON(bytes.NewReader(spec))
		if err != nil {
			return nil, ez.New(ez.EINVALID, "the document is not valid JSON", err)
		}
	} else {
		var tree any
		err := yaml.Unmarshal(spec, &tree)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "the document is neither JSON nor valid YAML: "+err.Error(), err)
		}
		tree, err = stringKeys(tree, "")
		if err != nil {
			return nil, ez.Wrap(err)
		}
		raw, err := json.Marshal(tree)
		if err != nil {
			return nil, ez.New(ez.EINVALID, "the YAML document has a value JSON cannot hold", err)
		}
		decoded, err = jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return nil, ez.New(ez.EINVALID, "the YAML document has a value JSON cannot hold", err)
		}
	}

	doc, ok := decoded.(map[string]any)
	if !ok {
		return nil, ez.New(ez.EINVALID, "the document is not an object", nil)
	}
	return doc, nil
}

// stringKeys turns the map[any]any yaml.v3 makes of a mapping with a key
// that is not a string into a map[string]any. An integer key, such as an
// unquoted response code, becomes its decimal text, so one not written in
// plain decimal, such as 01 or 0x10, is renamed (1, 16); any other key, such
// as null or 1.10 (read as 1.1), is refused.
// path is the JSON pointer of node, for the message.
func stringKeys(node any, path string) (any, error) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			child, err := stringKeys(v, path+pointer([]string{k}))
			if err != nil {
				return nil, ez.Wrap(err)
			}
			n[k] = child
		}
		return n, nil
	case map[any]any:
		out := make(map[string]any, len(n))
		for k, v := range n {
			var key string
			switch typed := k.(type) {
			case string:
				key = typed
			case int:
				key = strconv.Itoa(typed)
			case int64:
				key = strconv.FormatInt(typed, 10)
			case uint64:
				key = strconv.FormatUint(typed, 10)
			case nil:
				return nil, ez.New(ez.EINVALID, "the YAML document has a key at "+strconv.Quote(path)+" that is not a string: it is read as null; quote the key", nil)
			default:
				return nil, ez.New(ez.EINVALID, fmt.Sprintf("the YAML document has a key at %q that is not a string: it is read as the %T %v; quote the key", path, k, k), nil)
			}
			child, err := stringKeys(v, path+pointer([]string{key}))
			if err != nil {
				return nil, ez.Wrap(err)
			}
			out[key] = child
		}
		return out, nil
	case []any:
		for i, v := range n {
			child, err := stringKeys(v, path+"/"+strconv.Itoa(i))
			if err != nil {
				return nil, ez.Wrap(err)
			}
			n[i] = child
		}
		return n, nil
	}
	return node, nil
}

// schemaMaps and schemaLists are the keywords whose values are maps of
// schemas and lists of schemas; every other keyword below is one schema.
var (
	schemaMaps   = []string{"properties", "patternProperties", "dependentSchemas", "$defs", "definitions"}
	schemaLists  = []string{"allOf", "anyOf", "oneOf", "prefixItems"}
	schemaSingle = []string{"items", "additionalProperties", "not", "if", "then", "else", "contains", "propertyNames", "unevaluatedItems", "unevaluatedProperties"}
)

// place is where a schema sits relative to the JSON value it checks.
type place int

const (
	// member checks the value its parent checks, composed with the parent's
	// other keywords: a component, an allOf member.
	member place = iota
	// instance checks a value of its own: the component checked, a
	// property, an item, a map value.
	instance
)

// Where the closed copies sit in the document, beside the schemas as
// written: the schema the value is checked against, and the closed copy of
// each component it reaches, by name.
const (
	checkKey  = "x-spectest-check"
	closedKey = "x-spectest-closed"
)

// closer makes the closed copies of a check. The schemas as written are
// never changed: closed holds a copy of each component reached, made once,
// and every $ref in a copy that is not left as written leads to another
// copy.
type closer struct {
	schemas map[string]any
	closed  map[string]any
}

// copy returns the schema node as the check reads it at the place at.
// Properties, items, prefixItems, an additionalProperties schema and allOf
// members are copied the same way, and a $ref to a whole component is
// pointed at its copy; at an instance place, an object whose chain declares
// its properties is closed. Every other keyword is kept as written, so
// nothing under oneOf, anyOf, not, if, then, else, contains, propertyNames,
// patternProperties, dependentSchemas or $defs, or behind a $ref to part of
// a component, is closed or rewritten. A writeOnly property becomes false
// and leaves required, where required stands in composer, the schema whose
// inline allOf node is in, or in node itself.
func (c *closer) copy(node any, at place, composer map[string]any) any {
	s, ok := node.(map[string]any)
	if !ok {
		return node
	}
	if composer == nil {
		composer = s
	}
	out := maps.Clone(s)

	ref, _ := s["$ref"].(string)
	name, ok := c.component(ref)
	if ok {
		c.closeComponent(name)
		out["$ref"] = "#/" + closedKey + "/" + pointerToken(name)
	}

	properties, _ := s["properties"].(map[string]any)
	if properties != nil {
		copied := make(map[string]any, len(properties))
		for key, property := range properties {
			if writeOnly(c, property, map[string]bool{}) {
				copied[key] = false
				continue
			}
			copied[key] = c.copy(property, instance, nil)
		}
		out["properties"] = copied
	}
	required, _ := s["required"].([]any)
	if len(required) > 0 {
		chain := &chain{closer: c, seen: map[string]bool{}}
		chain.walk(composer)
		kept := []any{}
		for _, key := range required {
			k, _ := key.(string)
			if !slices.Contains(chain.writeOnly, k) {
				kept = append(kept, key)
			}
		}
		out["required"] = kept
	}
	for _, key := range []string{"items", "additionalProperties"} {
		_, ok := s[key]
		if ok {
			out[key] = c.copy(s[key], instance, nil)
		}
	}
	prefixItems, _ := s["prefixItems"].([]any)
	if prefixItems != nil {
		copied := make([]any, len(prefixItems))
		for i, item := range prefixItems {
			copied[i] = c.copy(item, instance, nil)
		}
		out["prefixItems"] = copied
	}
	allOf, _ := s["allOf"].([]any)
	if allOf != nil {
		copied := make([]any, len(allOf))
		for i, m := range allOf {
			copied[i] = c.copy(m, member, composer)
		}
		out["allOf"] = copied
	}

	if at == instance {
		names, ok := c.declaredNames(s)
		if ok {
			closeObject(out, names)
		}
	}
	return out
}

// closeComponent makes the closed copy of the named component, once; the
// entry is set before the copy is made, which ends a cycle.
func (c *closer) closeComponent(name string) {
	_, ok := c.closed[name]
	if ok {
		return
	}
	c.closed[name] = nil
	c.closed[name] = c.copy(c.schemas[name], member, nil)
}

// component returns the name of the component a $ref of the form
// #/components/schemas/<name> points to, when the document has it. The
// fragment is percent-decoded before it is read as a JSON pointer, so %2F
// separates tokens as / does.
func (c *closer) component(ref string) (string, bool) {
	token, ok := strings.CutPrefix(ref, "#/components/schemas/")
	if !ok {
		return "", false
	}
	token, err := url.PathUnescape(token)
	if err != nil || strings.Contains(token, "/") {
		return "", false
	}
	name := strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
	_, ok = c.schemas[name]
	return name, ok
}

// writeOnly reports whether the schema node, or one its allOf and its $refs
// to components lead to, says writeOnly: true.
func writeOnly(c *closer, node any, seen map[string]bool) bool {
	s, ok := node.(map[string]any)
	if !ok {
		return false
	}
	if s["writeOnly"] == true {
		return true
	}
	ref, _ := s["$ref"].(string)
	name, ok := c.component(ref)
	if ok && !seen[name] {
		seen[name] = true
		if writeOnly(c, c.schemas[name], seen) {
			return true
		}
	}
	allOf, _ := s["allOf"].([]any)
	for _, m := range allOf {
		if writeOnly(c, m, seen) {
			return true
		}
	}
	return false
}

// openKeywords leave an object open: they let it take keys its properties do
// not name, name keys beside properties and required as dependentRequired
// does, compose it in a way a chain does not follow, or, as discriminator
// on the base of an allOf hierarchy, admit the keys of the schemas that
// extend it.
var openKeywords = []string{"patternProperties", "additionalProperties", "unevaluatedProperties", "propertyNames", "dependentRequired", "oneOf", "anyOf", "if", "dependentSchemas", "$dynamicRef", "discriminator"}

// declaredNames returns the property names the schema s declares, itself and
// through its allOf and its $refs to components, in properties or required,
// and whether an object it accepts may hold only those keys: false when
// nothing in the chain declares properties, when something in it states its
// own rule for other keys or a dependentRequired, composes by oneOf, anyOf,
// if, dependentSchemas or $dynamicRef,
// holds a discriminator or is a free-form object, and when a $ref in it is
// not to a whole component of the document.
func (c *closer) declaredNames(s any) ([]string, bool) {
	chain := &chain{closer: c, seen: map[string]bool{}}
	chain.walk(s)
	if !chain.declares || chain.open {
		return nil, false
	}
	slices.Sort(chain.names)
	return slices.Compact(chain.names), true
}

// chain follows a schema through allOf and $refs to components.
type chain struct {
	closer    *closer
	seen      map[string]bool
	names     []string
	writeOnly []string
	declares  bool
	open      bool
}

func (c *chain) walk(node any) {
	s, ok := node.(map[string]any)
	if !ok {
		return
	}
	for _, key := range openKeywords {
		_, ok := s[key]
		if ok {
			c.open = true
		}
	}
	if freeForm(s) {
		c.open = true
	}
	properties, ok := s["properties"].(map[string]any)
	if ok {
		c.declares = true
	}
	for name, property := range properties {
		c.names = append(c.names, name)
		if writeOnly(c.closer, property, map[string]bool{}) {
			c.writeOnly = append(c.writeOnly, name)
		}
	}
	required, _ := s["required"].([]any)
	for _, name := range required {
		n, ok := name.(string)
		if ok {
			c.names = append(c.names, n)
		}
	}
	ref, ok := s["$ref"].(string)
	if ok {
		name, found := c.closer.component(ref)
		if !found {
			c.open = true
		}
		if found && !c.seen[name] {
			c.seen[name] = true
			c.walk(c.closer.schemas[name])
		}
	}
	allOf, _ := s["allOf"].([]any)
	for _, m := range allOf {
		c.walk(m)
	}
}

// freeForm reports whether s says its value is an object and names no keys
// and nothing else to follow: no properties, required, $ref or allOf, as
// {type: object} written for a type the generator does not read. Such a
// schema in a chain lets the object hold any key; one that only constrains
// or describes, such as {required: [a]}, does not.
func freeForm(s map[string]any) bool {
	for _, key := range []string{"properties", "required", "$ref", "allOf"} {
		_, ok := s[key]
		if ok {
			return false
		}
	}
	switch t := s["type"].(type) {
	case string:
		return t == "object"
	case []any:
		return slices.Contains(t, any("object"))
	}
	return false
}

// closeObject makes the copy s refuse a key its chain does not declare: the
// declared names it does not hold itself match one pattern property of
// schema true, and additionalProperties is false. One pattern rather than a
// property per name keeps the schemas to compile from growing with the names
// at every place a component is reached. Unlike unevaluatedProperties, which
// counts a key as declared only where the schema declaring it passes, this
// names no correct key when a value beneath fails.
func closeObject(s map[string]any, names []string) {
	properties, _ := s["properties"].(map[string]any)
	var missing []string
	for _, name := range names {
		_, ok := properties[name]
		if !ok {
			missing = append(missing, regexp.QuoteMeta(name))
		}
	}
	if len(missing) > 0 {
		s["patternProperties"] = map[string]any{"^(?:" + strings.Join(missing, "|") + ")$": true}
	}
	s["additionalProperties"] = false
}

// responseSchema rewrites a schema of the document, in place, into the JSON
// Schema 2020-12 a response is validated against: $schema is dropped, as
// Schema fixes the draft, and in OpenAPI 3.0 (openapi30) nullable and the
// boolean exclusive bounds take their 2020-12 form. A closed copy shares
// the parts it keeps as written with the schemas, so a schema can be
// rewritten twice, which changes nothing the second time.
func responseSchema(node any, openapi30 bool) {
	s, ok := node.(map[string]any)
	if !ok {
		return
	}

	delete(s, "$schema")

	for _, key := range schemaMaps {
		children, _ := s[key].(map[string]any)
		for _, child := range children {
			responseSchema(child, openapi30)
		}
	}
	for _, key := range schemaLists {
		children, _ := s[key].([]any)
		for _, child := range children {
			responseSchema(child, openapi30)
		}
	}
	for _, key := range schemaSingle {
		responseSchema(s[key], openapi30)
	}

	if !openapi30 {
		return
	}
	exclusiveBound(s, "exclusiveMinimum", "minimum")
	exclusiveBound(s, "exclusiveMaximum", "maximum")
	if s["nullable"] != true {
		return
	}
	delete(s, "nullable")
	// The schema moves under else, so null passes whatever it says and any
	// other value fails as the schema alone would make it fail, without the
	// extra failure an anyOf with {type: null} would report.
	rest := make(map[string]any, len(s))
	for k, v := range s {
		rest[k] = v
		delete(s, k)
	}
	s["if"] = map[string]any{"type": "null"}
	s["else"] = rest
}

// exclusiveBound turns OpenAPI 3.0's boolean exclusive bound into the
// 2020-12 number: true moves the bound's value to it, false drops it.
func exclusiveBound(s map[string]any, exclusive string, bound string) {
	flag, ok := s[exclusive].(bool)
	if !ok {
		return
	}
	delete(s, exclusive)
	value, ok := s[bound]
	if flag && ok {
		s[exclusive] = value
		delete(s, bound)
	}
}

// leaves appends the failures under verr that have no causes.
func leaves(out []*jsonschema.ValidationError, verr *jsonschema.ValidationError) []*jsonschema.ValidationError {
	if len(verr.Causes) == 0 {
		return append(out, verr)
	}
	for _, cause := range verr.Causes {
		out = leaves(out, cause)
	}
	return out
}

func reason(errKind jsonschema.ErrorKind, printer *message.Printer) string {
	switch k := errKind.(type) {
	case *kind.FalseSchema:
		return "must be absent: its schema is false, as a writeOnly property's is in a response"
	// The library prints these as float64, which loses digits.
	case *kind.Minimum:
		return "minimum: got " + ratString(k.Got) + ", want " + ratString(k.Want)
	case *kind.Maximum:
		return "maximum: got " + ratString(k.Got) + ", want " + ratString(k.Want)
	case *kind.ExclusiveMinimum:
		return "exclusiveMinimum: got " + ratString(k.Got) + ", want " + ratString(k.Want)
	case *kind.ExclusiveMaximum:
		return "exclusiveMaximum: got " + ratString(k.Got) + ", want " + ratString(k.Want)
	case *kind.MultipleOf:
		return "multipleOf: got " + ratString(k.Got) + ", want " + ratString(k.Want)
	}
	return errKind.LocalizedString(printer)
}

// ratString writes a number exactly when it is an integer, and otherwise as
// the shortest decimal of its float64; a copy of validate's.
func ratString(r *big.Rat) string {
	if r.IsInt() {
		return r.Num().String()
	}
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// pointer is the JSON pointer (RFC 6901) of an instance location.
func pointer(tokens []string) string {
	var b strings.Builder
	for _, token := range tokens {
		b.WriteString("/")
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

// pointerToken escapes a component name as one token of a JSON pointer in a
// URI fragment.
func pointerToken(name string) string {
	return url.PathEscape(strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1"))
}
