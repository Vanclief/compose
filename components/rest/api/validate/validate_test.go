package validate

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

const fixtureDoc = `{
  "openapi": "3.1.0",
  "info": {"title": "fixture", "version": "1.0.0"},
  "paths": {},
  "components": {
    "schemas": {
      "fixture.RunStatus": {
        "type": "string",
        "enum": ["pending", "running", "failed", "cancelled"]
      },
      "fixture.UpdateRequest": {
        "type": "object",
        "properties": {
          "max_total_runs": {"type": ["integer", "null"], "minimum": 0},
          "status": {"$ref": "#/components/schemas/fixture.RunStatus", "enum": ["failed", "cancelled"]},
          "failure_reason": {"type": ["string", "null"]}
        }
      },
      "fixture.CreateRequest": {
        "type": "object",
        "required": ["name", "config"],
        "properties": {
          "name": {"type": "string", "minLength": 3},
          "config": {
            "type": "object",
            "required": ["name"],
            "properties": {"name": {"type": "string"}, "retries": {"type": "integer", "format": "int32"}}
          },
          "tags": {"type": "array", "items": {"type": "string"}, "maxItems": 2, "uniqueItems": true},
          "labels": {"type": "object", "additionalProperties": {"$ref": "#/components/schemas/fixture.Label"}},
          "meta": {"type": "object", "patternProperties": {"^x-": {"type": "integer"}}}
        }
      },
      "fixture.Label": {
        "type": "object",
        "properties": {"count": {"type": "integer", "minimum": 0}, "steps": {"type": "array", "items": {"$ref": "#/components/schemas/fixture.Label"}}}
      },
      "fixture.StrictRequest": {
        "type": "object",
        "additionalProperties": false,
        "properties": {"status": {"type": "string"}}
      },
      "fixture.Run": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "format": "uuid"},
          "created_at": {"type": "string", "format": "date-time"},
          "email": {"type": "string", "format": "email"},
          "phone": {"type": "string", "format": "phone"},
          "ip": {"type": "string", "format": "ip"},
          "total": {"type": "number", "format": "double"},
          "count": {"type": "integer", "format": "int64"},
          "kind": {"anyOf": [{"type": "string"}, {"type": "null"}]},
          "create": {"anyOf": [{"$ref": "#/components/schemas/fixture.CreateRequest"}, {"type": "null"}]}
        }
      }
    }
  }
}`

const sentinel = "ZZZ_SENTINEL"

func opSchema(body string, required ...string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string", "format": "uuid"}},
				"required":   []string{"id"},
			},
			"query": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"limit":  map[string]any{"type": "integer", "maximum": 1000},
					"status": map[string]any{"$ref": "spec.json#/components/schemas/fixture.RunStatus"},
				},
			},
			"header": map[string]any{
				"type":       "object",
				"properties": map[string]any{"x-trace": map[string]any{"type": "string"}},
			},
			"body": map[string]any{"$ref": "spec.json#/components/schemas/" + body},
		},
		"required": append([]string{"path"}, required...),
	}
}

func compile(t *testing.T, name, body string, required ...string) *Schema {
	t.Helper()
	compiler, err := NewCompiler(json.RawMessage(fixtureDoc))
	require.NoError(t, err)
	schema, err := compiler.Compile(name, opSchema(body, required...))
	require.NoError(t, err)
	return schema
}

func instance(t *testing.T, raw string) any {
	t.Helper()
	v, err := DecodeBytes([]byte(raw))
	require.NoError(t, err)
	return v
}

const validID = "01924f8e-9b1c-7a5d-8e3f-123456789abc"

func TestValidatePasses(t *testing.T) {
	schema := compile(t, "op:updateRun.json", "fixture.UpdateRequest")

	cases := []string{
		`{"path":{"id":"` + validID + `"},"query":{"limit":1000,"status":"running"},"header":{"x-trace":"abc"},"body":{"max_total_runs":5,"status":"failed"}}`,
		`{"path":{"id":"` + validID + `"}}`,
		`{"path":{"id":"` + validID + `"},"body":{"max_total_runs":null,"failure_reason":null}}`,
		`{"path":{"id":"550e8400-e29b-41d4-a716-446655440000"},"body":{}}`,
	}
	for _, c := range cases {
		require.NoError(t, schema.Validate(instance(t, c)), c)
	}
}

func TestValidateMessages(t *testing.T) {
	update := compile(t, "op:updateRun.json", "fixture.UpdateRequest")
	create := compile(t, "op:createRun.json", "fixture.CreateRequest", "body")
	strict := compile(t, "op:strict.json", "fixture.StrictRequest")

	cases := []struct {
		schema *Schema
		raw    string
		want   string
	}{
		{update, `{"path":{"id":"not-a-uuid"}}`, "path id must be a valid uuid"},
		{update, `{"path":{}}`, "path id is required"},
		{update, `{}`, "path is required"},
		{update, `{"path":{"id":"` + validID + `"},"body":{"max_total_runs":-1}}`, "max_total_runs must be >= 0"},
		{update, `{"path":{"id":"` + validID + `"},"body":{"max_total_runs":"x"}}`, "max_total_runs must be integer or null"},
		{update, `{"path":{"id":"` + validID + `"},"body":{"status":"pending"}}`, `status must be one of "failed", "cancelled"`},
		{update, `{"path":{"id":"` + validID + `"},"query":{"limit":"1.0"}}`, "query limit must be integer"},
		{update, `{"path":{"id":"` + validID + `"},"query":{"status":"nope"}}`, `query status must be one of "pending", "running", "failed", "cancelled"`},
		{update, `{"path":{"id":"` + validID + `"},"header":{"x-trace":1}}`, "header x-trace must be string"},
		{create, `{"path":{"id":"` + validID + `"}}`, "body is required"},
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"abc","config":{}}}`, "config.name is required"},
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"ab","config":{"name":"x"}}}`, "name must be at least 3 characters"},
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"abc","config":{"name":"x"},"tags":["a","a"]}}`, "tags must not contain duplicates"},
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"abc","config":{"name":"x"},"tags":["a",1]}}`, "tags.1 must be string"},
		{strict, `{"path":{"id":"` + validID + `"},"body":{"status":"x","Status":"y"}}`, "Status is not a known field"},
		{strict, `{"path":{"id":"` + validID + `"},"body":{"evil\nkey<script>":1}}`, "* is not a known field"},
		// Map keys are client-chosen: shown as "*", even when they look like a
		// declared name of the value schema.
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"abc","config":{"name":"x"},"labels":{"evil\nkey<script>":{"count":-1}}}}`, "labels.*.count must be >= 0"},
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"abc","config":{"name":"x"},"labels":{"count":{"count":-1}}}}`, "labels.*.count must be >= 0"},
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"abc","config":{"name":"x"},"labels":{"k":{"steps":[{"count":-1}]}}}}`, "labels.*.steps.0.count must be >= 0"},
		// A map key that looks like an index is still a map key; an array
		// index under it stays.
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"abc","config":{"name":"x"},"labels":{"1234567890123456789":{"count":-1}}}}`, "labels.*.count must be >= 0"},
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"abc","config":{"name":"x"},"labels":{"7":{"steps":[{"count":0},{"count":-1}]}}}}`, "labels.*.steps.1.count must be >= 0"},
		{create, `{"path":{"id":"` + validID + `"},"body":{"name":"abc","config":{"name":"x"},"meta":{"x-<b>":"s"}}}`, "meta.* must be integer"},
	}
	for _, c := range cases {
		err := c.schema.Validate(instance(t, c.raw))
		require.Error(t, err, c.raw)
		require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		require.Equal(t, c.want, ez.ErrorMessage(err), c.raw)
	}
}

func TestValidateMultipleErrors(t *testing.T) {
	schema := compile(t, "op:updateRun.json", "fixture.UpdateRequest")

	raw := `{"path":{"id":"` + sentinel + `"},"query":{"limit":5000,"status":"` + sentinel + `"},"header":{"x-trace":42},` +
		`"body":{"max_total_runs":"` + sentinel + `","status":"` + sentinel + `","failure_reason":42}}`
	err := schema.Validate(instance(t, raw))
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	want := []FieldError{
		{In: "path", Name: "id", Message: "must be a valid uuid"},
		{In: "query", Name: "limit", Message: "must be <= 1000"},
		{In: "query", Name: "status", Message: `must be one of "pending", "running", "failed", "cancelled"`},
		{In: "header", Name: "x-trace", Message: "must be string"},
		{In: "body", Name: "failure_reason", Message: "must be string or null"},
		{In: "body", Name: "max_total_runs", Message: "must be integer or null"},
		{In: "body", Name: "status", Message: `must be one of "failed", "cancelled"`},
	}
	require.Equal(t, want, Fields(err))
	require.Equal(t, "path id must be a valid uuid; query limit must be <= 1000; "+
		`query status must be one of "pending", "running", "failed", "cancelled"; header x-trace must be string; `+
		`failure_reason must be string or null; max_total_runs must be integer or null; `+
		`status must be one of "failed", "cancelled"`,
		ez.ErrorMessage(err))

	require.NotContains(t, err.Error(), sentinel)
	require.NotContains(t, ez.ErrorMessage(err), sentinel)

	// Fields survives ez.Wrap.
	require.Equal(t, want, Fields(ez.Wrap(err)))
	require.Equal(t, ez.EINVALID, ez.ErrorCode(ez.Wrap(err)))
}

func TestFormats(t *testing.T) {
	compiler, err := NewCompiler(json.RawMessage(fixtureDoc))
	require.NoError(t, err)
	schema, err := compiler.Compile("op:run.json", map[string]any{
		"type":       "object",
		"properties": map[string]any{"body": map[string]any{"$ref": "spec.json#/components/schemas/fixture.Run"}},
	})
	require.NoError(t, err)

	valid := `{"body":{"id":"` + validID + `","created_at":"2026-10-11T10:00:00Z","email":"a@b.co","phone":"+5215512345678","ip":"2001:db8::1",` +
		`"total":1.5,"count":12345678901234567890,"kind":null,"create":null}}`
	require.NoError(t, schema.Validate(instance(t, valid)))

	invalid := `{"body":{"id":"` + sentinel + `","created_at":"` + sentinel + `","email":"` + sentinel + `","phone":"` + sentinel + `","ip":"` + sentinel + `","kind":1,` +
		`"create":{"name":"abc","config":{"retries":"` + sentinel + `"}}}}`
	err = schema.Validate(instance(t, invalid))
	require.Error(t, err)
	require.Equal(t, "create.config.name is required; create.config.retries must be integer; created_at must be a valid date-time; email must be a valid email; id must be a valid uuid; ip must be a valid ip; "+
		"kind must be string or null; phone must be a valid phone", ez.ErrorMessage(err))
	require.NotContains(t, err.Error(), sentinel)
}

// TestIPFormat: the ip format accepts what netip.ParseAddr accepts, of
// either family and with a zone, and refuses the empty string, a prefix and
// an address with a port.
func TestIPFormat(t *testing.T) {
	compiler, err := NewCompiler(json.RawMessage(fixtureDoc))
	require.NoError(t, err)
	schema, err := compiler.Compile("op:ip.json", map[string]any{
		"type":       "object",
		"properties": map[string]any{"body": map[string]any{"type": "string", "format": "ip"}},
	})
	require.NoError(t, err)

	for _, value := range []string{"192.0.2.1", "2001:db8::1", "::ffff:192.0.2.1", "fe80::1%eth0"} {
		require.NoError(t, schema.Validate(map[string]any{"body": value}), value)
	}
	for _, value := range []string{"", "garbage", "192.0.2.010", "192.0.2.0/24", "192.0.2.1:443"} {
		err = schema.Validate(map[string]any{"body": value})
		require.Error(t, err, value)
		require.Equal(t, "body must be a valid ip", ez.ErrorMessage(err), value)
	}
}

func TestCompileErrors(t *testing.T) {
	_, err := NewCompiler(json.RawMessage(`{"openapi":`))
	require.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))

	compiler, err := NewCompiler(json.RawMessage(fixtureDoc))
	require.NoError(t, err)

	_, err = compiler.Compile("op:missing.json", map[string]any{"$ref": "spec.json#/components/schemas/Nope"})
	require.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))

	_, err = compiler.Compile("op:a.json", map[string]any{"type": "object"})
	require.NoError(t, err)
	_, err = compiler.Compile("op:a.json", map[string]any{"type": "object"})
	require.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
}

func TestValidateConcurrent(t *testing.T) {
	schema := compile(t, "op:updateRun.json", "fixture.UpdateRequest")
	good := instance(t, `{"path":{"id":"`+validID+`"},"body":{"max_total_runs":3}}`)
	bad := instance(t, `{"path":{"id":"x"},"body":{"max_total_runs":-1}}`)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				require.NoError(t, schema.Validate(good))
				err := schema.Validate(bad)
				require.Equal(t, "path id must be a valid uuid; max_total_runs must be >= 0", ez.ErrorMessage(err))
			}
		}()
	}
	wg.Wait()
}

func TestNewFieldsError(t *testing.T) {
	require.NoError(t, NewFieldsError(nil))

	err := NewFieldsError([]FieldError{
		{In: "body", Name: "b", Message: "is required"},
		{In: "query", Name: "a", Message: "must be integer"},
		{In: "body", Name: "b", Message: "is required"},
	})
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Equal(t, "query a must be integer; b is required", ez.ErrorMessage(err))
	require.Len(t, Fields(err), 2)
	require.Nil(t, Fields(ez.New(ez.EINVALID, "other", nil)))
}

func TestFieldsErrorKeepsMaxFields(t *testing.T) {
	var fields []FieldError
	for i := range MaxFields + 50 {
		fields = append(fields, FieldError{In: "body", Name: fmt.Sprintf("items.%04d", i), Message: "must be string"})
	}

	// The first MaxFields in order are kept, and the message counts the rest.
	err := NewFieldsError(fields)
	kept := Fields(err)
	require.Len(t, kept, MaxFields)
	require.Equal(t, "items.0000", kept[0].Name)
	require.Equal(t, "items.0099", kept[MaxFields-1].Name)
	require.True(t, strings.HasSuffix(ez.ErrorMessage(err), "; items.0099 must be string; and 50 more"), ez.ErrorMessage(err))

	// Exactly MaxFields are all listed, with nothing appended.
	err = NewFieldsError(fields[:MaxFields])
	require.Len(t, Fields(err), MaxFields)
	require.NotContains(t, ez.ErrorMessage(err), "more")
}

func TestValidateStopsAtMaxFields(t *testing.T) {
	schema := compile(t, "op:strict.json", "fixture.StrictRequest")

	// 150 unknown keys, which the validator lists in map order: the walk
	// stops past the cap, keeps the first 100 by name every time, and the
	// message says there are more without counting them.
	var keys []string
	for i := range MaxFields + 50 {
		keys = append(keys, fmt.Sprintf(`"k%03d":1`, i))
	}
	raw := `{"path":{"id":"` + validID + `"},"body":{` + strings.Join(keys, ",") + `}}`
	for range 5 {
		err := schema.Validate(instance(t, raw))
		kept := Fields(err)
		require.Len(t, kept, MaxFields)
		require.Equal(t, "k000", kept[0].Name)
		require.Equal(t, "k099", kept[MaxFields-1].Name)
		require.True(t, strings.HasSuffix(ez.ErrorMessage(err), "; k099 is not a known field; and more"), ez.ErrorMessage(err))
	}

	// Exactly MaxFields failing fields are all listed, with nothing appended.
	raw = `{"path":{"id":"` + validID + `"},"body":{` + strings.Join(keys[:MaxFields], ",") + `}}`
	err := schema.Validate(instance(t, raw))
	require.Len(t, Fields(err), MaxFields)
	require.True(t, strings.HasSuffix(ez.ErrorMessage(err), "; k099 is not a known field"), ez.ErrorMessage(err))

	// The caller's fields past the cap are counted: the list is whole.
	var extra []FieldError
	for i := range MaxFields + 50 {
		extra = append(extra, FieldError{In: "body", Name: fmt.Sprintf("x%03d", i), Message: "is invalid"})
	}
	err = schema.Validate(instance(t, `{"path":{"id":"`+validID+`"},"body":{}}`), extra...)
	require.True(t, strings.HasSuffix(ez.ErrorMessage(err), "; x099 is invalid; and 50 more"), ez.ErrorMessage(err))
}

func TestFieldsErrorOrdersIndicesAsNumbers(t *testing.T) {
	var fields []FieldError
	for i := range MaxFields + 50 {
		fields = append(fields, FieldError{In: "body", Name: fmt.Sprintf("steps.%d.retries", i), Message: "must be >= 0"})
	}
	slices.Reverse(fields)

	// The fields kept of a long array are its first items, not the first in
	// text order, where steps.100 comes before steps.2.
	err := NewFieldsError(fields)
	for i, field := range Fields(err) {
		require.Equal(t, fmt.Sprintf("steps.%d.retries", i), field.Name)
	}
	require.True(t, strings.HasPrefix(ez.ErrorMessage(err), "steps.0.retries must be >= 0; steps.1.retries must be >= 0; steps.2.retries"), ez.ErrorMessage(err))

	// Everything but a run of digits keeps the order of strings.Compare.
	names := []string{"b", "a.b", "a-b", "a", "tags.10", "tags.2", "tags.01", "tags.1", "x2y", "x10"}
	fields = nil
	for _, name := range names {
		fields = append(fields, FieldError{In: "body", Name: name, Message: "is invalid"})
	}
	var got []string
	for _, field := range Fields(NewFieldsError(fields)) {
		got = append(got, field.Name)
	}
	require.Equal(t, []string{"a", "a-b", "a.b", "b", "tags.01", "tags.1", "tags.2", "tags.10", "x2y", "x10"}, got)
}

func TestValidateMergesExtraFields(t *testing.T) {
	schema := compile(t, "op:updateRun.json", "fixture.UpdateRequest")
	extra := FieldError{In: "body", Name: "Status", Message: `is not a known field, did you mean "status"`}

	// A valid instance fails on the caller's fields alone.
	valid := `{"path":{"id":"` + validID + `"},"body":{}}`
	err := schema.Validate(instance(t, valid), extra)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Equal(t, []FieldError{extra}, Fields(err))

	// Schema violations and the caller's fields are reported together.
	invalid := `{"path":{"id":"` + validID + `"},"body":{"max_total_runs":-1}}`
	err = schema.Validate(instance(t, invalid), extra)
	want := []FieldError{
		extra,
		{In: "body", Name: "max_total_runs", Message: "must be >= 0"},
	}
	require.Equal(t, want, Fields(err))
}
