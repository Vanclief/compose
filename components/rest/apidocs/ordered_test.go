package apidocs

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
	"gopkg.in/yaml.v3"
)

func TestOrderedYAMLKeepsKeyOrder(t *testing.T) {
	var o Ordered[int]
	err := yaml.Unmarshal([]byte("zeta: 1\nalpha: 2\nmid: 3\n"), &o)
	require.NoError(t, err)

	require.Equal(t, []string{"zeta", "alpha", "mid"}, o.Keys)
	require.Equal(t, 3, o.Len())
	v, ok := o.Get("alpha")
	require.True(t, ok)
	require.Equal(t, 2, v)
}

func TestOrderedJSONKeepsKeyOrder(t *testing.T) {
	var o Ordered[int]
	err := json.Unmarshal([]byte(`{"zeta": 1, "alpha": 2, "mid": 3}`), &o)
	require.NoError(t, err)

	require.Equal(t, []string{"zeta", "alpha", "mid"}, o.Keys)
	v, ok := o.Get("mid")
	require.True(t, ok)
	require.Equal(t, 3, v)
}

func TestOrderedNestedMaps(t *testing.T) {
	yamlInput := "b:\n  y: 1\n  x: 2\na:\n  q: 3\n"
	jsonInput := `{"b": {"y": 1, "x": 2}, "a": {"q": 3}}`

	for name, decode := range map[string]func(*Ordered[Ordered[int]]) error{
		"yaml": func(o *Ordered[Ordered[int]]) error { return yaml.Unmarshal([]byte(yamlInput), o) },
		"json": func(o *Ordered[Ordered[int]]) error { return json.Unmarshal([]byte(jsonInput), o) },
	} {
		t.Run(name, func(t *testing.T) {
			var o Ordered[Ordered[int]]
			err := decode(&o)
			require.NoError(t, err)

			require.Equal(t, []string{"b", "a"}, o.Keys)
			inner, ok := o.Get("b")
			require.True(t, ok)
			require.Equal(t, []string{"y", "x"}, inner.Keys)
			require.Equal(t, 2, inner.Map["x"])
		})
	}
}

func TestOrderedRejectsNonMapping(t *testing.T) {
	var o Ordered[int]

	err := yaml.Unmarshal([]byte("- 1\n- 2\n"), &o)
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), "expected a mapping")

	err = yaml.Unmarshal([]byte("just a string"), &o)
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	err = json.Unmarshal([]byte(`[1, 2]`), &o)
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	require.Contains(t, err.Error(), "expected a JSON object")

	err = json.Unmarshal([]byte(`"text"`), &o)
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

func TestOrderedNullIsEmpty(t *testing.T) {
	var fromJSON struct {
		M Ordered[int] `json:"m" yaml:"m"`
	}
	err := json.Unmarshal([]byte(`{"m": null}`), &fromJSON)
	require.NoError(t, err)
	require.Equal(t, 0, fromJSON.M.Len())

	var fromYAML struct {
		M Ordered[int] `json:"m" yaml:"m"`
	}
	err = yaml.Unmarshal([]byte("m: ~\n"), &fromYAML)
	require.NoError(t, err)
	require.Equal(t, 0, fromYAML.M.Len())

	err = yaml.Unmarshal([]byte("m:\n"), &fromYAML)
	require.NoError(t, err)
	require.Equal(t, 0, fromYAML.M.Len())
}

func TestOrderedYAMLNumericKeysAreStrings(t *testing.T) {
	var o Ordered[string]
	err := yaml.Unmarshal([]byte("200: ok\n\"404\": missing\ndefault: error\n1.5: odd\n"), &o)
	require.NoError(t, err)

	require.Equal(t, []string{"200", "404", "default", "1.5"}, o.Keys)
	v, ok := o.Get("200")
	require.True(t, ok)
	require.Equal(t, "ok", v)
}

func TestOrderedYAMLMergeKeys(t *testing.T) {
	input := `
base: &base
  a: 1
  b: 2
other: &other
  b: 20
  c: 30
single:
  <<: *base
  b: 99
  d: 4
explicitFirst:
  b: 99
  <<: *base
list:
  <<: [*base, *other]
`
	var o Ordered[Ordered[int]]
	err := yaml.Unmarshal([]byte(input), &o)
	require.NoError(t, err)

	single := o.Map["single"]
	require.Equal(t, []string{"a", "b", "d"}, single.Keys)
	require.Equal(t, 99, single.Map["b"])
	require.Equal(t, 1, single.Map["a"])

	explicitFirst := o.Map["explicitFirst"]
	require.Equal(t, []string{"b", "a"}, explicitFirst.Keys)
	require.Equal(t, 99, explicitFirst.Map["b"])

	list := o.Map["list"]
	require.Equal(t, []string{"a", "b", "c"}, list.Keys)
	require.Equal(t, 2, list.Map["b"])
	require.Equal(t, 30, list.Map["c"])
}

func TestOrderedJSONDuplicateKeyKeepsFirstPosition(t *testing.T) {
	var o Ordered[int]
	err := json.Unmarshal([]byte(`{"a": 1, "b": 2, "a": 3}`), &o)
	require.NoError(t, err)

	require.Equal(t, []string{"a", "b"}, o.Keys)
	require.Equal(t, 3, o.Map["a"])
}

func TestOrderedMarshalJSONRoundTrip(t *testing.T) {
	input := `{"zeta":{"y":1,"x":2},"alpha":{},"mid":{"k":3}}`

	var o Ordered[Ordered[int]]
	err := json.Unmarshal([]byte(input), &o)
	require.NoError(t, err)

	out, err := json.Marshal(o)
	require.NoError(t, err)
	require.Equal(t, input, string(out))

	var again Ordered[Ordered[int]]
	err = json.Unmarshal(out, &again)
	require.NoError(t, err)
	require.Equal(t, o.Keys, again.Keys)
	require.Equal(t, []string{"y", "x"}, again.Map["zeta"].Keys)
}

func TestOrderedMarshalJSONEmpty(t *testing.T) {
	var o Ordered[int]
	out, err := json.Marshal(o)
	require.NoError(t, err)
	require.Equal(t, "{}", string(out))
}

func TestOrderedNilAndZeroValue(t *testing.T) {
	var nilOrdered *Ordered[int]
	require.Equal(t, 0, nilOrdered.Len())
	_, ok := nilOrdered.Get("x")
	require.False(t, ok)

	var o Ordered[int]
	_, ok = o.Get("x")
	require.False(t, ok)

	o.Set("b", 1)
	o.Set("a", 2)
	o.Set("b", 3)
	require.Equal(t, []string{"b", "a"}, o.Keys)
	require.Equal(t, 3, o.Map["b"])
}
