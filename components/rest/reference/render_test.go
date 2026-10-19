package reference

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const renderSpec = `
openapi: 3.0.3
info:
  title: Pets <Shop>
  version: "2.1"
  description: |
    Welcome to the **pet** API.

    - first point
    - second point

    <img src=x onerror=alert(1)>
servers:
  - url: https://pets.example.com
security:
  - Key: []
tags:
  - name: Pets
    description: Everything about pets.
paths:
  /pets/{pet_id}:
    parameters:
      - name: pet_id
        in: path
        required: true
        description: The pet.
        schema:
          type: string
          format: uuid
    get:
      operationId: pets.getPet
      summary: Get a pet
      tags: [Pets]
      parameters:
        - name: status
          in: query
          deprecated: true
          schema:
            type: string
            enum: [alive, sold]
      responses:
        "404":
          description: Not found
        "200":
          description: OK
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Pet'
        default:
          description: Error
  /pets:
    post:
      operationId: pets.createPet
      tags: [Pets]
      security: []
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/Pet'
      responses:
        "201":
          description: Created
  /tree:
    get:
      operationId: dup.op
      responses:
        "200":
          description: OK
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Node'
    post:
      operationId: dup.op
      responses:
        "204":
          description: Empty
components:
  securitySchemes:
    Key:
      type: apiKey
      in: header
      name: X-Key
  schemas:
    Pet:
      type: object
      required: [name, kind]
      properties:
        name:
          type: string
          minLength: 1
          maxLength: 40
        nickname:
          type: string
          nullable: true
        legacy_id:
          type: integer
          format: int64
          deprecated: true
        kind:
          type: string
          enum: [cat, dog]
        secret:
          type: string
          writeOnly: true
        animal:
          oneOf:
            - $ref: '#/components/schemas/Cat'
            - $ref: '#/components/schemas/Dog'
          discriminator:
            propertyName: kind
            mapping:
              meow: '#/components/schemas/Cat'
              woof: '#/components/schemas/Dog'
        contact:
          type: object
          anyOf:
            - required: [email]
            - required: [phone]
          properties:
            email:
              type: string
            phone:
              type: string
        labels:
          type: object
          additionalProperties:
            type: string
    Cat:
      type: object
      properties:
        lives:
          type: integer
    Dog:
      type: object
      properties:
        good:
          type: boolean
    Node:
      type: object
      properties:
        name:
          type: string
        children:
          type: array
          items:
            $ref: '#/components/schemas/Node'
`

func renderTestDoc(t *testing.T, opts Options) string {
	t.Helper()
	doc, err := Parse([]byte(renderSpec))
	require.NoError(t, err)
	html, err := Render(doc, opts)
	require.NoError(t, err)
	return string(html)
}

// requireBalanced checks that the common container tags open and close the
// same number of times, a cheap well-formedness guard for the hand-built
// schema tree HTML.
func requireBalanced(t *testing.T, html string) {
	t.Helper()
	for _, tag := range []string{"div", "pre", "code", "section", "article", "aside", "form", "label", "button", "ul", "li", "a", "span", "nav", "main"} {
		open := len(regexp.MustCompile(`<`+tag+`[\s>]`).FindAllStringIndex(html, -1))
		closed := strings.Count(html, "</"+tag+">")
		require.Equal(t, open, closed, "unbalanced <%s>", tag)
	}
}

func TestRenderEmmaInline(t *testing.T) {
	doc := loadEmma(t, "emma.yaml")

	start := time.Now()
	out, err := Render(doc, Options{})
	require.NoError(t, err)
	t.Logf("emma page: %d bytes in %s", len(out), time.Since(start))
	html := string(out)

	require.Contains(t, html, "<title>Emma API</title>")
	require.Contains(t, html, `id="operation/auth-login"`)
	require.Contains(t, html, `href="#operation/auth-login"`)
	require.Contains(t, html, `id="model/StandardError"`)
	require.Contains(t, html, `href="#model/StandardError"`)
	require.Contains(t, html, `id="tag/access-control"`)
	require.Contains(t, html, `id="operation/visitors-getaccesscodepublic"`)

	auth := html[strings.Index(html, `id="section/authentication"`):]
	auth = auth[:strings.Index(auth, "</section>")]
	require.Contains(t, auth, "<code>API-KEY</code>")
	require.Contains(t, auth, "<code>API-SECRET</code>")

	require.Contains(t, html, "<script>")
	require.Contains(t, html, "<style>")
	require.NotContains(t, html, "reference.js\"")

	// emma's anyOf of bare required lists is a note, not tabs.
	require.Contains(t, html, "At least one of <code>email</code>, <code>phone_number</code> is required.")
	// Unnamed oneOf variants are labelled by their pinned enum value.
	require.Contains(t, html, ">scope: cluster</button>")

	requireBalanced(t, html)
}

func TestRenderAllFixtures(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	require.NoError(t, err)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".json") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			doc := loadEmma(t, name)
			html, err := Render(doc, Options{AssetsURL: "/assets"})
			require.NoError(t, err)
			requireBalanced(t, string(html))
		})
	}
}

func TestRenderAssetsURL(t *testing.T) {
	html := renderTestDoc(t, Options{AssetsURL: "/docs/assets/", SpecURL: "/openapi.yaml", Title: "Docs"})

	require.Contains(t, html, `<script src="/docs/assets/reference.js?v=`+assetVersion("reference.js")+`"></script>`)
	require.Contains(t, html, `<link rel="stylesheet" href="/docs/assets/reference.css?v=`+assetVersion("reference.css")+`">`)
	require.NotEmpty(t, assetVersion("reference.js"))
	require.NotContains(t, html, "<script>")
	require.NotContains(t, html, "<style>")
	require.Contains(t, html, "<title>Docs</title>")
	require.Contains(t, html, `href="/openapi.yaml"`)
}

func TestRenderSmallDocument(t *testing.T) {
	html := renderTestDoc(t, Options{})
	requireBalanced(t, html)

	// Title is escaped; markdown renders lists and drops raw HTML.
	require.Contains(t, html, "<title>Pets &lt;Shop&gt;</title>")
	require.Contains(t, html, "<ul>\n<li>first point</li>")
	require.Contains(t, html, "<strong>pet</strong>")
	require.NotContains(t, html, "<img")
	require.NotContains(t, html, "onerror=alert")

	// Anchors: slugged operationIds, -2 on collision, tags and models.
	require.Contains(t, html, `id="operation/pets-getpet"`)
	require.Contains(t, html, `id="operation/dup-op"`)
	require.Contains(t, html, `id="operation/dup-op-2"`)
	require.Contains(t, html, `id="tag/pets"`)
	require.Contains(t, html, `id="tag/other"`)
	require.Contains(t, html, `id="model/Pet"`)

	getPet := section(t, html, `id="operation/pets-getpet"`)
	// Path-level parameter merged with the operation's query parameter.
	require.Contains(t, getPet, "Path parameters")
	require.Contains(t, getPet, `<span class="pn">pet_id</span><span class="ty">string &lt;uuid&gt;</span><span class="mk req">required</span>`)
	require.Contains(t, getPet, "Query parameters")
	require.Contains(t, getPet, `<span class="mk dep">deprecated</span>`)
	require.Contains(t, getPet, `<code class="chip">alive</code><code class="chip">sold</code>`)
	require.Contains(t, getPet, `<span class="pp">{pet_id}</span>`)
	// Responses are sorted with default last.
	require.Less(t, strings.Index(getPet, `>200</span>`), strings.Index(getPet, `>404</span>`))
	require.Less(t, strings.Index(getPet, `>404</span>`), strings.Index(getPet, `>default</span>`))
	require.Contains(t, getPet, `<a href="#auth/key">Key</a>`)

	createPet := section(t, html, `id="operation/pets-createpet"`)
	require.Contains(t, createPet, "No authentication")
	require.Contains(t, createPet, `data-requires="[]"`)
	// writeOnly properties appear in request trees only.
	require.Contains(t, createPet, ">secret</span>")
	require.NotContains(t, getPet, ">secret</span>")

	// Markers on the model card.
	pet := section(t, html, `id="model/Pet"`)
	require.Contains(t, pet, `<span class="mk nul">nullable</span>`)
	require.Contains(t, pet, `<span class="mk dep">deprecated</span>`)
	require.Contains(t, pet, `<code class="chip">cat</code><code class="chip">dog</code>`)
	require.Contains(t, pet, `<span class="cn">[1 .. 40] characters</span>`)
	require.Contains(t, pet, `<span class="pk">&lt;key&gt;</span>`)
	// oneOf renders a tab per variant, labelled by the discriminator mapping.
	require.Contains(t, pet, `role="tablist"><button type="button" role="tab" class="tab" aria-selected="true">meow</button><button type="button" role="tab" class="tab" aria-selected="false">woof</button>`)
	require.Contains(t, pet, `data-model="model/Cat" data-autofill`)
	require.Contains(t, pet, `data-model="model/Dog" data-autofill`)
	require.Contains(t, section(t, html, `id="model/Cat"`), ">lives</span>")
	// anyOf of bare required lists is a note.
	require.Contains(t, pet, "At least one of <code>email</code>, <code>phone</code> is required.")

	// A recursive schema terminates with a recursive row linking the model.
	node := section(t, html, `id="model/Node"`)
	require.Contains(t, node, `<a class="mk rec" href="#model/Node">recursive</a>`)
	tree := section(t, html, `id="operation/dup-op"`)
	require.Contains(t, tree, `<a class="mk rec" href="#model/Node">recursive</a>`)
}

// TestRenderAllOfWrapperRecursion guards against the schema tree blowing up
// exponentially when a model refers to itself through allOf wrappers (the
// 3.0 idiom for adding a description to a $ref): the wrapper must count as
// the model for the cycle guard and for lazy rows.
func TestRenderAllOfWrapperRecursion(t *testing.T) {
	spec := `
openapi: 3.0.3
info:
  title: Wrapped
  version: "1"
paths:
  /nodes:
    get:
      operationId: nodes.get
      responses:
        "200":
          description: OK
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/Node'
components:
  schemas:
    Node:
      type: object
      properties:
        left:
          description: Left child.
          allOf:
            - $ref: '#/components/schemas/Node'
        right:
          description: Right child.
          allOf:
            - $ref: '#/components/schemas/Node'
        parent:
          description: Parent node.
          allOf:
            - $ref: '#/components/schemas/Node'
`
	doc, err := Parse([]byte(spec))
	require.NoError(t, err)

	start := time.Now()
	out, err := Render(doc, Options{AssetsURL: "/assets"})
	require.NoError(t, err)
	require.Less(t, time.Since(start), 2*time.Second)
	require.Less(t, len(out), 50*1024)

	html := string(out)
	requireBalanced(t, html)
	node := section(t, html, `id="model/Node"`)
	require.Contains(t, node, `<a class="mk rec" href="#model/Node">recursive</a>`)
	require.Contains(t, node, "Left child.")
	op := section(t, html, `id="operation/nodes-get"`)
	require.Contains(t, op, `<a class="mk rec" href="#model/Node">recursive</a>`)
}

func TestRenderServersAndTryParams(t *testing.T) {
	spec := `
openapi: 3.0.3
info:
  title: Servers
  version: "1"
servers:
  - url: http://{host}:{port}/v1
    variables:
      host:
        default: localhost
      port:
        default: "8080"
        enum: ["8080", "9090"]
paths:
  /items/{ids}:
    get:
      operationId: items.list
      servers:
        - url: https://items.example.com
      parameters:
        - name: ids
          in: path
          required: true
          schema:
            type: array
            items:
              type: integer
          example: [1, 2]
        - name: tag
          in: query
          required: true
          schema:
            type: array
            items:
              type: string
          example: [a, b]
        - name: sort
          in: query
          explode: false
          schema:
            type: array
            items:
              type: string
          example: [name, age]
        - name: pipe
          in: query
          style: pipeDelimited
          explode: false
          schema:
            type: array
            items:
              type: string
      responses:
        "204":
          description: Empty
`
	doc, err := Parse([]byte(spec))
	require.NoError(t, err)
	out, err := Render(doc, Options{})
	require.NoError(t, err)
	html := string(out)

	require.Contains(t, html, `data-server="http://localhost:8080/v1"`)
	require.Contains(t, html, `<option value="http://localhost:8080/v1">`)
	require.Contains(t, html, `<li><code>http://localhost:8080/v1</code>`)
	require.Contains(t, html, `Template <code>http://{host}:{port}/v1</code>`)
	require.NotContains(t, html, `value="http://{host}`)

	op := section(t, html, `id="operation/items-list"`)
	require.Contains(t, op, `data-server="https://items.example.com"`)
	require.Contains(t, op, `data-name="ids" value="1,2" placeholder="1,2" data-required data-array title=`)
	require.Contains(t, op, `data-name="tag" value="a,b" placeholder="a,b" data-required data-array data-explode`)
	require.Contains(t, op, `data-name="sort" value="" placeholder="name,age" data-array title=`)
	require.Contains(t, op, `data-name="pipe" value="" placeholder="string" data-array data-sep="|"`)
	require.NotContains(t, op, `{port}`)

	out, err = Render(doc, Options{ServerURL: "https://staging.example.com"})
	require.NoError(t, err)
	op = section(t, string(out), `id="operation/items-list"`)
	require.NotContains(t, op, `data-server=`)
}

// section returns the element that starts at marker up to the next element of
// the same kind, enough to scope assertions to one operation or model.
func section(t *testing.T, html, marker string) string {
	t.Helper()
	i := strings.Index(html, marker)
	require.GreaterOrEqual(t, i, 0, "missing %s", marker)
	rest := html[i:]
	end := strings.Index(rest, "</article>")
	require.GreaterOrEqual(t, end, 0)
	return rest[:end]
}

func TestSlugify(t *testing.T) {
	require.Equal(t, "visitors-getaccesscodepublic", slugify("visitors.getAccessCodePublic"))
	require.Equal(t, "get-users-id", slugify("GET /users/{id}"))
	require.Equal(t, "item", slugify("..."))
}

func TestShortNames(t *testing.T) {
	names := shortNames([]string{
		"StandardError",
		"application_models_property_UnitRole",
		"application_modules_tickets_ListRequest",
		"application_modules_units_ListRequest",
	})
	require.Equal(t, "StandardError", names["StandardError"])
	require.Equal(t, "UnitRole", names["application_models_property_UnitRole"])
	require.Equal(t, "tickets.ListRequest", names["application_modules_tickets_ListRequest"])
	require.Equal(t, "units.ListRequest", names["application_modules_units_ListRequest"])
}

// TestWriteEmmaPage writes the rendered emma page for a human to open in a
// browser: APIDOCS_WRITE=/tmp/reference-emma.html go test -run TestWriteEmmaPage
func TestWriteEmmaPage(t *testing.T) {
	path := os.Getenv("APIDOCS_WRITE")
	if path == "" {
		t.Skip("set APIDOCS_WRITE=/tmp/reference-emma.html to write the rendered emma page")
	}
	doc := loadEmma(t, "emma.yaml")
	html, err := Render(doc, Options{SpecURL: "/openapi.yaml"})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, html, 0o644))
	t.Logf("wrote %d bytes to %s", len(html), path)
}

func BenchmarkRenderEmma(b *testing.B) {
	data, err := os.ReadFile("testdata/emma.yaml")
	require.NoError(b, err)
	doc, err := Parse(data)
	require.NoError(b, err)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err = Render(doc, Options{})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// navShape prints a sidebar tree as indented lines: folders as "name/ (n)"
// and operations as their titles.
func navShape(f *navFolder, indent string) []string {
	var out []string
	for _, op := range f.Ops {
		out = append(out, indent+op.Title)
	}
	for _, sub := range f.Folders {
		out = append(out, indent+sub.Name+"/ ("+strconv.Itoa(sub.Count)+")")
		out = append(out, navShape(sub, indent+"  ")...)
	}
	return out
}

func navOps(paths ...string) []*opView {
	var out []*opView
	for _, p := range paths {
		parts := strings.SplitN(p, " ", 2)
		out = append(out, &opView{Title: parts[0], Path: parts[1]})
	}
	return out
}

func TestNavTree(t *testing.T) {
	ops := navOps(
		"ListPoints /complexes/{complex_id}/access-control/access-points",
		"CreatePoint /complexes/{complex_id}/access-control/access-points",
		"GetPoint /complexes/{cid}/access-control/access-points/{id}",
		"ListCards /complexes/{complex_id}/access-control/cards",
		"GetCard /complexes/{complex_id}/access-control/cards/{id}",
		"RevokeCard /complexes/{complex_id}/access-control/cards/{id}/revoke",
		"BulkCreate /complexes/{complex_id}/access-control/cards/bulk",
		"BulkDelete /complexes/{complex_id}/access-control/cards/bulk",
		"Ping /complexes/{complex_id}/access-control/ping",
		"ListDevices /complexes/{complex_id}/access-control/devices",
		"GetDevice /complexes/{complex_id}/access-control/devices/{id}",
	)
	tree := navTree(ops)
	require.Equal(t, 11, tree.Count)
	require.Equal(t, []string{
		// A segment with a single operation is not a folder.
		"Ping",
		"access-points/ (3)",
		"  ListPoints",
		"  CreatePoint",
		"  GetPoint",
		// {id} adds no level; revoke has one operation so it stays a leaf;
		// bulk has two and becomes a sub-folder after the operations.
		"cards/ (5)",
		"  ListCards",
		"  GetCard",
		"  RevokeCard",
		"  bulk/ (2)",
		"    BulkCreate",
		"    BulkDelete",
		"devices/ (2)",
		"  ListDevices",
		"  GetDevice",
	}, navShape(tree, ""))

	// One resource level stays flat, in document order.
	flat := navTree(navOps(
		"List /visitors",
		"Create /visitors",
		"Get /visitors/{id}",
		"Update /visitors/{id}",
		"Delete /visitors/{id}",
		"Codes /visitors/{id}/codes",
		"Report /visitors/report",
		"Export /visitors/export",
		"Search /visitors/search",
	))
	require.Empty(t, flat.Folders)
	require.Equal(t, []string{"List", "Create", "Get", "Update", "Delete", "Codes", "Report", "Export", "Search"}, navShape(flat, ""))

	// Small tags are always flat.
	small := navTree(navOps(
		"Login /auth/login",
		"Challenge /auth/login/challenge",
		"Update /auth/login/update",
		"Legacy /login",
		"LegacyChallenge /login/challenge",
	))
	require.Empty(t, small.Folders)
	require.Equal(t, 5, small.Count)

	// One operation outside /complexes/{complex_id} leaves complexes as a
	// top-level folder that only scopes by id: it is dissolved. A folder
	// holding nothing but one sub-folder is compacted into a/b.
	scoped := navTree(navOps(
		"Public /public/code",
		"ListCodes /complexes/{complex_id}/codes",
		"GetCode /complexes/{complex_id}/codes/{id}",
		"ListAccesses /complexes/{complex_id}/accesses",
		"GetAccess /complexes/{complex_id}/accesses/{id}",
		"ListBills /complexes/{complex_id}/finance/utilities/bills",
		"GetBill /complexes/{complex_id}/finance/utilities/bills/{id}",
		"ListShares /complexes/{complex_id}/finance/utilities/shares",
		"GetShare /complexes/{complex_id}/finance/utilities/shares/{id}",
	))
	require.Equal(t, []string{
		"Public",
		"accesses/ (2)",
		"  ListAccesses",
		"  GetAccess",
		"codes/ (2)",
		"  ListCodes",
		"  GetCode",
		"finance/utilities/ (4)",
		"  bills/ (2)",
		"    ListBills",
		"    GetBill",
		"  shares/ (2)",
		"    ListShares",
		"    GetShare",
	}, navShape(scoped, ""))

	// A lone folder left at the top (the parameter sits at different depths,
	// so the prefix stops early) is unwrapped.
	paths := []string{}
	for i := 0; i < 5; i++ {
		paths = append(paths, "A"+strconv.Itoa(i)+" /x/{id}/c", "B"+strconv.Itoa(i)+" /x/c/{other}")
	}
	lone := navTree(navOps(paths...))
	require.Empty(t, lone.Folders)
	require.Equal(t, 10, len(lone.Ops))
}
