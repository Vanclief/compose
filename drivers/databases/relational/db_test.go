package relational

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/vanclief/ez"
)

// fakePgError carries a SQLSTATE and a schema name the way pgdriver.Error does
type fakePgError struct {
	code   string
	schema string
	detail string
}

func (e fakePgError) Error() string {
	return "ERROR: (SQLSTATE=" + e.code + ")"
}

func (e fakePgError) Field(k byte) string {
	switch k {
	case 'C':
		return e.code
	case 's':
		return e.schema
	case 'D':
		return e.detail
	}

	return ""
}

func TestIsCreateRace(t *testing.T) {
	races := map[string]fakePgError{
		"unique violation on the catalog": {code: "23505", schema: "pg_catalog"},
		"duplicate table":                 {code: "42P07"},
		"duplicate object":                {code: "42710"},
	}
	for name, err := range races {
		assert.True(t, isCreateRace(err), name)
		assert.True(t, isCreateRace(fmt.Errorf("create: %w", err)), name+" wrapped")
		assert.True(t, isCreateRace(ez.Wrap(err)), name+" wrapped by ez")
	}

	others := map[string]fakePgError{
		"unique violation on app data":         {code: "23505", schema: "public"},
		"unique violation with no schema name": {code: "23505"},
		"insufficient privilege":               {code: "42501"},
	}
	for name, err := range others {
		assert.False(t, isCreateRace(err), name)
		assert.False(t, isCreateRace(fmt.Errorf("create: %w", err)), name+" wrapped")
	}

	assert.False(t, isCreateRace(errors.New("table exists")), "not a PostgreSQL error")
	assert.False(t, isCreateRace(ez.New(ez.EINTERNAL, "Creating the table failed", nil)), "an ez error")
	assert.False(t, isCreateRace(nil), "no error")
}

func TestRetryCreate(t *testing.T) {
	// Each lost race is on another object, then the statement succeeds
	calls := 0
	err := retryCreate(func() error {
		calls++
		if calls <= 3 {
			return fakePgError{code: "23505", schema: "pg_catalog", detail: fmt.Sprintf("object %d", calls)}
		}
		return nil
	})
	assert.NoError(t, err)
	assert.Equal(t, 4, calls)

	// The same failure twice in a row is not a race
	calls = 0
	err = retryCreate(func() error {
		calls++
		return ez.Wrap(fakePgError{code: "42710"})
	})
	assert.Error(t, err)
	assert.Equal(t, 2, calls)

	// Another error is returned at once
	calls = 0
	err = retryCreate(func() error {
		calls++
		return fakePgError{code: "42501"}
	})
	assert.Error(t, err)
	assert.Equal(t, 1, calls)

	// Races without end stop at createAttempts
	calls = 0
	err = retryCreate(func() error {
		calls++
		return fakePgError{code: "23505", schema: "pg_catalog", detail: fmt.Sprintf("object %d", calls)}
	})
	assert.Error(t, err)
	assert.Equal(t, createAttempts, calls)
}
