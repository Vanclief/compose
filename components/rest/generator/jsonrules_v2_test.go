//go:build goexperiment.jsonv2 && go1.27

package generator

import (
	"encoding/json/jsontext"
	"testing"
)

type jsonToRaw struct {
	A int
}

func (jsonToRaw) MarshalJSONTo(enc *jsontext.Encoder) error {
	return enc.WriteToken(jsontext.String("raw"))
}

type jsonToHolder struct {
	jsonToRaw
	N int `json:"n"`
}

// encoding/json/v2 writes a value with MarshalJSONTo, as TestMarshalJSONToFollowsTheToolchain
// has the generator expect, also a struct that gets the method.
func TestMarshalJSONToIsWritten(t *testing.T) {
	type thing struct {
		Raw jsonToRaw `json:"raw"`
	}
	got := []any{marshalled(t, &thing{}), marshalled(t, &jsonToHolder{N: 2})}
	if got[0].(map[string]any)["raw"] != "raw" || got[1] != "raw" {
		t.Fatalf("expected encoding/json to write MarshalJSONTo's output, got %v", got)
	}
}
