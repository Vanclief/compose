package openapi

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vanclief/compose/components/rest/api/openapi/internal/fixture"
	"github.com/vanclief/compose/components/rest/api/openapi/internal/fixture/other"
)

var update = flag.Bool("update", false, "rewrite golden files")

var (
	requestTypes = []reflect.Type{
		reflect.TypeFor[fixture.ListRequest](),
		reflect.TypeFor[fixture.UpdateRequest](),
		reflect.TypeFor[*fixture.CreateRequest](),
		reflect.TypeFor[fixture.Node](),
	}
	responseTypes = []reflect.Type{
		reflect.TypeFor[fixture.Run](),
		reflect.TypeFor[*fixture.ListResponse](),
		reflect.TypeFor[fixture.Tree](),
		reflect.TypeFor[fixture.Page[fixture.Run]](),
		reflect.TypeFor[fixture.Page[other.Run]](),
		reflect.TypeFor[[]fixture.Run](),
		reflect.TypeFor[fixture.Flags](),
	}
)

// buildFixture registers the whole fixture and finalizes it.
func buildFixture(t *testing.T) (*Result, map[reflect.Type][]Param) {
	t.Helper()
	r := NewReflector()
	params := map[reflect.Type][]Param{}
	for _, typ := range requestTypes {
		p, err := r.AddRequest(typ)
		require.NoError(t, err, typ.String())
		params[typ] = p
	}
	for _, typ := range responseTypes {
		err := r.AddResponse(typ)
		require.NoError(t, err, typ.String())
	}
	res, err := r.Finalize()
	require.NoError(t, err)
	return res, params
}

func TestComponentsGolden(t *testing.T) {
	res, _ := buildFixture(t)
	got, err := json.MarshalIndent(res.Components, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')

	path := filepath.Join("testdata", "components.golden.json")
	if *update {
		err = os.WriteFile(path, got, 0o644)
		require.NoError(t, err)
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got), "run go test -update to accept the change")
}
