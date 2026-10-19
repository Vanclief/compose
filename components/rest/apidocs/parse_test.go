package apidocs

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func loadEmma(t *testing.T, name string) *Document {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	doc, err := Parse(data)
	require.NoError(t, err)
	return doc
}

func TestParseEmmaYAMLAndJSONAgree(t *testing.T) {
	yamlDoc := loadEmma(t, "emma.yaml")
	jsonDoc := loadEmma(t, "emma.json")

	require.Equal(t, "3.0.3", yamlDoc.OpenAPI)
	require.Equal(t, "Emma API", yamlDoc.Info.Title)
	require.ElementsMatch(t, yamlDoc.Paths.Keys, jsonDoc.Paths.Keys)
	require.ElementsMatch(t, yamlDoc.Components.Schemas.Keys, jsonDoc.Components.Schemas.Keys)
	require.Len(t, yamlDoc.Paths.Keys, 180)
	require.Len(t, yamlDoc.Tags, 16)

	item, ok := yamlDoc.Paths.Get("/auth/login")
	require.True(t, ok)
	require.NotNil(t, item.Post)
	require.Equal(t, "auth.login", item.Post.OperationID)

	body := yamlDoc.RequestBody(item.Post.RequestBody)
	mt, ok := body.Content.Get("application/json")
	require.True(t, ok)
	schema, name := yamlDoc.Schema(mt.Schema)
	require.Equal(t, "application_services_authentication_login_LoginRequest", name)
	require.Equal(t, "object", schema.Type.Primary())
}
