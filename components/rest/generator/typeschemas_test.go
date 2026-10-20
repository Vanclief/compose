package generator

import (
	"reflect"
	"strings"
	"testing"
)

// thingFixture routes GET /things/:id to a handler whose module function returns
// things.Thing, declared by thingsSource, plus extra files.
func thingFixture(thingsSource string, extra map[string]string) map[string]string {
	files := map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n\nrequire github.com/shopspring/decimal v1.0.0\n",
		"api/routes.go": `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("/things/:id", h.GetThing)
}
`,
		"api/handler.go": `package api

import (
	"github.com/labstack/echo/v4"

	"example.com/app/things"
)

type Handler struct{}

func (h *Handler) GetThing(c echo.Context) error {
	return h.JSONResponse(c, "GetThing", nil, &things.GetRequest{})
}
`,
		"things/things.go": thingsSource,
	}
	for name, contents := range extra {
		files[name] = contents
	}

	return files
}

func thingConfig(root string) Config {
	return Config{Root: root, RoutesGlob: "api/routes.go", HandlerGlob: "api/*.go"}
}

func TestTypeMappings(t *testing.T) {
	root := writeFixture(t, thingFixture(`package things

import (
	"context"
	"database/sql"
	"encoding/json"
	"math/big"
	"net"
	"time"

	"github.com/shopspring/decimal"
)

type Thing struct {
	Count    int             `+"`json:\"count\"`"+`
	Size     uint            `+"`json:\"size\"`"+`
	Small    int16           `+"`json:\"small\"`"+`
	Raw      json.RawMessage `+"`json:\"raw\"`"+`
	Anything interface{}     `+"`json:\"anything\"`"+`
	Timeout  time.Duration   `+"`json:\"timeout\"`"+`
	Seen     sql.NullTime    `+"`json:\"seen\"`"+`
	Rank     sql.NullInt32   `+"`json:\"rank\"`"+`
	Level    sql.NullInt16   `+"`json:\"level\"`"+`
	Flag     sql.NullByte    `+"`json:\"flag\"`"+`
	Address  net.IP          `+"`json:\"address\"`"+`
	Big      big.Int         `+"`json:\"big\"`"+`
	Price    decimal.Decimal `+"`json:\"price\"`"+`
}

type GetRequest struct{}

func Get(ctx context.Context, request *GetRequest) (*Thing, error) { return nil, nil }
`, nil))

	cfg := thingConfig(root)
	cfg.TypeSchemas = map[string]map[string]any{
		"github.com/shopspring/decimal.Decimal": {"type": "string", "format": "decimal"},
	}
	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	schemas := componentSchemas(spec.Document)
	properties := schemas["things_Thing"]["properties"].(map[string]any)

	want := map[string]any{
		"count":    map[string]any{"type": "integer", "format": "int64"},
		"size":     map[string]any{"type": "integer", "format": "int64"},
		"small":    map[string]any{"type": "integer", "format": "int32"},
		"raw":      map[string]any{},
		"anything": map[string]any{},
		"timeout":  map[string]any{"type": "integer", "format": "int64", "description": "nanoseconds"},
		"seen":     map[string]any{"type": "string", "format": "date-time", "nullable": true},
		"rank":     map[string]any{"type": "integer", "format": "int32", "nullable": true},
		"level":    map[string]any{"type": "integer", "format": "int32", "nullable": true},
		"flag":     map[string]any{"type": "integer", "format": "int32", "nullable": true},
		"address":  map[string]any{"type": "string"},
		"big":      map[string]any{"type": "integer"},
		"price":    map[string]any{"$ref": "#/components/schemas/github_com_shopspring_decimal_Decimal"},
	}
	for name, schema := range want {
		if !reflect.DeepEqual(properties[name], schema) {
			t.Errorf("%s: expected %v, got %v", name, schema, properties[name])
		}
	}
	if !reflect.DeepEqual(schemas["github_com_shopspring_decimal_Decimal"], map[string]any{"type": "string", "format": "decimal"}) {
		t.Fatalf("expected the TypeSchemas entry as the type's component, got %v", schemas["github_com_shopspring_decimal_Decimal"])
	}
	// Nullable mappings stay optional; the others are always serialized.
	required := schemas["things_Thing"]["required"].([]string)
	if containsString(required, "seen") || !containsString(required, "count") || !containsString(required, "price") {
		t.Fatalf("unexpected required list %v", required)
	}
}

func TestGenericInstantiationsAreUnresolved(t *testing.T) {
	source := `package things

import "context"

type Page[T any] struct {
	Items []T ` + "`json:\"items\"`" + `
}

type Pair[K comparable, V any] struct {
	Key K ` + "`json:\"key\"`" + `
	Val V ` + "`json:\"val\"`" + `
}

type Thing struct {
	Page Page[string]      ` + "`json:\"page\"`" + `
	Pair Pair[string, int] ` + "`json:\"pair\"`" + `
}

type GetRequest struct{}

func Get(ctx context.Context, request *GetRequest) (*Thing, error) { return nil, nil }
`
	root := writeFixture(t, thingFixture(source, nil))

	_, err := Generate(thingConfig(root))
	if err == nil {
		t.Fatal("expected generic instantiations to fail generation")
	}
	for _, want := range []string{"generic instantiation Page[string]", "generic instantiation Pair[string, int]", `Config.TypeSchemas["example.com/app/things.Page"]`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected %q in %v", want, err)
		}
	}

	cfg := thingConfig(root)
	cfg.TypeSchemas = map[string]map[string]any{
		"example.com/app/things.Page": {"type": "object", "properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{}}}},
		"example.com/app/things.Pair": {"type": "object"},
	}
	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	page := componentSchemas(spec.Document)["things_Thing"]["properties"].(map[string]any)["page"]
	if !reflect.DeepEqual(page, map[string]any{"$ref": "#/components/schemas/things_Page"}) {
		t.Fatalf("expected the described generic type's component, got %v", page)
	}
}

func TestCustomMarshalJSONMustBeDescribed(t *testing.T) {
	source := `package things

import (
	"context"
	"encoding/json"
)

type Status string

type Thing struct {
	ID     int64  ` + "`json:\"id\"`" + `
	Status Status ` + "`json:\"status_id\"`" + `
	Secret string ` + "`json:\"-\"`" + `
}

func (t Thing) MarshalJSON() ([]byte, error) {
	type alias Thing
	return json.Marshal(struct {
		alias
		Status struct {
			ID Status ` + "`json:\"id\"`" + `
		} ` + "`json:\"status\"`" + `
	}{alias: alias(t)})
}

type Wrapper struct {
	*Thing
	Extra string ` + "`json:\"extra\"`" + `
}

type GetRequest struct{}

func Get(ctx context.Context, request *GetRequest) (*Wrapper, error) { return nil, nil }
`
	root := writeFixture(t, thingFixture(source, nil))

	_, err := Generate(thingConfig(root))
	if err == nil {
		t.Fatal("expected a struct with MarshalJSON to fail generation")
	}
	if !strings.Contains(err.Error(), `type Wrapper embeds Thing, which defines MarshalJSON`) {
		t.Fatalf("expected the promoted MarshalJSON to be reported, got %v", err)
	}

	cfg := thingConfig(root)
	cfg.TypeSchemas = map[string]map[string]any{
		"example.com/app/things.Wrapper": {"type": "object"},
	}
	_, err = Generate(cfg)
	if err != nil {
		t.Fatalf("expected a described wrapper to cover its embedded type, got %v", err)
	}

	cfg.TypeSchemas = map[string]map[string]any{
		"example.com/app/things.Thing": ExtendGenerated(map[string]any{
			"properties": map[string]any{
				"status": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"$ref": "#/components/schemas/things_Status"}}},
			},
			"required":    []any{"status"},
			"description": "A thing.",
		}, "status_id"),
	}
	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	thing := componentSchemas(spec.Document)["things_Thing"]
	properties := thing["properties"].(map[string]any)
	if !reflect.DeepEqual(sortedKeys(properties), []string{"id", "status"}) {
		t.Fatalf("expected the derived id plus the extension's status, without status_id, got %v", sortedKeys(properties))
	}
	if !reflect.DeepEqual(thing["required"], []string{"id", "status"}) || thing["description"] != "A thing." {
		t.Fatalf("expected the extension's required names and keywords, got %v", thing)
	}
	_, leaked := thing[extendsGeneratedKey]
	if leaked {
		t.Fatal("expected the extension marker left out of the document")
	}
}
