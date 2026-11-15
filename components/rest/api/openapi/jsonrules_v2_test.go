//go:build goexperiment.jsonv2 && go1.27

package openapi

import (
	"encoding/json/jsontext"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// PCStream writes itself with MarshalJSONTo and reads itself with
// UnmarshalJSONFrom, which encoding/json/v2 calls.
type PCStream struct {
	V string
}

func (s PCStream) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String(s.V))
}

func (s *PCStream) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	_, err := dec.ReadToken()
	return err
}

// pcStreamHolder gets PCStream's methods by embedding it.
type pcStreamHolder struct {
	PCStream
	Name string `json:"name"`
}

// TestStreamMethodsAreCoders: encoding/json/v2 hands a struct that gets
// MarshalJSONTo or UnmarshalJSONFrom from an embedded field to that method, so
// the registry refuses it as one that gets MarshalJSON or UnmarshalJSON.
func TestStreamMethodsAreCoders(t *testing.T) {
	require.Equal(t, `"v"`, marshalled(t, pcStreamHolder{PCStream: PCStream{V: "v"}, Name: "n"}))

	err := NewReflector().AddResponse(reflect.TypeFor[pcStreamHolder]())
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Equal(t, `openapi: openapi.pcStreamHolder: the JSON encoder of embedded field PCStream would take over the whole value, not the fields the schema lists; remove that encoder, or respond with a struct whose fields are the JSON to send`, ez.ErrorMessage(err))

	_, err = NewReflector().AddRequest(reflect.TypeFor[pcStreamHolder]())
	require.Error(t, err)
	require.Equal(t, `openapi: openapi.pcStreamHolder: the JSON decoder of embedded field PCStream would take over the whole value, not the fields the schema lists; make it a named field`, ez.ErrorMessage(err))
}

// PCWrongStream has a MarshalJSONTo and an UnmarshalJSONFrom of other
// signatures, which encoding/json/v2 does not call.
type PCWrongStream struct {
	V string `json:"v"`
}

func (PCWrongStream) MarshalJSONTo(jsontext.Encoder) error {
	return nil
}

func (*PCWrongStream) UnmarshalJSONFrom(*jsontext.Encoder) error {
	return nil
}

type pcWrongStreamHolder struct {
	PCWrongStream
	Name string `json:"name"`
}

// TestStreamMethodsNeedTheirSignature: a MarshalJSONTo or UnmarshalJSONFrom
// of another signature is not a coder, so a struct that gets one is written
// and read as its fields.
func TestStreamMethodsNeedTheirSignature(t *testing.T) {
	require.Equal(t, `{"v":"v","name":"n"}`, marshalled(t, pcWrongStreamHolder{PCWrongStream: PCWrongStream{V: "v"}, Name: "n"}))

	err := NewReflector().AddResponse(reflect.TypeFor[pcWrongStreamHolder]())
	require.NoError(t, err)
	_, err = NewReflector().AddRequest(reflect.TypeFor[pcWrongStreamHolder]())
	require.NoError(t, err)
}

// TestFormatOptionRefusedOnV2: encoding/json/v2 refuses a struct with a json
// format: option for every value, so the probe reports it here.
func TestFormatOptionRefusedOnV2(t *testing.T) {
	require.Contains(t, refusedTags(reflect.TypeFor[probeFormat]()), "Go struct field At has unsupported `format` tag option")
}

type pcStreamField struct {
	Stream PCStream `json:"stream"`
}

type pcAppendField struct {
	Append PCAppend `json:"append"`
}

// TestOwnEncoderNamed: a response struct encoding/json/v2 writes with
// MarshalJSONTo or AppendText is refused naming the method.
func TestOwnEncoderNamed(t *testing.T) {
	err := NewReflector().AddResponse(reflect.TypeFor[pcStreamField]())
	require.Error(t, err)
	require.Equal(t, `openapi: openapi.PCStream writes its own JSON with MarshalJSONTo, which the schema of its fields would not describe; respond with a struct whose fields are the JSON to send, or, where it writes a string, add an UnmarshalText so the type is published as a string`, ez.ErrorMessage(err))

	err = NewReflector().AddResponse(reflect.TypeFor[pcAppendField]())
	require.Error(t, err)
	require.Equal(t, `openapi: openapi.PCAppend writes its own JSON with AppendText, which the schema of its fields would not describe; respond with a struct whose fields are the JSON to send, or, where it writes a string, add an UnmarshalText so the type is published as a string`, ez.ErrorMessage(err))
}
