package reference

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// modelTagsSpec declares the tags Pets and Stores and uses Admin without
// declaring it. Pet is used by three Pets operations and one Stores one;
// Address once by each; Money names Stores in x-tags although only Pets
// uses it, and Audit names Admin first in a list; Orphan is used by no one,
// and neither is stores.GetRequest, whose module's ListResponse is Stores'.
const modelTagsSpec = `openapi: 3.1.0
info: {title: Petstore, version: "1"}
tags:
  - name: Pets
  - name: Stores
paths:
  /pets:
    get:
      tags: [Pets]
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {type: array, items: {$ref: '#/components/schemas/Pet'}}
    post:
      tags: [Pets]
      requestBody:
        content:
          application/json:
            schema: {$ref: '#/components/schemas/PetCreate'}
      responses:
        "201": {description: created}
  /pets/{id}:
    get:
      tags: [Pets]
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Pet'}
  /stores:
    get:
      tags: [Stores]
      parameters:
        - name: page
          in: query
          schema: {$ref: '#/components/schemas/PageSize'}
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
                properties:
                  stores: {$ref: '#/components/schemas/app_handlers_stores_ListResponse'}
                  featured: {$ref: '#/components/schemas/Pet'}
    post:
      tags: [Stores]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                address: {$ref: '#/components/schemas/Address'}
      responses:
        "201": {description: created}
  /audit:
    get:
      tags: [Admin]
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Audit'}
components:
  schemas:
    Pet:
      type: object
      x-table: pets
      properties:
        id: {type: integer}
        status: {$ref: '#/components/schemas/PetStatus'}
    PetStatus: {type: string, enum: [available, sold]}
    PetCreate:
      type: object
      properties:
        name: {type: string}
        price: {$ref: '#/components/schemas/Money'}
        address: {$ref: '#/components/schemas/Address'}
    Store:
      type: object
      x-table: stores
      properties:
        id: {type: integer}
    PageSize: {type: integer}
    Money:
      type: object
      x-tags: Stores
      properties:
        cents: {type: integer}
    Audit:
      type: object
      x-tags: [Admin, Pets]
      properties:
        at: {type: string}
    Address:
      type: object
      properties:
        city: {type: string}
    Orphan: {type: string}
    app_handlers_stores_GetRequest:
      type: object
      properties:
        id: {type: integer}
    app_handlers_stores_ListResponse:
      type: object
      properties:
        stores: {type: array, items: {$ref: '#/components/schemas/Store'}}
`

// groupList prints the Models sidebar groups as Name[models] entries.
func groupList(groups []modelGroup) string {
	var parts []string
	for _, g := range groups {
		var names []string
		for _, m := range g.Models {
			names = append(names, m.Short)
		}
		parts = append(parts, g.Name+"["+strings.Join(names, " ")+"]")
	}
	return strings.Join(parts, " ")
}

func TestModelTags(t *testing.T) {
	doc, err := Parse([]byte(modelTagsSpec))
	require.NoError(t, err)
	require.Equal(t, map[string]string{
		"Pet":       "Pets",   // three Pets operations against one Stores one
		"PetStatus": "Pets",   // through Pet
		"PetCreate": "Pets",   // a request body
		"Store":     "Stores", // a response's items
		"PageSize":  "Stores", // a parameter
		"Money":     "Stores", // x-tags as a string wins over its Pets use
		"Audit":     "Admin",  // the first tag of an x-tags list

		"app_handlers_stores_ListResponse": "Stores",
		"app_handlers_stores_GetRequest":   "Stores", // unused, filed with its module
	}, doc.modelTags())
}

func TestModelTagGroups(t *testing.T) {
	doc, err := Parse([]byte(modelTagsSpec))
	require.NoError(t, err)
	p := buildPage(doc, Options{}, nil)
	// Declared tags first, then the undeclared Admin, then Shared for the tie
	// (Address) and the unused (Orphan); stored models lead their group.
	require.Equal(t, "Pets[Pet PetCreate PetStatus] Stores[Store GetRequest ListResponse Money PageSize] Admin[Audit] Shared[Address Orphan]", groupList(p.ModelNav.Groups))
	require.Empty(t, p.ModelNav.Loose)
	require.True(t, p.ModelNav.Groups[3].Shared)

	// The Models page follows the sidebar.
	var order []string
	for _, m := range p.Models {
		order = append(order, m.Short)
	}
	require.Equal(t, []string{"Pet", "PetCreate", "PetStatus", "Store", "GetRequest", "ListResponse", "Money", "PageSize", "Audit", "Address", "Orphan"}, order)
}

func TestModelGroupsWithoutTags(t *testing.T) {
	doc, err := Parse([]byte(`openapi: 3.1.0
info: {title: Flat, version: "1"}
paths:
  /cards:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {$ref: '#/components/schemas/app_models_cards_Card'}
components:
  schemas:
    Status: {type: string, enum: [on, off]}
    Settings:
      type: object
      properties:
        status: {$ref: '#/components/schemas/Status'}
    app_models_cards_Card:
      type: object
      properties:
        id: {type: integer}
`))
	require.NoError(t, err)
	p := buildPage(doc, Options{}, nil)
	// Without tags the models keep their module folders, the rest loose.
	require.Equal(t, "cards[Card]", groupList(p.ModelNav.Groups))
	var loose []string
	for _, m := range p.ModelNav.Loose {
		loose = append(loose, m.Short)
	}
	require.Equal(t, []string{"Settings", "Status"}, loose)
}

func TestModelTagGroupsDiff(t *testing.T) {
	head, err := Parse([]byte(modelTagsSpec))
	require.NoError(t, err)
	// The base has a Legacy model, removed since, used by a Legacy operation
	// that is gone too, and a PetStatus without "sold".
	base := strings.Replace(modelTagsSpec, "enum: [available, sold]", "enum: [available]", 1)
	base = strings.Replace(base, "  /audit:\n", `  /legacy:
    get:
      tags: [Legacy]
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Legacy'}
  /audit:
`, 1) + `    Legacy:
      type: object
      properties:
        id: {type: integer}
`
	diff, err := newDiffState(head, []byte(base))
	require.NoError(t, err)
	p := buildPage(head, Options{}, diff)
	// The removed model is filed by the base document, after the head's tags.
	require.Equal(t, "Pets[Pet PetCreate PetStatus] Stores[Store GetRequest ListResponse Money PageSize] Admin[Audit] Legacy[Legacy] Shared[Address Orphan]", groupList(p.ModelNav.Groups))
	require.Equal(t, []groupChange{{Class: "diff-removed", Letter: "R", Label: "removed", Count: 1}}, p.ModelNav.Groups[3].Changes)
	// PetStatus changed, and Pet with it.
	var changed []string
	for _, m := range p.ModelNav.Groups[0].Models {
		if m.Diff != nil {
			changed = append(changed, m.Short)
		}
	}
	require.Equal(t, []string{"Pet", "PetStatus"}, changed)
	require.Equal(t, []groupChange{{Class: "diff-changed", Letter: "C", Label: "changed", Count: 2}}, p.ModelNav.Groups[0].Changes)
}

func TestRenderModelTagGroups(t *testing.T) {
	doc, err := Parse([]byte(modelTagsSpec))
	require.NoError(t, err)
	out, err := Render(doc, Options{AssetsURL: "/assets"})
	require.NoError(t, err)
	html := string(out)
	requireBalanced(t, html)

	pane := section(t, html, `data-pane="models"`)
	require.Contains(t, pane, `<div class="nav-group">
<button type="button" class="nav-group-tg" aria-expanded="false"><svg class="ico-folder" viewBox="0 0 16 16" aria-hidden="true"><path d="M1.75 3.5h4.1l1.5 1.6h6.9v7.4H1.75z"/></svg><span class="nav-group-name">Pets</span><span class="count">3</span></button>
<ul class="nav" hidden>`)
	require.Contains(t, pane, `<div class="nav-group nav-shared">`)
	require.Contains(t, pane, `<span class="nav-group-name">Shared</span><span class="count">2</span>`)
	require.NotContains(t, pane, `nav-family`)
	// Groups and articles share one order.
	require.Less(t, strings.Index(pane, `href="#model/PetStatus"`), strings.Index(pane, `href="#model/Store"`))
	require.Less(t, strings.Index(html, `id="model/PetStatus"`), strings.Index(html, `id="model/Store"`))
	require.Less(t, strings.Index(html, `id="model/Audit"`), strings.Index(html, `id="model/Address"`))
}
