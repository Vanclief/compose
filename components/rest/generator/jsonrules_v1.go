//go:build !goexperiment.jsonv2

package generator

// The JSON rules of encoding/json's own implementation, which a toolchain without the jsonv2
// experiment builds: Go 1.26 and earlier, and Go 1.27 with GOEXPERIMENT=nojsonv2.
// jsonrules_v2.go declares the same names for the implementation on encoding/json/v2.

// encodingJSONV2 reports the rules of encoding/json on encoding/json/v2.
const encodingJSONV2 = false

// jsonMethods are the methods encoding/json writes a value with as the JSON they return, and
// textMethods those it writes a value with as a JSON string, in the order it prefers them; a
// method of jsonMethods wins over one of textMethods.
var (
	jsonMethods = []string{"MarshalJSON"}
	textMethods = []string{"MarshalText"}
)

// jsonNumberMethods are json.Number's marshal methods: none, encoding/json writes a Number as a
// JSON number by its exact type.
var jsonNumberMethods = map[string]bool{}
