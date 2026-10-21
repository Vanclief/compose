package mcp

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/vanclief/compose/components/rest/reference"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// golden compares got with testdata/<name>.golden, rewriting the file when
// the test runs with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		err := os.WriteFile(path, []byte(got), 0o644)
		require.NoError(t, err)
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run go test ./components/mcp/ -update to create %s", path)
	assert.Equal(t, string(want), got)
}

// assertClean fails when a line of out ends in whitespace.
func assertClean(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		assert.Equal(t, strings.TrimRight(line, " \t"), line, "trailing whitespace")
	}
}

func TestRenderSearchModules(t *testing.T) {
	c := smallCatalog(t)
	assert.Equal(t, `default  1 function
packages  5 functions  Packages received at the front desk.
units  2 functions`, c.renderSearch("", ""))
	assert.Equal(t, c.renderSearch("", ""), c.renderSearch(" ", "  "))
}

func TestRenderSearchModule(t *testing.T) {
	c := smallCatalog(t)
	assert.Equal(t, `module packages: Packages received at the front desk.
  packages.delete_packages_by_id  DELETE /packages/{id}
  packages.get_package  Get one package.  -> Package
  packages.list_packages  List packages  -> Package[]
  packages.register_package  Register a package at the front desk.  -> Package
  packages.update_package  Update a package [deprecated]`, c.renderSearch("packages", ""))

	assert.Equal(t, `module units
  units.set_notes  Replace the notes of a unit  -> units.Status
  units.summary  Summarize a unit  -> UnitSummary [composite]`, c.renderSearch("Units", ""))

	assert.Equal(t, `unknown module "nope"; modules: default, packages, units`, c.renderSearch("nope", ""))
	assert.Equal(t, `unknown module "nope"; modules: default, packages, units`, c.renderSearch("nope", "package"))
}

func TestRenderSearchQuery(t *testing.T) {
	c := smallCatalog(t)

	// Matches on an argument name (status) and on the return term.
	assert.Equal(t, `module packages
  packages.list_packages  List packages  -> Package[]
module units
  units.set_notes  Replace the notes of a unit  -> units.Status`, c.renderSearch("", " STATUS "))

	// Restricted to one module; matches on the description.
	assert.Equal(t, `module units
  units.summary  Summarize a unit  -> UnitSummary [composite]`, c.renderSearch("units", "package"))

	// Matches on the summary.
	assert.Equal(t, `module packages
  packages.register_package  Register a package at the front desk.  -> Package`, c.renderSearch("", "front desk"))

	// Every word must match, each in any field; underscores are spaces.
	assert.Equal(t, `module packages
  packages.register_package  Register a package at the front desk.  -> Package`, c.renderSearch("", "register package"))
	assert.Equal(t, c.renderSearch("", "register package"), c.renderSearch("", "register_package"))
	assert.Equal(t, c.renderSearch("", "register package"), c.renderSearch("", "desk register"))
	assert.Contains(t, c.renderSearch("", "register zzz"), "no functions match")

	assert.Equal(t, `no functions match "zzz"
modules: default, packages, units`, c.renderSearch("", " zzz "))
}

func TestRenderDescribe(t *testing.T) {
	c := smallCatalog(t)

	assert.Equal(t, `packages.register_package  POST /packages
  Register a package at the front desk.
  The resident is notified.
  args:
    unit_id  integer  required  Unit receiving the package
    carrier  string   enum: dhl|fedex|ups
    notes    string?
  returns Package: {id integer, unit_id integer, status packages.Status, received_at string date-time, unit Unit}
  see also: Package, RegisterPackageRequest, Unit, packages.Status`, c.renderDescribe([]string{"packages.register_package"}))

	assert.Equal(t, `packages.list_packages  GET /packages
  List packages
  args:
    limit   integer int32
    status  packages.Status  Only packages in this status
  returns Package[]
  see also: Package, packages.Status

packages.update_package  PATCH /packages/{id}  [deprecated]
  Update a package
  args:
    id    integer  required
    body  Package  required
  returns {ok boolean}
  see also: Package`, c.renderDescribe([]string{"packages.list_packages", " packages.update_package "}))

	assert.Equal(t, `units.set_notes  PUT /units/{unit_id}/notes
  Replace the notes of a unit
  args:
    unit_id       integer   required
    X-Request-Id  string
    body          string[]
  returns units.Status: string enum: occupied|vacant
  see also: units.Status`, c.renderDescribe([]string{"units.set_notes"}))

	assert.Equal(t, `units.summary  composite
  Summarize a unit
  Counts the packages of a unit.
  args:
    unit_id  integer  required  Unit to summarize
    days     integer
  returns UnitSummary: {packages integer, status string}
  see also: UnitSummary`, c.renderDescribe([]string{"units.summary"}))

	// The fallback summary repeats the header and is not printed again.
	assert.Equal(t, `packages.delete_packages_by_id  DELETE /packages/{id}
  args:
    id  integer  required`, c.renderDescribe([]string{"packages.delete_packages_by_id"}))

	// The description does not repeat the summary it starts with.
	assert.Equal(t, `packages.get_package  GET /packages/{id}
  Get one package.
  Includes the carrier.
  args:
    id  integer  required
  returns Package: {id integer, unit_id integer, status packages.Status, received_at string date-time, unit Unit}
  see also: Package, Unit, packages.Status`, c.renderDescribe([]string{"packages.get_package"}))

	assert.Equal(t, `default.get_health  GET /health
  returns ErrorResponse: {message string}
  see also: ErrorResponse`, c.renderDescribe([]string{"default.get_health"}))
}

func TestTrimSummary(t *testing.T) {
	cases := map[[2]string]string{
		{"Get units by their id, including archived ones.", "Get unit"}: "Get units by their id, including archived ones.",
		{"Get unit. Including archived ones.", "Get unit"}:              "Including archived ones.",
		{"Get unit\nIncluding archived ones.", "Get unit"}:              "Including archived ones.",
		{"Get unit", "Get unit"}:                                        "",
		{"Get unit: by id", "Get unit:"}:                                "by id",
		{"Something else", "Get unit"}:                                  "Something else",
		{"Get unit", ""}:                                                "Get unit",
	}
	for in, want := range cases {
		assert.Equal(t, want, trimSummary(in[0], in[1]), in)
	}
}

func TestRenderDescribeUnknown(t *testing.T) {
	c := smallCatalog(t)

	assert.Equal(t, `unknown function "packages.list"
  did you mean: packages.list_packages`, c.renderDescribe([]string{"packages.list"}))

	// The part after the dot is searched on its own, up to five names.
	assert.Equal(t, `unknown function "nope.package"
  did you mean: packages.delete_packages_by_id, packages.get_package, packages.list_packages, packages.register_package, packages.update_package`, c.renderDescribe([]string{"nope.package"}))

	assert.Equal(t, `unknown function "zzz"

units.summary  composite
  Summarize a unit
  Counts the packages of a unit.
  args:
    unit_id  integer  required  Unit to summarize
    days     integer
  returns UnitSummary: {packages integer, status string}
  see also: UnitSummary`, c.renderDescribe([]string{"zzz", "units.summary"}))
}

func TestRenderExplain(t *testing.T) {
	c := smallCatalog(t)

	assert.Equal(t, `Package  (aliases: paquete, parcel)
  A parcel held at the front desk until the resident picks it up.
  fields: id integer, unit_id integer, status packages.Status, received_at string date-time, unit Unit
  accepted by: packages.update_package
  returned by: packages.get_package, packages.list_packages, packages.register_package`, c.renderExplain([]string{"paquete"}))

	assert.Equal(t, `Front desk  (aliases: recepción)
  The lobby counter.

packages.Status
  (no description)
  type: string enum: pending|delivered
  accepted by: packages.list_packages

Unit
  (no description)
  fields: id integer, name string`, c.renderExplain([]string{"front desk", "packages.Status", "Unit"}))

	assert.Equal(t, `UnitSummary
  (no description)
  fields: packages integer, status string
  returned by: units.summary`, c.renderExplain([]string{"UnitSummary"}))

	assert.Equal(t, `unknown term "stat"
  did you mean: packages.Status, units.Status

unknown term "zzz"`, c.renderExplain([]string{"stat", "zzz"}))

	// Aliases are searched too.
	assert.Equal(t, `unknown term "parc"
  did you mean: Package`, c.renderExplain([]string{"parc"}))
}

func TestTypeName(t *testing.T) {
	c := smallCatalog(t)
	pkg := mustTerm(t, c, "Package")

	cases := map[string]struct {
		schema *reference.Schema
		want   string
	}{
		"nil":            {nil, "any"},
		"untyped":        {&reference.Schema{}, "any"},
		"term pointer":   {pkg.Schema, "Package"},
		"term ref":       {&reference.Schema{Ref: "#/components/schemas/models_Package", Nullable: true}, "Package?"},
		"unknown ref":    {&reference.Schema{Ref: "#/components/schemas/Missing"}, "Missing"},
		"allOf wrapper":  {&reference.Schema{AllOf: []*reference.Schema{{Ref: "#/components/schemas/models_Unit"}}, Nullable: true}, "Unit?"},
		"array of terms": {&reference.Schema{Type: reference.TypeList{"array"}, Items: &reference.Schema{Ref: "#/components/schemas/models_Unit"}}, "Unit[]"},
		"array of enums": {&reference.Schema{Type: reference.TypeList{"array"}, Items: &reference.Schema{Type: reference.TypeList{"string"}, Enum: []any{"a", "b"}}}, "string[] enum: a|b"},
		"format":         {&reference.Schema{Type: reference.TypeList{"string"}, Format: "date-time", Nullable: true}, "string date-time?"},
		"enum":           {&reference.Schema{Type: reference.TypeList{"integer"}, Enum: []any{1.0, 2.0}}, "integer enum: 1|2"},
		"inline object":  {&reference.Schema{Type: reference.TypeList{"object"}}, "object"},
		"untyped object": {&reference.Schema{Properties: reference.Ordered[*reference.Schema]{Keys: []string{"a"}, Map: map[string]*reference.Schema{"a": {}}}}, "object"},
		"oneOf": {&reference.Schema{OneOf: []*reference.Schema{
			{Ref: "#/components/schemas/models_Unit"},
			{Type: reference.TypeList{"string"}},
		}}, "Unit|string"},
		"anyOf": {&reference.Schema{AnyOf: []*reference.Schema{
			{Type: reference.TypeList{"integer"}},
			{Type: reference.TypeList{"boolean"}},
		}}, "integer|boolean"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, c.typeName(tc.schema))
		})
	}
}

func TestRenderEmmaGolden(t *testing.T) {
	c := emmaCatalog(t)

	f := mustFunction(t, c, "finance.list_billing_profiles")
	require.NotEmpty(t, f.Returns)

	outputs := map[string]string{
		"emma_search_modules": c.renderSearch("", ""),
		"emma_search_finance": c.renderSearch("finance", ""),
		"emma_search_billing": c.renderSearch("", "billing"),
		"emma_describe":       c.renderDescribe([]string{"finance.list_billing_profiles", "auth.login", "nope.missing"}),
		"emma_explain":        c.renderExplain([]string{f.Returns, "nope"}),
	}
	for name, got := range outputs {
		t.Run(name, func(t *testing.T) {
			assertClean(t, got)
			golden(t, name, got)
		})
	}
}

func TestRenderEmmaSearchLineLength(t *testing.T) {
	c := emmaCatalog(t)

	total := 0
	lines := 0
	for _, m := range c.Modules {
		out := c.renderSearch(m.Name, "")
		assertClean(t, out)
		for _, line := range strings.Split(out, "\n")[1:] {
			total += len(line)
			lines++
		}
	}
	// 303 operations, 7 of them multipart-only and left out.
	require.Equal(t, 296, lines)
	average := float64(total) / float64(lines)
	t.Logf("average search line: %.1f bytes over %d functions", average, lines)
	assert.Less(t, average, 120.0)
}

func TestRenderSmallClean(t *testing.T) {
	c := smallCatalog(t)
	var names []string
	for _, f := range c.Functions() {
		names = append(names, f.Name)
	}
	var terms []string
	for _, term := range c.Terms {
		terms = append(terms, term.Name)
	}
	assertClean(t, c.renderDescribe(names))
	assertClean(t, c.renderExplain(terms))
	assertClean(t, c.renderSearch("", "a"))
}
