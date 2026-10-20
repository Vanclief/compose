package reference

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// familiesSpec has two entities, Pet and PetOwner, and models that join
// them by prefix (PetStatus, the longer PetOwnerRole), by their one user
// (Color), by the one head they refer to (CreatePetResponse) or not at all
// (Money, used by both; Petstore, a prefix but not at a word boundary).
const familiesSpec = `openapi: 3.1.0
info: {title: Pets, version: "1"}
paths: {}
components:
  schemas:
    Pet:
      type: object
      properties:
        id: {type: integer}
        status: {$ref: '#/components/schemas/PetStatus'}
        color: {$ref: '#/components/schemas/Color'}
        price: {$ref: '#/components/schemas/Money'}
    PetStatus: {type: string, enum: [available, sold]}
    Color: {type: string, enum: [black, white]}
    PetOwner:
      type: object
      properties:
        id: {type: integer}
        role: {$ref: '#/components/schemas/PetOwnerRole'}
        budget: {$ref: '#/components/schemas/Money'}
    PetOwnerRole: {type: string, enum: [owner, keeper]}
    Money: {type: string, format: decimal}
    CreatePetResponse:
      type: object
      properties:
        pet: {$ref: '#/components/schemas/Pet'}
    Petstore:
      type: object
      properties:
        name: {type: string}
`

// familyList prints a sidebar list as Head[members] entries.
func familyList(list []modelFamily) string {
	var parts []string
	for _, f := range list {
		name := "Shared"
		if f.Head != nil {
			name = f.Head.Short
		}
		var members []string
		for _, m := range f.Members {
			members = append(members, m.Short)
		}
		if len(members) > 0 {
			name += "[" + strings.Join(members, " ") + "]"
		}
		parts = append(parts, name)
	}
	return strings.Join(parts, " ")
}

func TestModelFamilies(t *testing.T) {
	doc, err := Parse([]byte(familiesSpec))
	require.NoError(t, err)
	p := buildPage(doc, Options{}, nil)
	require.Equal(t, "Pet[Color CreatePetResponse PetStatus] PetOwner[PetOwnerRole] Shared[Money Petstore]", familyList(p.ModelNav.Loose))

	// The Models page follows the sidebar.
	var order []string
	for _, m := range p.Models {
		order = append(order, m.Short)
	}
	require.Equal(t, []string{"Pet", "Color", "CreatePetResponse", "PetStatus", "PetOwner", "PetOwnerRole", "Money", "Petstore"}, order)
}

func TestModelFamiliesWithoutHeads(t *testing.T) {
	doc, err := Parse([]byte(`openapi: 3.1.0
info: {title: Flat, version: "1"}
paths: {}
components:
  schemas:
    Status: {type: string, enum: [on, off]}
    Settings:
      type: object
      properties:
        status: {$ref: '#/components/schemas/Status'}
    SettingsResponse:
      type: object
      properties:
        settings: {$ref: '#/components/schemas/Settings'}
`))
	require.NoError(t, err)
	p := buildPage(doc, Options{}, nil)
	require.Equal(t, "Settings SettingsResponse Status", familyList(p.ModelNav.Loose))
}

func TestIsFamilyPrefix(t *testing.T) {
	require.True(t, isFamilyPrefix("Benchmark", "BenchmarkStatus"))
	require.True(t, isFamilyPrefix("Run", "Run2"))
	require.True(t, isFamilyPrefix("card", "card_format"))
	require.False(t, isFamilyPrefix("Bench", "Benchmark"))
	require.False(t, isFamilyPrefix("Pet", "Pet"))
	require.False(t, isFamilyPrefix("", "Pet"))
}

func TestRenderModelFamilies(t *testing.T) {
	doc, err := Parse([]byte(familiesSpec))
	require.NoError(t, err)
	out, err := Render(doc, Options{AssetsURL: "/assets"})
	require.NoError(t, err)
	html := string(out)
	requireBalanced(t, html)

	pane := section(t, html, `data-pane="models"`)
	// A family's row is its head's link with a chevron and a member count.
	require.Contains(t, pane, `<li class="nav-folder nav-family"><div class="nav-family-head"><button type="button" class="nav-folder-tg nav-family-tg" aria-expanded="false" title="Show the 3 models in this family"><span class="nav-folder-name">Pet</span></button><a class="nav-model" href="#model/Pet"`)
	require.Contains(t, pane, `<span class="nav-title">Pet</span><span class="count">3</span></a></div>`)
	require.Contains(t, pane, `<span class="nav-folder-name">Shared</span><span class="count">2</span></button>`)
	// Members hang under their head, collapsed.
	family := pane[strings.Index(pane, `href="#model/PetOwner"`):]
	require.Less(t, strings.Index(family, `<ul class="nav" hidden>`), strings.Index(family, `href="#model/PetOwnerRole"`))
	// Articles follow the families.
	require.Less(t, strings.Index(html, `id="model/CreatePetResponse"`), strings.Index(html, `id="model/PetOwner"`))
}

func TestModelFamiliesDiff(t *testing.T) {
	head, err := Parse([]byte(familiesSpec))
	require.NoError(t, err)
	// The base has a Legacy entity with its LegacyKind, both removed since,
	// and a PetStatus without "sold".
	base := strings.Replace(familiesSpec, "enum: [available, sold]", "enum: [available]", 1) + `    Legacy:
      type: object
      properties:
        id: {type: integer}
        kind: {$ref: '#/components/schemas/LegacyKind'}
    LegacyKind: {type: string, enum: [old]}
`
	diff, err := newDiffState(head, []byte(base))
	require.NoError(t, err)
	p := buildPage(head, Options{}, diff)
	require.Equal(t, "Legacy[LegacyKind] Pet[Color CreatePetResponse PetStatus] PetOwner[PetOwnerRole] Shared[Money Petstore]", familyList(p.ModelNav.Loose))

	// A family sums up its members' changes for when it is collapsed:
	// PetStatus changed, and CreatePetResponse with it through Pet.
	require.Equal(t, []familyChange{{Class: "diff-removed", Letter: "R", Label: "removed", Count: 1}}, p.ModelNav.Loose[0].Changes)
	require.Equal(t, []familyChange{{Class: "diff-changed", Letter: "C", Label: "changed", Count: 2}}, p.ModelNav.Loose[1].Changes)
}
