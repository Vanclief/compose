package reference

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func graphNode(g *Graph, name string) *Node {
	for _, n := range g.Nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func graphEdge(g *Graph, from, to, field string) *Edge {
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.FromField == field {
			return e
		}
	}
	return nil
}

func graphField(n *Node, name string) *Field {
	for i := range n.Fields {
		if n.Fields[i].Name == name {
			return &n.Fields[i]
		}
	}
	return nil
}

func parseGraph(t *testing.T, spec string) *Graph {
	t.Helper()
	doc, err := Parse([]byte(spec))
	require.NoError(t, err)
	return doc.Graph()
}

func TestSchemaExtensions(t *testing.T) {
	schema := `{"type": "object", "x-ref": "#/components/schemas/Org", "x-flags": {"unique": true}, "properties": {"a": {"type": "string", "x-unit": "kg"}}}`
	for format, s := range parseSchemaBothWays(t, "3.1.0", schema) {
		require.Equal(t, "#/components/schemas/Org", s.Extensions["x-ref"], format)
		flags, ok := s.Extensions["x-flags"].(map[string]any)
		require.True(t, ok, format)
		require.Equal(t, true, flags["unique"], format)
		require.Len(t, s.Extensions, 2, format)

		a, ok := s.Properties.Get("a")
		require.True(t, ok, format)
		require.Equal(t, map[string]any{"x-unit": "kg"}, a.Extensions, format)

		// Extensions stay out of the JSON form of a schema.
		out, err := json.Marshal(s)
		require.NoError(t, err, format)
		require.NotContains(t, string(out), "x-ref", format)
		require.NotContains(t, string(out), "Extensions", format)
	}

	for format, s := range parseSchemaBothWays(t, "3.1.0", `{"type": "string"}`) {
		require.Nil(t, s.Extensions, format)
	}
}

func TestGraphEmma(t *testing.T) {
	const (
		ticket   = "application_models_coordination_Ticket"
		category = "application_models_coordination_TicketCategory"
		cluster  = "application_models_property_Cluster"
		complex  = "application_models_property_Complex"
		org      = "application_models_organization_Organization"
		unit     = "application_models_property_Unit"
		user     = "application_models_user_User"
	)

	for _, file := range []string{"emma.yaml", "emma.json"} {
		g := loadEmma(t, file).Graph()

		for _, name := range []string{ticket, category, cluster, complex, org, unit, user} {
			require.NotNil(t, graphNode(g, name), "%s: %s", file, name)
		}
		node := graphNode(g, ticket)
		require.Equal(t, "Ticket", node.Short)
		require.Equal(t, "coordination", node.Group)
		require.Equal(t, "model/"+ticket, node.Anchor)
		require.Equal(t, "id", node.Fields[0].Name)
		require.Equal(t, "property", graphNode(g, cluster).Group)

		e := graphEdge(g, ticket, cluster, "cluster_id")
		require.NotNil(t, e, file)
		require.Equal(t, EdgeFK, e.Kind)
		require.False(t, e.Inferred)
		require.Equal(t, "many : 0..1", e.Cardinality)
		require.Nil(t, graphEdge(g, ticket, cluster, "cluster"), "the cluster object and cluster_id are one relation")

		e = graphEdge(g, ticket, org, "organization_id")
		require.NotNil(t, e, file)
		require.Equal(t, EdgeFK, e.Kind)
		require.Equal(t, "many : 1", e.Cardinality)

		e = graphEdge(g, ticket, category, "categories")
		require.NotNil(t, e, file)
		require.Equal(t, EdgeHasMany, e.Kind)
		require.Equal(t, "1 : many", e.Cardinality)

		e = graphEdge(g, ticket, complex, "complex_id")
		require.NotNil(t, e, file)
		require.True(t, e.Inferred)
		require.Equal(t, complex, graphField(node, "complex_id").Ref)

		require.NotNil(t, graphEdge(g, ticket, unit, "unit_id"), file)
		require.NotNil(t, graphEdge(g, ticket, user, "creator_id"), file)
		require.NotNil(t, graphEdge(g, "application_models_accesscontrol_PersonAccess", user, "user_id"), file)

		priority := graphField(node, "priority")
		require.Equal(t, "TicketPriority", priority.Type)
		require.NotEmpty(t, priority.Enum)
		require.Empty(t, priority.Ref)
		cl := graphField(node, "cluster")
		require.True(t, cl.Nullable)
		require.True(t, cl.Optional)
		require.Equal(t, cluster, cl.Ref)
		require.Equal(t, "TicketCategory[]", graphField(node, "categories").Type)
		require.Equal(t, "uuid", graphField(node, "id").Type)

		names := map[string]bool{}
		for _, n := range g.Nodes {
			names[n.Name] = true
			require.False(t, isWrapperName(n.Name), n.Name)
			require.NotEqual(t, "github_com_uptrace_bun_schema_BaseModel", n.Name)
		}
		require.False(t, names["application_services_authentication_login_LoginRequest"])
		require.False(t, names["application_models_coordination_TicketPriority"], "enums are not entities")

		seen := map[string]bool{}
		for _, e := range g.Edges {
			require.True(t, names[e.From] && names[e.To], "%+v", e)
			key := e.From + " " + e.To + " " + e.FromField + " " + e.Kind
			require.False(t, seen[key], "duplicate edge %s", key)
			seen[key] = true
		}
	}
}

func TestGraphPetstoreFixture(t *testing.T) {
	g := loadEmma(t, "petstore-3.1.yaml").Graph()
	require.NotNil(t, graphNode(g, "Pet"))
	require.Equal(t, "", graphNode(g, "Pet").Group)
	require.Nil(t, graphNode(g, "Dog"), "a oneOf variant without id is not an entity")
}

func TestGraphPetstore(t *testing.T) {
	g := parseGraph(t, `
openapi: 3.0.3
info: {title: Petstore, version: "1"}
paths:
  /pets:
    post:
      requestBody:
        content:
          application/json:
            schema: {$ref: '#/components/schemas/NewPetRequest'}
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: {$ref: '#/components/schemas/PetList'}
components:
  schemas:
    Pet:
      type: object
      required: [id, name, category]
      properties:
        id: {type: integer, format: int64}
        name: {type: string}
        status: {type: string, enum: [available, pending, sold, archived]}
        category: {$ref: '#/components/schemas/Category'}
        tags:
          type: array
          items: {$ref: '#/components/schemas/Tag'}
    Category:
      type: object
      properties:
        id: {type: integer}
        name: {type: string}
    Tag:
      type: object
      properties:
        name: {type: string}
    NewPetRequest:
      type: object
      properties:
        id: {type: integer}
        pet: {$ref: '#/components/schemas/Pet'}
    PetList:
      type: object
      properties:
        items:
          type: array
          items: {$ref: '#/components/schemas/Pet'}
        total: {type: integer}
`)

	var names []string
	for _, n := range g.Nodes {
		names = append(names, n.Name)
	}
	require.Equal(t, []string{"Pet", "Category", "Tag"}, names)

	e := graphEdge(g, "Pet", "Category", "category")
	require.NotNil(t, e)
	require.Equal(t, EdgeHasOne, e.Kind)
	require.Equal(t, "1 : 1", e.Cardinality)
	e = graphEdge(g, "Pet", "Tag", "tags")
	require.NotNil(t, e)
	require.Equal(t, EdgeHasMany, e.Kind)
	require.Len(t, g.Edges, 2)

	status := graphField(graphNode(g, "Pet"), "status")
	require.Equal(t, []string{"available", "pending", "sold", "archived"}, status.Enum)
	require.True(t, status.Optional)
	require.False(t, graphField(graphNode(g, "Pet"), "name").Optional)
	require.True(t, graphField(graphNode(g, "Pet"), "tags").Optional)
}

func TestGraphExtensionRefsAndInference(t *testing.T) {
	g := parseGraph(t, `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "components": {"schemas": {
    "Org": {"type": "object", "properties": {"id": {"type": "string"}}},
    "TicketCategory": {"type": "object", "properties": {"id": {"type": "string"}}},
    "Account": {"type": "object", "properties": {"id": {"type": "string"}}},
    "Invoice": {
      "type": "object",
      "required": ["id", "owner_ref"],
      "properties": {
        "id": {"type": "string"},
        "owner_ref": {"type": "string", "x-ref": "#/components/schemas/Org"},
        "account_id": {"type": "string", "x-ref": "#/components/schemas/Account"},
        "account": {"$ref": "#/components/schemas/Account"},
        "categories_id": {"type": "string"},
        "parent_account_id": {"type": ["string", "null"]},
        "warehouse_id": {"type": "string"}
      }
    }
  }}
}`)

	e := graphEdge(g, "Invoice", "Org", "owner_ref")
	require.NotNil(t, e)
	require.Equal(t, EdgeFK, e.Kind)
	require.False(t, e.Inferred)
	require.Equal(t, "many : 1", e.Cardinality)
	require.Equal(t, "Org", graphField(graphNode(g, "Invoice"), "owner_ref").Ref)

	// The x-ref key and the account object are one relation.
	e = graphEdge(g, "Invoice", "Account", "account_id")
	require.NotNil(t, e)
	require.False(t, e.Inferred)
	require.Nil(t, graphEdge(g, "Invoice", "Account", "account"))

	e = graphEdge(g, "Invoice", "TicketCategory", "categories_id")
	require.Nil(t, e, "categories alone is Category, not TicketCategory")

	e = graphEdge(g, "Invoice", "Account", "parent_account_id")
	require.NotNil(t, e)
	require.True(t, e.Inferred)
	require.Equal(t, "many : 0..1", e.Cardinality)

	warehouse := graphField(graphNode(g, "Invoice"), "warehouse_id")
	require.Empty(t, warehouse.Ref)
	for _, e := range g.Edges {
		require.NotEqual(t, "warehouse_id", e.FromField)
	}
	require.Len(t, g.Edges, 3)
}

func TestGraphInferenceNames(t *testing.T) {
	require.Equal(t, "category", singularize("categories"))
	require.Equal(t, "address", singularize("addresses"))
	require.Equal(t, "status", singularize("status"))
	require.Equal(t, "unit", singularize("units"))
	require.Equal(t, "box", singularize("boxes"))
	require.Equal(t, "organization", foreignKeyBase("organization_id"))
	require.Equal(t, "organization", foreignKeyBase("organizationId"))
	require.Equal(t, "", foreignKeyBase("id"))
	require.Equal(t, "", foreignKeyBase("paid"))
	require.Equal(t, "TicketCategory", camelCase(splitWords("ticket_category")))
	require.Equal(t, "TicketCategory", camelCase(splitWords("ticketCategory")))
	require.Equal(t, "coordination", schemaGroup("application_models_coordination_Ticket"))
	require.Equal(t, "", schemaGroup("Pet"))
	require.Equal(t, "", schemaGroup("user_profile_settings"))

	index := newEntityIndex([]*Node{{Name: "a_models_crm_Category", Short: "crm.Category", Group: "crm"}, {Name: "a_models_finance_Category", Short: "finance.Category", Group: "finance"}})
	require.Equal(t, "a_models_finance_Category", index.match("categories", "finance"))
	require.Equal(t, "a_models_crm_Category", index.match("category", "other"))
	require.Equal(t, "a_models_crm_Category", index.match("parent_category", "crm"))
	require.Equal(t, "a_models_crm_Category", index.match("crm_category", ""))
}

func TestGraphHubs(t *testing.T) {
	// Org is pointed at by nine entities, Team by eight and by itself.
	schemas := `"Org": {"type": "object", "properties": {"id": {"type": "string"}}},
    "Team": {"type": "object", "properties": {"id": {"type": "string"}, "parent_team_id": {"type": "string"}}}`
	for i := 0; i < 9; i++ {
		team := ""
		if i < 8 {
			team = `, "team_id": {"type": "string"}`
		}
		schemas += fmt.Sprintf(`,
    "Thing%d": {"type": "object", "properties": {"id": {"type": "string"}, "org_id": {"type": "string"}%s}}`, i, team)
	}
	g := parseGraph(t, `{"openapi": "3.1.0", "info": {"title": "t", "version": "1"}, "components": {"schemas": {`+schemas+`}}}`)

	require.True(t, graphNode(g, "Org").Hub)
	require.False(t, graphNode(g, "Team").Hub, "eight sources and a self reference are not a hub")
	require.NotNil(t, graphEdge(g, "Team", "Team", "parent_team_id"))
	require.False(t, graphNode(g, "Thing0").Hub)
	require.NotNil(t, graphEdge(g, "Thing0", "Org", "org_id"), "hub edges stay in the graph")
}

func TestGraphHubsEmma(t *testing.T) {
	g := loadEmma(t, "emma.yaml").Graph()
	var hubs []string
	for _, n := range g.Nodes {
		if n.Hub {
			hubs = append(hubs, n.Short)
		}
	}
	require.ElementsMatch(t, []string{"Complex", "User", "Unit", "Cluster", "Organization", "Space"}, hubs)
}

// tablesSpec declares where two of its models are stored: the graph takes
// exactly those as entities, wrapper names included, and leaves out the id
// carrying shapes that declare no table.
const tablesSpec = `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "paths": {},
  "components": {"schemas": {
    "Org": {"type": "object", "x-table": "orgs", "properties": {"id": {"type": "string"}}},
    "AuditBody": {
      "type": "object",
      "x-table": " audit_bodies ",
      "properties": {"id": {"type": "string"}, "org_id": {"type": "string"}, "address": {"$ref": "#/components/schemas/Address"}}
    },
    "Address": {"type": "object", "properties": {"street": {"type": "string"}}},
    "OrgSummary": {"type": "object", "properties": {"id": {"type": "string"}, "org": {"$ref": "#/components/schemas/Org"}}},
    "Blank": {"type": "object", "x-table": "", "properties": {"id": {"type": "string"}}}
  }}
}`

func TestGraphDeclaredTables(t *testing.T) {
	doc, err := Parse([]byte(tablesSpec))
	require.NoError(t, err)
	require.Equal(t, "orgs", doc.Table("Org"))
	require.Equal(t, "audit_bodies", doc.Table("AuditBody"))
	require.Equal(t, "", doc.Table("Address"))
	require.Equal(t, "", doc.Table("Blank"))
	require.Equal(t, "", doc.Table("Missing"))

	g := doc.Graph()
	require.Len(t, g.Nodes, 2)
	require.Equal(t, "orgs", graphNode(g, "Org").Table)
	require.Equal(t, "audit_bodies", graphNode(g, "AuditBody").Table)
	require.Nil(t, graphNode(g, "Address"), "embedded, not stored")
	require.Nil(t, graphNode(g, "OrgSummary"), "has an id but no table")
	require.Nil(t, graphNode(g, "Blank"))

	e := graphEdge(g, "AuditBody", "Org", "org_id")
	require.NotNil(t, e)
	require.True(t, e.Inferred)
	require.Len(t, g.Edges, 1)

	// Without any x-table the entities are guessed as before.
	g = parseGraph(t, `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "components": {"schemas": {
    "Org": {"type": "object", "properties": {"id": {"type": "string"}}},
    "OrgResponse": {"type": "object", "properties": {"id": {"type": "string"}}}
  }}
}`)
	require.Len(t, g.Nodes, 1)
	require.Empty(t, g.Nodes[0].Table)
}

func TestGraphDropsInverseHasMany(t *testing.T) {
	g := parseGraph(t, `{
  "openapi": "3.1.0",
  "info": {"title": "t", "version": "1"},
  "components": {"schemas": {
    "Route": {"type": "object", "properties": {
      "id": {"type": "string"},
      "checkpoints": {"type": "array", "items": {"$ref": "#/components/schemas/Checkpoint"}},
      "tags": {"type": "array", "items": {"$ref": "#/components/schemas/Tag"}}
    }},
    "Checkpoint": {"type": "object", "properties": {
      "id": {"type": "string"},
      "route_id": {"type": "string"},
      "route": {"$ref": "#/components/schemas/Route"}
    }},
    "Tag": {"type": "object", "properties": {"id": {"type": "string"}}}
  }}
}`)

	require.NotNil(t, graphEdge(g, "Checkpoint", "Route", "route_id"))
	require.Nil(t, graphEdge(g, "Route", "Checkpoint", "checkpoints"), "the other side of Checkpoint.route_id")
	require.Equal(t, "Checkpoint", graphField(graphNode(g, "Route"), "checkpoints").Ref, "the field still links its model")
	require.NotNil(t, graphEdge(g, "Route", "Tag", "tags"), "no key back, so the has-many stays")
	require.Len(t, g.Edges, 2)
}
