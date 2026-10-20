package enums

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

type color string

type unregistered string

func TestSetRecordsValuesByType(t *testing.T) {
	set := Set([]color{"red", "green", "blue"})
	require.Len(t, set, 3)

	values, ok := Values(reflect.TypeFor[color]())
	require.True(t, ok)
	require.Equal(t, []string{"red", "green", "blue"}, values)

	values[0] = "mutated"
	again, _ := Values(reflect.TypeFor[color]())
	require.Equal(t, "red", again[0])

	_, ok = Values(reflect.TypeFor[unregistered]())
	require.False(t, ok)
}

type mode string

func TestSetRecordsUnionAcrossCalls(t *testing.T) {
	Set([]mode{"", "organization", "custom"})
	Set([]mode{"organization", "custom"})
	Set([]mode{"legacy"})

	values, ok := Values(reflect.TypeFor[mode]())
	require.True(t, ok)
	require.Equal(t, []string{"", "organization", "custom", "legacy"}, values)
}
