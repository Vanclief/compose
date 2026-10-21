package mcp

import (
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/reference"
)

type unitSummaryInput struct {
	UnitID int `json:"unit_id" jsonschema:"Unit to summarize"`
	Days   int `json:"days,omitempty"`
}

type UnitSummary struct {
	Packages int    `json:"packages"`
	Status   string `json:"status"`
}

// Unit collides with the Unit schema of the small spec.
type Unit struct {
	ID int `json:"id"`
}

func parseFile(t *testing.T, path string) *reference.Document {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	doc, err := reference.Parse(data)
	require.NoError(t, err)
	return doc
}

func unitSummary() Composite {
	c := NewComposite("units", "summary", func(ctx context.Context, call Caller, in unitSummaryInput) (UnitSummary, error) {
		return UnitSummary{}, nil
	})
	c.Summary = "Summarize a unit"
	c.Description = "Counts the packages of a unit."
	c.ReadOnly = true
	return c
}

func smallConcepts() map[string]Concept {
	return map[string]Concept{
		"Package":    {Text: "A parcel held at the front desk until the resident picks it up.", Aliases: []string{"paquete", "parcel"}},
		"Front desk": {Text: "The lobby counter.", Aliases: []string{"recepción"}},
	}
}

func smallCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := buildCatalog(parseFile(t, "testdata/small.json"), []Composite{unitSummary()}, smallConcepts())
	require.NoError(t, err)
	return c
}

func emmaCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := buildCatalog(parseFile(t, "../rest/reference/testdata/emma.json"), nil, nil)
	require.NoError(t, err)
	return c
}

func functionNames(c *Catalog) []string {
	var names []string
	for _, f := range c.Functions() {
		names = append(names, f.Name)
	}
	return names
}

func argNames(f *Function) []string {
	var names []string
	for _, a := range f.Args {
		names = append(names, a.Name+":"+a.In)
	}
	return names
}

func mustFunction(t *testing.T, c *Catalog, name string) *Function {
	t.Helper()
	f, ok := c.Function(name)
	require.True(t, ok, name)
	return f
}

func mustTerm(t *testing.T, c *Catalog, name string) *Term {
	t.Helper()
	term, ok := c.Term(name)
	require.True(t, ok, name)
	return term
}

func TestBuildCatalogModulesAndNames(t *testing.T) {
	c := smallCatalog(t)

	var modules []string
	for _, m := range c.Modules {
		modules = append(modules, m.Name)
	}
	assert.Equal(t, []string{"default", "packages", "units"}, modules)

	assert.Equal(t, []string{
		"default.get_health",
		"packages.delete_packages_by_id",
		"packages.get_package",
		"packages.list_packages",
		"packages.register_package",
		"packages.update_package",
		"units.set_notes",
		"units.summary",
	}, functionNames(c))

	packages, ok := c.Module("packages")
	require.True(t, ok)
	assert.Equal(t, "Packages received at the front desk.", packages.Description)
	units, ok := c.Module("units")
	require.True(t, ok)
	assert.Equal(t, "", units.Description)

	_, ok = c.Function("system.reindex")
	assert.False(t, ok, "x-internal operations are skipped")
}

func TestBuildCatalogSummaryAndFlags(t *testing.T) {
	c := smallCatalog(t)

	get := mustFunction(t, c, "packages.get_package")
	assert.Equal(t, "Get one package.", get.Summary)
	assert.Equal(t, "Get one package. Includes the carrier.", get.Description)
	assert.True(t, get.ReadOnly)
	assert.False(t, get.Destructive)

	health := mustFunction(t, c, "default.get_health")
	assert.Equal(t, "GET /health", health.Summary)

	del := mustFunction(t, c, "packages.delete_packages_by_id")
	assert.True(t, del.Destructive)
	assert.False(t, del.ReadOnly)
	assert.Equal(t, "DELETE", del.Method)
	assert.Equal(t, "/packages/{id}", del.Path)

	update := mustFunction(t, c, "packages.update_package")
	assert.True(t, update.Deprecated)
	assert.False(t, update.ReadOnly)
}

func TestBuildCatalogArgs(t *testing.T) {
	c := smallCatalog(t)

	list := mustFunction(t, c, "packages.list_packages")
	assert.Equal(t, []string{"limit:query", "status:query"}, argNames(list), "cookie parameters are skipped")
	assert.False(t, list.Args[0].Required)
	assert.Equal(t, "Only packages in this status", list.Args[1].Description)
	assert.Equal(t, mustTerm(t, c, "packages.Status").Schema, list.Args[1].Schema, "ref parameters share the term schema")

	register := mustFunction(t, c, "packages.register_package")
	assert.Equal(t, []string{"unit_id:body", "carrier:body", "notes:body"}, argNames(register), "allOf body is flattened")
	assert.True(t, register.Args[0].Required)
	assert.Equal(t, "Unit receiving the package", register.Args[0].Description)
	assert.False(t, register.Args[1].Required)
	assert.True(t, register.Args[2].Schema.Nullable)

	update := mustFunction(t, c, "packages.update_package")
	assert.Equal(t, []string{"id:path", "body:body"}, argNames(update), "a body property named like a parameter keeps the body whole")
	assert.True(t, update.bodyWhole)
	assert.False(t, register.bodyWhole)
	assert.True(t, update.Args[0].Required)
	assert.True(t, update.Args[1].Required)
	assert.Equal(t, mustTerm(t, c, "Package").Schema, update.Args[1].Schema)

	notes := mustFunction(t, c, "units.set_notes")
	assert.Equal(t, []string{"unit_id:path", "X-Request-Id:header", "body:body"}, argNames(notes), "a non-object body is one argument")
	assert.True(t, notes.bodyWhole)
	assert.False(t, notes.Args[2].Required)
	assert.Equal(t, "array", notes.Args[2].Schema.Type.Primary())

	del := mustFunction(t, c, "packages.delete_packages_by_id")
	assert.Equal(t, []string{"id:path"}, argNames(del))
}

func TestBuildCatalogReturns(t *testing.T) {
	c := smallCatalog(t)
	pkg := mustTerm(t, c, "Package")

	register := mustFunction(t, c, "packages.register_package")
	assert.Equal(t, "Package", register.Returns, "lowest 2xx wins over default")
	assert.Same(t, pkg.Schema, register.ReturnSchema)

	get := mustFunction(t, c, "packages.get_package")
	assert.Equal(t, "Package", get.Returns, "nullable wrapper around a $ref")
	assert.Same(t, pkg.Schema, get.ReturnSchema)

	list := mustFunction(t, c, "packages.list_packages")
	assert.Equal(t, "", list.Returns)
	require.NotNil(t, list.ReturnSchema)
	assert.Equal(t, "#/components/schemas/models_Package", list.ReturnSchema.Items.Ref)

	update := mustFunction(t, c, "packages.update_package")
	assert.Equal(t, "", update.Returns)
	require.NotNil(t, update.ReturnSchema)
	assert.Equal(t, []string{"ok"}, update.ReturnSchema.Properties.Keys)

	del := mustFunction(t, c, "packages.delete_packages_by_id")
	assert.Equal(t, "", del.Returns)
	assert.Nil(t, del.ReturnSchema)

	health := mustFunction(t, c, "default.get_health")
	assert.Equal(t, "ErrorResponse", health.Returns, "default when there is no 2xx")
}

func TestBuildCatalogTerms(t *testing.T) {
	c := smallCatalog(t)

	var names []string
	for _, term := range c.Terms {
		names = append(names, term.Name)
	}
	assert.Equal(t, []string{
		"ErrorResponse",
		"Front desk",
		"Package",
		"RegisterPackageRequest",
		"Unit",
		"UnitRef",
		"UnitSummary",
		"packages.Status",
		"units.Status",
	}, names)

	register := mustTerm(t, c, "RegisterPackageRequest")
	assert.Equal(t, []string{"unit_id", "carrier", "notes"}, register.Schema.Properties.Keys, "term schemas are flattened")

	pkg := mustTerm(t, c, "package")
	assert.Equal(t, "Package", pkg.Name)
	assert.Equal(t, []string{"packages.update_package"}, pkg.AcceptedBy)
	assert.Equal(t, []string{"packages.get_package", "packages.list_packages", "packages.register_package"}, pkg.ReturnedBy)

	status := mustTerm(t, c, "packages.Status")
	assert.Equal(t, []string{"packages.list_packages"}, status.AcceptedBy)
	assert.Empty(t, status.ReturnedBy)

	assert.Equal(t, []string{"packages.register_package"}, register.AcceptedBy, "a flattened body still lists its term")
	assert.Equal(t, []string{"units.set_notes"}, mustTerm(t, c, "units.Status").ReturnedBy)
	assert.Equal(t, []string{"default.get_health"}, mustTerm(t, c, "ErrorResponse").ReturnedBy, "default responses only count without a 2xx")
	assert.Empty(t, mustTerm(t, c, "Unit").AcceptedBy, "nested refs are not direct references")
}

func TestBuildCatalogConcepts(t *testing.T) {
	c := smallCatalog(t)

	pkg := mustTerm(t, c, "paquete")
	assert.Equal(t, "Package", pkg.Name)
	assert.Equal(t, "A parcel held at the front desk until the resident picks it up.", pkg.Description)
	assert.Equal(t, []string{"paquete", "parcel"}, pkg.Aliases)
	assert.NotNil(t, pkg.Schema)
	same := mustTerm(t, c, " PARCEL ")
	assert.Same(t, pkg, same)

	desk := mustTerm(t, c, "Recepción")
	assert.Equal(t, "Front desk", desk.Name)
	assert.Equal(t, "The lobby counter.", desk.Description)
	assert.Nil(t, desk.Schema)

	unit := mustTerm(t, c, "Unit")
	assert.Equal(t, "", unit.Description, "terms without a concept keep the schema description")

	concepts := map[string]Concept{"Unit": {Aliases: []string{"apartment"}}}
	c2, err := buildCatalog(parseFile(t, "testdata/small.json"), nil, concepts)
	require.NoError(t, err)
	assert.Equal(t, "", mustTerm(t, c2, "apartment").Description, "empty concept text keeps the schema description")
}

func TestBuildCatalogComposite(t *testing.T) {
	c := smallCatalog(t)

	f := mustFunction(t, c, "units.summary")
	assert.True(t, f.Composite)
	assert.True(t, f.ReadOnly)
	assert.Equal(t, "Summarize a unit", f.Summary)
	assert.Equal(t, "Counts the packages of a unit.", f.Description)
	assert.Equal(t, "", f.Method)
	assert.Equal(t, []string{"unit_id:body", "days:body"}, argNames(f))
	assert.True(t, f.Args[0].Required)
	assert.Equal(t, "Unit to summarize", f.Args[0].Description)
	assert.False(t, f.Args[1].Required)
	assert.Equal(t, "UnitSummary", f.Returns)
	require.NotNil(t, f.composite)
	assert.Equal(t, "summary", f.composite.Name)

	term := mustTerm(t, c, "UnitSummary")
	assert.Same(t, term.Schema, f.ReturnSchema)
	assert.Equal(t, []string{"packages", "status"}, term.Schema.Properties.Keys)
	assert.Equal(t, []string{"units.summary"}, term.ReturnedBy)

	list := NewComposite("units", "counts", func(ctx context.Context, call Caller, in struct{}) ([]int, error) {
		return nil, nil
	})
	again := NewComposite("units", "summary_again", func(ctx context.Context, call Caller, in *unitSummaryInput) (*UnitSummary, error) {
		return nil, nil
	})
	c2, err := buildCatalog(parseFile(t, "testdata/small.json"), []Composite{unitSummary(), list, again}, nil)
	require.NoError(t, err)
	counts := mustFunction(t, c2, "units.counts")
	assert.Empty(t, counts.Args)
	assert.Equal(t, "", counts.Returns)
	require.NotNil(t, counts.ReturnSchema)
	assert.Equal(t, "array", counts.ReturnSchema.Type.Primary())
	assert.Equal(t, "UnitSummary", mustFunction(t, c2, "units.summary_again").Returns, "composites may share an output type")
	assert.Equal(t, []string{"unit_id:body", "days:body"}, argNames(mustFunction(t, c2, "units.summary_again")))
	assert.Equal(t, []string{"units.summary", "units.summary_again"}, mustTerm(t, c2, "UnitSummary").ReturnedBy)

	m := NewComposite("reports", "daily", func(ctx context.Context, call Caller, in struct{}) (string, error) {
		return "", nil
	})
	c3, err := buildCatalog(parseFile(t, "testdata/small.json"), []Composite{m}, nil)
	require.NoError(t, err)
	reports, ok := c3.Module("reports")
	require.True(t, ok, "composites create their module")
	assert.Equal(t, "", reports.Description)
	assert.Equal(t, "string", mustFunction(t, c3, "reports.daily").ReturnSchema.Type.Primary())
}

func TestBuildCatalogErrors(t *testing.T) {
	doc := parseFile(t, "testdata/small.json")
	run := func(ctx context.Context, call Caller, in struct{}) (string, error) {
		return "", nil
	}

	cases := map[string]struct {
		composites []Composite
		concepts   map[string]Concept
	}{
		"duplicate function": {composites: []Composite{NewComposite("packages", "list_packages", run)}},
		"duplicate composite": {composites: []Composite{
			NewComposite("units", "x", run),
			NewComposite("units", "x", run),
		}},
		"camel case name": {composites: []Composite{NewComposite("units", "doThing", run)}},
		"empty module":    {composites: []Composite{NewComposite("", "thing", run)}},
		"dotted name":     {composites: []Composite{NewComposite("units", "a.b", run)}},
		"output collides": {composites: []Composite{NewComposite("units", "unit", func(ctx context.Context, call Caller, in struct{}) (Unit, error) {
			return Unit{}, nil
		})}},
		"alias on two terms": {concepts: map[string]Concept{
			"Package": {Aliases: []string{"box"}},
			"Unit":    {Aliases: []string{"box"}},
		}},
		"alias names another term": {concepts: map[string]Concept{"Package": {Aliases: []string{"unit"}}}},
		"empty concept name":       {concepts: map[string]Concept{" ": {Text: "x"}}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := buildCatalog(doc, tc.composites, tc.concepts)
			require.Error(t, err)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		})
	}

	duplicate := `{"openapi": "3.1.0", "info": {"title": "x", "version": "1"}, "paths": {
		"/a": {"get": {"operationId": "things.listThings", "responses": {}}},
		"/b": {"get": {"operationId": "things.list_things", "responses": {}}}
	}}`
	dupDoc, err := reference.Parse([]byte(duplicate))
	require.NoError(t, err)
	_, err = buildCatalog(dupDoc, nil, nil)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Contains(t, err.Error(), "things.list_things")
}

func TestDisplayNames(t *testing.T) {
	names := displayNames([]string{
		"app_finance_billing_ListResponse",
		"app_finance_units_ListResponse",
		"app_property_units_ListResponse",
		"ListResponse",
		"pkg.Error",
		"Profile",
		"profile",
		"a_b",
		"a.b",
	})
	assert.Equal(t, map[string]string{
		"app_finance_billing_ListResponse": "billing.ListResponse",
		"app_finance_units_ListResponse":   "finance.units.ListResponse",
		"app_property_units_ListResponse":  "property.units.ListResponse",
		"ListResponse":                     "ListResponse",
		"pkg.Error":                        "Error",
		"Profile":                          "Profile",
		"profile":                          "profile",
		"a_b":                              "a_b",
		"a.b":                              "a.b",
	}, names)
}

func TestBuildCatalogEmma(t *testing.T) {
	doc := parseFile(t, "../rest/reference/testdata/emma.json")
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)

	assert.Len(t, c.Modules, 16)
	// The fixture has 303 operations and none is x-internal; the 7 whose
	// request body is multipart/form-data only are left out.
	assert.Len(t, doc.Operations(), 303)
	assert.Len(t, c.Functions(), 296)

	shape := regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)
	seen := map[string]bool{}
	for _, f := range c.Functions() {
		assert.Regexp(t, shape, f.Name)
		assert.False(t, seen[f.Name], f.Name)
		seen[f.Name] = true
		if f.Returns != "" {
			_, ok := c.Term(f.Returns)
			assert.True(t, ok, "%s returns %s", f.Name, f.Returns)
		}
	}

	assert.Len(t, c.Terms, 633)
	termNames := map[string]bool{}
	for _, term := range c.Terms {
		key := normalizeTerm(term.Name)
		assert.False(t, termNames[key], term.Name)
		termNames[key] = true
	}

	f := mustFunction(t, c, "finance.list_billing_profiles")
	assert.Equal(t, []string{"limit:query", "cursor:query", "cluster_id:query", "unit_id:query", "user_id:query"}, argNames(f))
	for _, a := range f.Args {
		assert.False(t, a.Required, a.Name)
	}
	assert.NotEmpty(t, f.Returns)
	assert.True(t, f.ReadOnly)
	returned := mustTerm(t, c, f.Returns)
	assert.Contains(t, returned.ReturnedBy, f.Name)
	assert.Contains(t, returned.Schema.Properties.Keys, "profiles", "allOf responses are flattened")

	login := mustFunction(t, c, "auth.login")
	assert.Equal(t, "POST", login.Method)
	assert.NotEmpty(t, login.Args)
	assert.NotEmpty(t, login.Returns)

	access, ok := c.Module("access_control")
	require.True(t, ok)
	assert.Len(t, access.Functions, 24)
	_, ok = c.Function("finance.import_expenses")
	assert.False(t, ok, "multipart-only operations are left out")
}

// parseSpec parses an inline document.
func parseSpec(t *testing.T, spec string) *reference.Document {
	t.Helper()
	doc, err := reference.Parse([]byte(spec))
	require.NoError(t, err)
	return doc
}

func TestBuildCatalogWithoutOperationIDs(t *testing.T) {
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "x", "version": "1"}, "paths": {
		"/users": {"get": {"responses": {}}},
		"/users/{id}": {"get": {"responses": {}}},
		"/users/{id}/posts/{post_id}": {"get": {"responses": {}}}
	}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"default.get_users",
		"default.get_users_by_id",
		"default.get_users_by_id_posts_by_post_id",
	}, functionNames(c))
}

func TestBuildCatalogTermsDifferingInCase(t *testing.T) {
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "x", "version": "1"}, "paths": {}, "components": {"schemas": {
		"user": {"type": "object", "description": "lower"},
		"User": {"type": "object", "description": "upper"}
	}}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "lower", mustTerm(t, c, "user").Description)
	assert.Equal(t, "upper", mustTerm(t, c, "User").Description)
	assert.Equal(t, "lower", mustTerm(t, c, "USER").Description, "the lowercase key stays with the first term")
	assert.Equal(t, "upper", mustTerm(t, c, "#/components/schemas/User").Description)
}

func TestBuildCatalogBodyAndHeaderArgs(t *testing.T) {
	doc := parseSpec(t, `{"openapi": "3.1.0", "info": {"title": "x", "version": "1"}, "paths": {
		"/items/{id}": {"put": {"operationId": "items.update", "parameters": [
			{"name": "id", "in": "path", "required": true, "schema": {"type": "integer"}},
			{"name": "Accept", "in": "header", "schema": {"type": "string"}},
			{"name": "content-type", "in": "header", "schema": {"type": "string"}},
			{"name": "AUTHORIZATION", "in": "header", "schema": {"type": "string"}},
			{"name": "X-Trace", "in": "header", "schema": {"type": "string"}}
		], "requestBody": {"content": {"application/json; charset=utf-8": {"schema": {"$ref": "#/components/schemas/Item"}}}}, "responses": {}}},
		"/wrapped": {"post": {"operationId": "items.wrap", "requestBody": {"content": {"application/json": {"schema": {"type": "object", "properties": {
			"body": {"type": "string"}
		}}}}}, "responses": {}}},
		"/upload": {"post": {"operationId": "items.upload", "requestBody": {"content": {"multipart/form-data": {"schema": {"type": "object", "properties": {
			"file": {"type": "string", "format": "binary"}
		}}}}}, "responses": {}}}
	}, "components": {"schemas": {
		"Created": {"type": "string", "readOnly": true},
		"Item": {"type": "object", "properties": {
			"id": {"type": "integer", "readOnly": true},
			"created_at": {"$ref": "#/components/schemas/Created"},
			"name": {"type": "string"}
		}}
	}}}`)
	c, err := buildCatalog(doc, nil, nil)
	require.NoError(t, err)

	update := mustFunction(t, c, "items.update")
	assert.Equal(t, []string{"id:path", "X-Trace:header", "name:body"}, argNames(update), "readOnly properties and dispatch headers are not arguments")
	assert.False(t, update.bodyWhole, "a readOnly property named like a parameter does not keep the body whole")
	assert.Same(t, doc.Paths.Map["/items/{id}"].Put.Parameters[0], update.Args[0].param)
	assert.NotNil(t, update.Args[1].param)
	assert.Nil(t, update.Args[2].param)

	wrap := mustFunction(t, c, "items.wrap")
	assert.Equal(t, []string{"body:body"}, argNames(wrap))
	assert.False(t, wrap.bodyWhole, "a property named body is still a property")

	_, ok := c.Function("items.upload")
	assert.False(t, ok, "operations without a JSON request body are left out")
}
