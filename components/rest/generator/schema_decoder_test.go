package generator

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

const testHandlerPkgPath = "example.com/mod/application/interfaces/rest/handler/crm"

func parseHandlerPackageSource(t *testing.T, source string) (map[string]string, map[string]funcInfo, *ast.BlockStmt) {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), "webhook.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	imports := newTestSchemaBuilder(t, t.TempDir()).importMap(file)
	helpers := map[string]funcInfo{}
	var handlerBody *ast.BlockStmt

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Recv == nil {
			helpers[fn.Name.Name] = funcInfo{Decl: fn, Imports: imports}
			continue
		}
		handlerBody = fn.Body
	}

	if handlerBody == nil {
		t.Fatalf("expected the source to declare a handler method")
	}

	return imports, helpers, handlerBody
}

func TestRequestTypeFromHandlerPrefersDecoderWireType(t *testing.T) {
	source := `package crm

import (
	"encoding/json"

	"example.com/mod/leads"
)

type webhookEnvelope struct {
	TriggerEvent string ` + "`json:\"triggerEvent\"`" + `
}

func decodeWebhook(raw []byte) (*leads.Request, error) {
	envelope := webhookEnvelope{}
	err := json.Unmarshal(raw, &envelope)
	if err != nil {
		return nil, err
	}

	return &leads.Request{}, nil
}

func (h *Handler) Webhook(c echo.Context) error {
	body, err := decodeWebhook(nil)
	if err != nil {
		return err
	}

	return h.JSONResponse(c, "Crm.Webhook", request, body)
}
`
	imports, helpers, handlerBody := parseHandlerPackageSource(t, source)

	ref, ok := newTestGenerator(t).requestTypeFromHandler(handlerBody, imports, testHandlerPkgPath, helpers)
	if !ok {
		t.Fatalf("expected the handler request type to resolve through the decoder")
	}

	want := typeRef{ImportPath: testHandlerPkgPath, Name: "webhookEnvelope"}
	if ref != want {
		t.Fatalf("expected the wire envelope %+v, got %+v", want, ref)
	}
}

func TestTypeRefFromHelperCallRejectsDecoderWithoutUnmarshal(t *testing.T) {
	source := `package crm

import "example.com/mod/leads"

func decodeWebhook(raw []byte) (*leads.Request, error) {
	return &leads.Request{}, nil
}

func (h *Handler) Webhook(c echo.Context) error {
	body, err := decodeWebhook(nil)
	if err != nil {
		return err
	}

	return h.JSONResponse(c, "Crm.Webhook", request, body)
}
`
	_, helpers, _ := parseHandlerPackageSource(t, source)

	call := &ast.CallExpr{Fun: ast.NewIdent("decodeWebhook")}

	ref, ok := typeRefFromHelperCall(call, helpers, testHandlerPkgPath)
	if ok {
		t.Fatalf("expected a decoder without an unmarshal target to stay unresolved, got %+v", ref)
	}
}

func TestSchemaForAnyFieldIsUnconstrained(t *testing.T) {
	repoRoot := t.TempDir()
	pkgDir := filepath.Join(repoRoot, "application", "models", "box")

	err := os.MkdirAll(pkgDir, 0o755)
	if err != nil {
		t.Fatal(err)
	}

	source := "package box\n\ntype Box struct {\n\tValue any `json:\"value\"`\n}\n"
	err = os.WriteFile(filepath.Join(pkgDir, "box.go"), []byte(source), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	builder := newTestSchemaBuilder(t, repoRoot)
	builder.schemaRef(typeRef{ImportPath: "example.com/app/application/models/box", Name: "Box"})

	err = builder.unresolvedError()
	if err != nil {
		t.Fatalf("expected any to resolve as an unconstrained schema, got %v", err)
	}

	schema := builder.schemas["application_models_box_Box"]
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties on the Box schema, got %v", schema)
	}

	value, ok := properties["value"].(map[string]any)
	if !ok || len(value) != 0 {
		t.Fatalf("expected value to be an unconstrained schema, got %v", properties["value"])
	}
}
