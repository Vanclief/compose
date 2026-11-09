package types

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

type optionalAddress struct {
	City string `json:"city"`
	Zip  string `json:"zip,omitempty"`
}

type optionalPatch struct {
	Name    Optional[string]          `json:"name"`
	Age     Optional[int]             `json:"age"`
	Address Optional[optionalAddress] `json:"address"`
}

func TestOptionalUnmarshal(t *testing.T) {
	var p optionalPatch
	err := json.Unmarshal([]byte(`{"name":"Ana","age":null,"address":{"city":"Monterrey"}}`), &p)
	require.NoError(t, err)

	require.Equal(t, Some("Ana"), p.Name)
	require.Equal(t, Null[int](), p.Age)
	require.Equal(t, Some(optionalAddress{City: "Monterrey"}), p.Address)

	var absent optionalPatch
	err = json.Unmarshal([]byte(`{}`), &absent)
	require.NoError(t, err)
	require.False(t, absent.Name.Set)
	require.False(t, absent.Age.Set)
	require.False(t, absent.Address.Set)
}

func TestOptionalUnmarshalReuse(t *testing.T) {
	o := Some(optionalAddress{City: "Monterrey", Zip: "64000"})

	// A new value replaces the old one instead of merging into it.
	err := json.Unmarshal([]byte(`{"city":"Puebla"}`), &o)
	require.NoError(t, err)
	require.Equal(t, Some(optionalAddress{City: "Puebla"}), o)

	err = json.Unmarshal([]byte(`null`), &o)
	require.NoError(t, err)
	require.Equal(t, Null[optionalAddress](), o)

	err = json.Unmarshal([]byte(`{"city":"Tijuana"}`), &o)
	require.NoError(t, err)
	require.Equal(t, Some(optionalAddress{City: "Tijuana"}), o)
}

func TestOptionalUnmarshalError(t *testing.T) {
	o := Some(5)
	err := json.Unmarshal([]byte(`"five"`), &o)

	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Equal(t, Some(5), o)
}

func TestOptionalMarshal(t *testing.T) {
	type response struct {
		Name    Optional[string]          `json:"name,omitzero"`
		Age     Optional[int]             `json:"age,omitzero"`
		Address Optional[optionalAddress] `json:"address,omitzero"`
		Note    Optional[string]          `json:"note"`
	}

	raw, err := json.Marshal(response{
		Name:    Some("Ana"),
		Age:     Null[int](),
		Address: Some(optionalAddress{City: "Monterrey"}),
	})
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"Ana","age":null,"address":{"city":"Monterrey"},"note":null}`, string(raw))

	raw, err = json.Marshal(response{})
	require.NoError(t, err)
	require.JSONEq(t, `{"note":null}`, string(raw))
}

func TestOptionalGet(t *testing.T) {
	v, ok := Some("x").Get()
	require.True(t, ok)
	require.Equal(t, "x", v)

	_, ok = Null[string]().Get()
	require.False(t, ok)

	_, ok = Optional[string]{}.Get()
	require.False(t, ok)

	// A set zero value is still a value.
	v, ok = Some("").Get()
	require.True(t, ok)
	require.Equal(t, "", v)
}

func TestOptionalType(t *testing.T) {
	require.Equal(t, reflect.TypeFor[optionalAddress](), Optional[optionalAddress]{}.OptionalType())
	require.Equal(t, reflect.TypeFor[[]string](), Optional[[]string]{}.OptionalType())
}
