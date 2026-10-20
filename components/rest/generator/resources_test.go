package generator

import (
	"reflect"
	"strings"
	"testing"
)

// shopFixture has a shop namespace with two resources, carts and orders, and a stock namespace
// whose items resource collides by name with shop's.
var shopFixture = map[string]string{
	"go.mod": "module example.com/app\n\ngo 1.22\n",
	"api/handlers.go": `package api

import (
	"example.com/app/api/handler"
	"example.com/app/api/handler/shop"
	"example.com/app/api/handler/stock"
)

type Handlers struct {
	*handler.Base
	Shop  *shop.Handler
	Stock *stock.Handler
}
`,
	"api/routes.go": `package api

func addRoutes(e *echo.Echo, h *Handlers) {
	e.POST("/carts", h.Shop.CreateCart)
	e.GET("/carts/:id", h.Shop.GetCart)
	e.POST("/orders", h.Shop.CreateOrder)
	e.GET("/orders/:id", h.Shop.GetOrder)
	e.GET("/orders/:id/cart", h.Shop.GetOrderCart)
	e.GET("/shop/items", h.Shop.ListItems)
	e.GET("/stock/items", h.Stock.ListItems)
}
`,
	"api/handler/shop/shop.go": `package shop

import (
	"github.com/labstack/echo/v4"

	"example.com/app/api/handler"
	"example.com/app/modules/shop/carts"
	"example.com/app/modules/shop/items"
	"example.com/app/modules/shop/orders"
)

type Handler struct{ *handler.Base }

func (h *Handler) CreateCart(c echo.Context) error {
	return h.BindedJSONResponse(c, "Shop.CreateCart", nil, &carts.CreateRequest{})
}

func (h *Handler) GetCart(c echo.Context) error {
	return h.JSONResponse(c, "Shop.GetCart", nil, &carts.GetRequest{})
}

func (h *Handler) CreateOrder(c echo.Context) error {
	return h.BindedJSONResponse(c, "Shop.CreateOrder", nil, &orders.CreateRequest{})
}

func (h *Handler) GetOrder(c echo.Context) error {
	return h.JSONResponse(c, "Shop.GetOrder", nil, &orders.GetRequest{})
}

func (h *Handler) GetOrderCart(c echo.Context) error {
	return h.JSONResponse(c, "Shop.GetOrderCart", nil, &orders.CartRequest{})
}

func (h *Handler) ListItems(c echo.Context) error {
	return h.JSONResponse(c, "Shop.ListItems", nil, &items.ListRequest{})
}
`,
	"api/handler/stock/stock.go": `package stock

import (
	"github.com/labstack/echo/v4"

	"example.com/app/api/handler"
	"example.com/app/modules/stock/items"
)

type Handler struct{ *handler.Base }

func (h *Handler) ListItems(c echo.Context) error {
	return h.JSONResponse(c, "Stock.ListItems", nil, &items.ListRequest{})
}
`,
	"modules/shop/carts/carts.go": `package carts

import (
	"context"

	"example.com/app/models/shop"
)

type CreateRequest struct {
	Note string ` + "`json:\"note\"`" + `
}

type GetRequest struct {
	ID int64 ` + "`json:\"id\"`" + `
}

func Create(ctx context.Context, request *CreateRequest) (*shop.Cart, error) { return nil, nil }

func Get(ctx context.Context, request *GetRequest) (*shop.Cart, error) { return nil, nil }
`,
	"modules/shop/orders/orders.go": `package orders

import (
	"context"

	"example.com/app/models/shop"
)

type CreateRequest struct {
	CartID int64 ` + "`json:\"cart_id\"`" + `
}

type GetRequest struct {
	ID int64 ` + "`json:\"id\"`" + `
}

type CartRequest struct {
	ID int64 ` + "`json:\"id\"`" + `
}

func Create(ctx context.Context, request *CreateRequest) (*shop.Order, error) { return nil, nil }

func Get(ctx context.Context, request *GetRequest) (*shop.Order, error) { return nil, nil }

func Cart(ctx context.Context, request *CartRequest) (*shop.Cart, error) { return nil, nil }
`,
	"modules/shop/items/items.go": `package items

import "context"

type ListRequest struct{}

type ListResponse struct {
	Count int64 ` + "`json:\"count\"`" + `
}

func List(ctx context.Context, request *ListRequest) (*ListResponse, error) { return nil, nil }
`,
	"modules/stock/items/items.go": `package items

import "context"

type ListRequest struct{}

type ListResponse struct {
	Count int64 ` + "`json:\"count\"`" + `
}

func List(ctx context.Context, request *ListRequest) (*ListResponse, error) { return nil, nil }
`,
	"models/shop/shop.go": `package shop

import "github.com/vanclief/compose/primitives/enums"

type CartState string

const (
	OpenCart   CartState = "open"
	ClosedCart CartState = "closed"
)

var cartStates = enums.Set([]CartState{OpenCart, ClosedCart})

type Money int64

type Cart struct {
	ID    int64     ` + "`json:\"id\"`" + `
	State CartState ` + "`json:\"state\"`" + `
	Total Money     ` + "`json:\"total\"`" + `
}

type Order struct {
	ID    int64 ` + "`json:\"id\"`" + `
	Total Money ` + "`json:\"total\"`" + `
}
`,
}

func shopConfig(root string) Config {
	return Config{
		Root:         root,
		RoutesGlob:   "api/routes.go",
		HandlersFile: "api/handlers.go",
		HandlerGlob:  "api/handler/*/*.go",
		Info:         Info{Title: "Shop", Version: "1.0"},
	}
}

func generateShop(t *testing.T, configure func(*Config)) map[string]any {
	t.Helper()

	cfg := shopConfig(writeFixture(t, shopFixture))
	if configure != nil {
		configure(&cfg)
	}
	spec, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}

	return spec.Document
}

func schemaXTags(t *testing.T, document map[string]any, name string) []string {
	t.Helper()

	schema, ok := componentSchemas(document)[name]
	if !ok {
		t.Fatalf("expected schema %s, got %v", name, sortedKeys(componentSchemas(document)))
	}
	tags, _ := schema["x-tags"].([]string)

	return tags
}

func TestResourceTags(t *testing.T) {
	document := generateShop(t, nil)

	wantTags := []any{
		map[string]any{"name": "Shop"},
		map[string]any{"name": "Stock"},
		map[string]any{"name": "Carts", "x-parent": "Shop"},
		map[string]any{"name": "Orders", "x-parent": "Shop"},
		map[string]any{"name": "Shop items", "x-parent": "Shop"},
		map[string]any{"name": "Stock items", "x-parent": "Stock"},
	}
	if !reflect.DeepEqual(document["tags"], wantTags) {
		t.Fatalf("expected a child tag per resource, colliding names qualified by their parent, got %v", document["tags"])
	}

	cases := map[string][]string{
		// The resource's own schemas.
		"modules_shop_carts_CreateRequest": {"Carts"},
		"modules_shop_orders_CartRequest":  {"Orders"},
		"modules_shop_items_ListResponse":  {"Shop items"},
		"modules_stock_items_ListResponse": {"Stock items"},
		// Cart is returned by carts and orders: the resource named like it wins.
		"models_shop_Cart":  {"Carts"},
		"models_shop_Order": {"Orders"},
		// An enum only Cart uses joins its resource.
		"models_shop_CartState": {"Carts"},
		// A scalar both resources use stays under the operation tag.
		"models_shop_Money": {"Shop"},
	}
	for name, want := range cases {
		got := schemaXTags(t, document, name)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: expected x-tags %v, got %v", name, want, got)
		}
	}

	for _, op := range []map[string]any{operation(t, document, "/carts", "post"), operation(t, document, "/orders/{id}", "get")} {
		if !reflect.DeepEqual(op["tags"], []string{"Shop"}) {
			t.Fatalf("expected operation tags untouched, got %v", op["tags"])
		}
	}
}

func TestResourceTagHook(t *testing.T) {
	var entities []string
	document := generateShop(t, func(cfg *Config) {
		cfg.ResourceTag = func(pkgPath string, entity string, name string) string {
			entities = append(entities, entity)
			switch name {
			case "Orders":
				return "Purchases"
			case "Carts":
				return ""
			}
			return name
		}
	})

	tags := document["tags"].([]any)
	names := map[string]bool{}
	for _, tag := range tags {
		names[tag.(map[string]any)["name"].(string)] = true
	}
	if !names["Purchases"] || names["Orders"] || names["Carts"] {
		t.Fatalf("expected the hook's names, got %v", tags)
	}
	if !reflect.DeepEqual(schemaXTags(t, document, "modules_shop_carts_CreateRequest"), []string{"Shop"}) {
		t.Fatal("expected a dropped resource's schemas under their operation tag")
	}
	if !reflect.DeepEqual(schemaXTags(t, document, "models_shop_Order"), []string{"Purchases"}) {
		t.Fatal("expected the renamed resource's entity")
	}
	found := map[string]bool{}
	for _, entity := range entities {
		found[entity] = true
	}
	if !found["Cart"] || !found["Order"] || !found[""] {
		t.Fatalf("expected the hook to see each resource's entity, got %v", entities)
	}
}

func TestResourceName(t *testing.T) {
	cases := []struct {
		entity, pkgPath, want string
	}{
		{"BankAccount", "app/modules/finance/bankaccounts", "Bank accounts"},
		{"APIKey", "app/modules/keys", "API keys"},
		{"Category", "app/modules/categories", "Categories"},
		{"Address", "app/modules/addresses", "Addresses"},
		{"Day", "app/modules/days", "Days"},
		{"", "app/modules/finance/overview", "Overview"},
		{"", "app/modules/unit_finances", "Unit finances"},
	}
	for _, c := range cases {
		got := resourceName(c.entity, c.pkgPath)
		if got != c.want {
			t.Fatalf("resourceName(%q, %q) = %q, want %q", c.entity, c.pkgPath, got, c.want)
		}
	}
}

func TestOpenAPIAtLeast(t *testing.T) {
	cases := map[string]bool{"3.0.3": false, "3.1.0": false, "3.2.0": true, "4.0": true, "": false, "x.y": false}
	for version, want := range cases {
		if openAPIAtLeast(version, 3, 2) != want {
			t.Fatalf("openAPIAtLeast(%q, 3, 2) != %v", version, want)
		}
	}
}

// Resource tags come from the Go types, so a namer that drops the package (and the "_"
// separator goTypeName used to split on) names them the same.
func TestResourceTagsWithSchemaNameHook(t *testing.T) {
	document := generateShop(t, func(cfg *Config) {
		cfg.SchemaName = func(importPath string, typeName string) string {
			return strings.ReplaceAll(strings.TrimPrefix(importPath, "example.com/"), "/", "") + typeName
		}
	})

	want := generateShop(t, nil)["tags"]
	if !reflect.DeepEqual(document["tags"], want) {
		t.Fatalf("expected the default naming's tags %v, got %v", want, document["tags"])
	}
}
