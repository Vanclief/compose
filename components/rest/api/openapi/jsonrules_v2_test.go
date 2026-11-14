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
	require.Equal(t, `openapi: openapi.pcStreamHolder: the JSON encoder of embedded field PCStream would take over the whole value, not the fields the schema lists; make it a named field`, ez.ErrorMessage(err))

	_, err = NewReflector().AddRequest(reflect.TypeFor[pcStreamHolder]())
	require.Error(t, err)
	require.Equal(t, `openapi: openapi.pcStreamHolder: the JSON decoder of embedded field PCStream would take over the whole value, not the fields the schema lists; make it a named field`, ez.ErrorMessage(err))
}
