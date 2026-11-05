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

	// Five functions or fewer fold: each shows its route, description and
	// arguments. A fallback summary is the route already and is not repeated.
	assert.Equal(t, `module packages: Packages received at the front desk.
  packages.delete_packages_by_id  DELETE /packages/{id}
    args:
      id  integer  required

  packages.get_package  Get one package.  -> Package
    GET /packages/{id}
    Includes the carrier.
    args:
      id  integer  required

  packages.list_packages  List packages  -> Package[]
    GET /packages
    args:
      limit   integer int32
      status  packages.Status  Only packages in this status

  packages.register_package  Register a package at the front desk.  -> Package
    POST /packages
    The resident is notified.
    args:
      unit_id  integer  required  Unit receiving the package
      carrier  string   enum: dhl|fedex|ups
      notes    string?

  packages.update_package  Update a package [deprecated]
    PATCH /packages/{id}
    args:
      id    integer  required
      body  Package  required`, c.renderSearch("packages", ""))

	assert.Equal(t, `module units
  units.set_notes  Replace the notes of a unit  -> units.Status
    PUT /units/{unit_id}/notes
    args:
      unit_id       integer   required
      X-Request-Id  string
      body          string[]

  units.summary  Summarize a unit  -> UnitSummary [composite]
    Counts the packages of a unit.
    args:
      unit_id  integer  required  Unit to summarize
      days     integer`, c.renderSearch("Units", ""))

	assert.Equal(t, `unknown module "nope"; modules: default, packages, units`, c.renderSearch("nope", ""))
	assert.Equal(t, `unknown module "nope"; modules: default, packages, units`, c.renderSearch("nope", "package"))
}

func TestRenderSearchQuery(t *testing.T) {
	c := smallCatalog(t)

	// Matches on an argument name (status) and on the return term. Folded
	// modules are separated by a blank line.
	assert.Equal(t, `module packages
  packages.list_packages  List packages  -> Package[]
    GET /packages
    args:
      limit   integer int32
      status  packages.Status  Only packages in this status

module units
  units.set_notes  Replace the notes of a unit  -> units.Status
    PUT /units/{unit_id}/notes
    args:
      unit_id       integer   required
      X-Request-Id  string
      body          string[]`, c.renderSearch("", " STATUS "))

	// Restricted to one module; matches on the description.
	assert.Equal(t, `module units
  units.summary  Summarize a unit  -> UnitSummary [composite]
    Counts the packages of a unit.
    args:
      unit_id  integer  required  Unit to summarize
      days     integer`, c.renderSearch("units", "package"))

	// Matches on the summary.
	assert.Equal(t, `module packages
  packages.register_package  Register a package at the front desk.  -> Package
    POST /packages
    The resident is notified.
    args:
      unit_id  integer  required  Unit receiving the package
      carrier  string   enum: dhl|fedex|ups
      notes    string?`, c.renderSearch("", "front desk"))

	// More than five matches keep one dense line per function.
	assert.Equal(t, `module packages
  packages.delete_packages_by_id  DELETE /packages/{id}
  packages.get_package  Get one package.  -> Package
  packages.list_packages  List packages  -> Package[]
  packages.register_package  Register a package at the front desk.  -> Package
  packages.update_package  Update a package [deprecated]
module units
  units.summary  Summarize a unit  -> UnitSummary [composite]`, c.renderSearch("", "package"))

	// Every word must match, each in any field; underscores are spaces.
	assert.Equal(t, c.renderSearch("", "front desk"), c.renderSearch("", "register package"))
	assert.Equal(t, c.renderSearch("", "register package"), c.renderSearch("", "register_package"))
	assert.Equal(t, c.renderSearch("", "register package"), c.renderSearch("", "desk register"))
	assert.Contains(t, c.renderSearch("", "register zzz"), "no functions match")

	assert.Equal(t, `no functions match "zzz"
modules: default, packages, units`, c.renderSearch("", " zzz "))
}

func TestRenderEmmaSearchFolded(t *testing.T) {
	c := emmaCatalog(t)

	// One match: the route and the arguments follow the search line, and the
	// summary is not repeated.
	out := c.renderSearch("", "unpair")
	assert.Equal(t, `module finance
  finance.unpair_bank_account_transfer  Unpair Bank Account Transfer  -> string uuid
    DELETE /complexes/{complex_id}/bank-accounts/transfers/{id}
    args:
      complex_id  integer int64  required
      id          string uuid    required`, out)
	assert.Equal(t, 1, strings.Count(out, "Unpair Bank Account Transfer"))

	// Five matches across two modules still fold.
	out = c.renderSearch("", "fingerprint")
	assert.Equal(t, 5, strings.Count(out, "\n    args:"))
	assert.Contains(t, out, "\n    GET /users/{user_id}/fingerprints\n")
	assert.Contains(t, out, "\n\nmodule users\n")

	// Six matches are dense lines only.
	out = c.renderSearch("", "invoices")
	assert.Equal(t, 8, len(strings.Split(out, "\n")))
	assert.NotContains(t, out, "args:")
	assert.Contains(t, out, "\n  finance.list_invoices  List Invoices  -> invoices.ListResponse\n")

	// A module listing of three functions folds too.
	assert.Equal(t, `module system
  system.client_version  Client Version
    GET /client/version

  system.head_health  Health Check
    HEAD /health

  system.health  Health Check
    GET /health`, c.renderSearch("system", ""))

	// The bare module list never folds.
	assert.Equal(t, len(c.Modules), len(strings.Split(c.renderSearch("", ""), "\n")))

	// A folded block leaves the return shape and related terms to
	// describe_function.
	for _, query := range []string{"unpair", "fingerprint"} {
		out = c.renderSearch("", query)
		assertClean(t, out)
		assert.NotContains(t, out, "see also")
		assert.NotContains(t, out, "\n    returns ")
	}
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
		"emma_search_folded":  c.renderSearch("", "fingerprint"),
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

	// Small modules fold, so the dense line of every function is measured
	// as a large listing prints it.
	total := 0
	lines := 0
	for _, m := range c.Modules {
		assertClean(t, c.renderSearch(m.Name, ""))
		for _, f := range m.Functions {
			total += len("  " + c.searchLine(f))
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
