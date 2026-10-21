package generator

import (
	"reflect"
	"strings"
	"testing"
)

// Echo reads a path parameter's name up to the next "/", so "-" and "." belong to it.
func TestPathParametersKeepEchoNames(t *testing.T) {
	cases := map[string]string{
		"/users/:user-id":         "/users/{user-id}",
		"/files/:name.ext":        "/files/{name.ext}",
		"/users/:user-id/files/*": "/users/{user-id}/files/{path}",
	}
	for echoPath, want := range cases {
		got := toOpenAPIPath(echoPath)
		if got != want {
			t.Errorf("toOpenAPIPath(%q) = %q, want %q", echoPath, got, want)
		}
	}

	files := map[string]string{}
	for name, contents := range requiredFixture {
		files[name] = contents
	}
	files["api/routes.go"] = `package api

func addRoutes(e *echo.Echo, h *Handler) {
	e.GET("/invoices/:invoice-id", h.GetInvoice)
	e.POST("/files/:name.ext", h.CreateInvoice)
}
`
	root := writeFixture(t, files)
	spec, err := Generate(Config{Root: root, RoutesGlob: "api/routes.go", HandlerGlob: "api/*.go"})
	if err != nil {
		t.Fatal(err)
	}

	wantParams := map[string][]string{
		"get /invoices/{invoice-id}": {"invoice-id"},
		"post /files/{name.ext}":     {"name.ext"},
	}
	for key, want := range wantParams {
		method, path, _ := strings.Cut(key, " ")
		got := parameterNames(operation(t, spec.Document, path, method))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: expected parameters %v, got %v", key, want, got)
		}
	}
}
