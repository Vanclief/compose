package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/vanclief/compose/components/rest/api/internal/fixture"
	"github.com/vanclief/compose/components/rest/apitest"
)

// fakeTB captures Errorf calls.
type fakeTB struct {
	testing.TB
	errors []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Errorf(format string, args ...any) {
	f.errors = append(f.errors, fmt.Sprintf(format, args...))
}

func TestContract(t *testing.T) {
	s := newServer(t)
	contract := apitest.New(t, s.reg)

	rec := contract.Do(t, s.e, "runs.list", httptest.NewRequest(http.MethodGet, "/runs?limit=10", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = contract.Do(t, s.e, "runs.get", httptest.NewRequest(http.MethodGet, "/runs/"+fixture.KnownID.String(), nil))
	require.Equal(t, http.StatusOK, rec.Code)

	rec = contract.Do(t, s.e, "runs.get", httptest.NewRequest(http.MethodGet, "/runs/"+fixture.MissingID.String(), nil))
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = contract.Do(t, s.e, "runs.list", httptest.NewRequest(http.MethodGet, "/runs?limit=5000", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)

	contract.Do(t, s.e, "health.check", httptest.NewRequest(http.MethodGet, "/health", nil))

	// Undocumented response field fails the strict contract.
	fake := &fakeTB{TB: t}
	contract.AssertResponse(fake, "runs.get", http.StatusOK, []byte(`{
		"id": "0192f3a1-7b2c-7def-8a12-3456789abcde", "name": "x", "status": "running",
		"max_total_runs": 0, "created_at": "2026-10-11T12:00:00Z", "secret": "leak"}`))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "secret is not a known field")

	// Missing required field and bad enum fail too.
	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "runs.get", http.StatusOK, []byte(`{"id": "0192f3a1-7b2c-7def-8a12-3456789abcde", "status": "bogus"}`))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "name is required")
	require.Contains(t, fake.errors[0], "status must be one of")

	// Error envelope.
	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "runs.get", http.StatusNotFound, []byte(`{"error":{"code":"not_found","message":"x","request_id":"1"}}`))
	require.Empty(t, fake.errors)

	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "runs.get", http.StatusNotFound, []byte(`{"error":{"code":"not_found"}}`))
	require.Len(t, fake.errors, 1)

	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "nope", http.StatusOK, []byte(`{}`))
	require.Len(t, fake.errors, 1)
}
