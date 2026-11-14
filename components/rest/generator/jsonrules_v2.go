//go:build goexperiment.jsonv2

package generator

// The JSON rules of encoding/json on encoding/json/v2, which a toolchain with the jsonv2
// experiment builds: Go 1.27 by default, as Go 1.27.2's encoding/json writes. jsonrules_v1.go
// declares the same names for encoding/json's own implementation.

// encodingJSONV2 reports the rules of encoding/json on encoding/json/v2.
const encodingJSONV2 = true

// jsonMethods are the methods encoding/json writes a value with as the JSON they return, and
// textMethods those it writes a value with as a JSON string, in the order it prefers them; a
// method of jsonMethods wins over one of textMethods.
var (
	jsonMethods = []string{"MarshalJSONTo", "MarshalJSON"}
	textMethods = []string{"AppendText", "MarshalText"}
)

// jsonNumberMethods are json.Number's marshal methods: encoding/json writes a Number with its
// MarshalJSONTo, which a struct embedding one gets.
var jsonNumberMethods = map[string]bool{"MarshalJSONTo": true}
