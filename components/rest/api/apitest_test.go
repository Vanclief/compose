package api_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/internal/fixture"
	"github.com/vanclief/compose/components/rest/apitest"
	"github.com/vanclief/compose/types"
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

	// Blob and negotiated responses are checked against the declared media types.
	report := httptest.NewRequest(http.MethodGet, "/runs/"+fixture.KnownID.String()+"/report", nil)
	rec = contract.Do(t, s.e, "runs.report", report)
	require.Equal(t, "application/pdf", rec.Header().Get("Content-Type"))

	export := httptest.NewRequest(http.MethodGet, "/runs/export", nil)
	export.Header.Set("Accept", "text/csv")
	rec = contract.Do(t, s.e, "runs.export", export)
	require.Equal(t, "text/csv", rec.Header().Get("Content-Type"))

	rec = contract.Do(t, s.e, "runs.export", httptest.NewRequest(http.MethodGet, "/runs/export", nil))
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	fake := &fakeTB{TB: t}
	contract.Do(fake, s.e, "runs.get", httptest.NewRequest(http.MethodGet, "/runs/"+fixture.KnownID.String()+"/report", nil))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], `runs.get 200 response has undeclared content type "application/pdf"`)

	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "runs.report", http.StatusOK, []byte(`{}`))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "runs.report does not produce application/json")

	// Undocumented response field fails the strict contract.
	fake = &fakeTB{TB: t}
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

	// A repeated key fails even when its last value matches.
	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "runs.get", http.StatusNotFound, []byte(`{"error":{"code":"x","code":"not_found","message":"x","request_id":"1"}}`))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "runs.get 404 response does not match the contract: body repeats an object key")

	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "nope", http.StatusOK, []byte(`{}`))
	require.Len(t, fake.errors, 1)
}

type hiddenResp struct {
	Name   string      `json:"name"`
	Secret string      `json:"secret" hidden:"true"`
	Inner  hiddenInner `json:"inner"`
}

type hiddenInner struct {
	Token *string `json:"token,omitempty" hidden:"true"`
}

// TestContractHiddenField: a hidden field is left out of the published
// documents but sent, so the strict contract lists it and checks its value;
// a field that is neither documented nor hidden still fails.
func TestContractHiddenField(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/hidden"), "", "get", func(ctx context.Context, req *fixture.HealthRequest) (hiddenResp, error) {
		token := "t"
		return hiddenResp{Name: "a", Secret: "b", Inner: hiddenInner{Token: &token}}, nil
	})
	reg.Ready()

	contract := apitest.New(t, reg)
	fake := &fakeTB{TB: t}
	rec := contract.Do(fake, e, "hidden.get", httptest.NewRequest(http.MethodGet, "/hidden", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"name":"a","secret":"b","inner":{"token":"t"}}`, rec.Body.String())
	require.Empty(t, fake.errors)

	// A hidden field stays optional.
	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "hidden.get", http.StatusOK, []byte(`{"name":"a","inner":{}}`))
	require.Empty(t, fake.errors)

	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "hidden.get", http.StatusOK, []byte(`{"name":"a","secret":"b","other":"c"}`))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "other is not a known field")

	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "hidden.get", http.StatusOK, []byte(`{"name":"a","secret":1}`))
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "secret must be string")

	for _, v := range []api.Version{api.V31, api.V30} {
		raw, err := reg.Spec(v)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "secret")
		require.NotContains(t, string(raw), "token")
	}
}

type labelsResponse struct {
	Labels map[string]string `json:"labels"`
}

// TestContractNilMap: a nil map is sent as null, which the strict contract
// and both documents allow, as for a nil slice.
func TestContractNilMap(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.GET[fixture.HealthRequest, labelsResponse](reg.Group("/labels"), "", "get", stub)
	reg.Ready()

	contract := apitest.New(t, reg)
	rec := contract.Do(t, e, "labels.get", httptest.NewRequest(http.MethodGet, "/labels", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"labels":null}`, rec.Body.String())

	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	doc := decode(t, raw)
	ref := dig(t, doc, "paths", "/labels", "get", "responses", "200", "content", "application/json", "schema", "$ref").(string)
	name := strings.TrimPrefix(ref, "#/components/schemas/")
	require.Equal(t, []any{"object", "null"}, dig(t, doc, "components", "schemas", name, "properties", "labels", "type"))

	raw, err = reg.Spec(api.V30)
	require.NoError(t, err)
	doc = decode(t, raw)
	require.Equal(t, "object", dig(t, doc, "components", "schemas", name, "properties", "labels", "type"))
	require.Equal(t, true, dig(t, doc, "components", "schemas", name, "properties", "labels", "nullable"))
}

type addressResponse struct {
	IP       net.IP                 `json:"ip"`
	Pointer  *net.IP                `json:"pointer"`
	Optional types.Optional[net.IP] `json:"optional,omitzero"`
}

// TestContractZeroIP: a zero net.IP held by value is written as "", which
// the ip format refuses, so the contract fails it; a nil *net.IP is written
// as null and an unset types.Optional[net.IP] is left out, and both pass.
func TestContractZeroIP(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	var res addressResponse
	api.GET(reg.Group("/address"), "", "get", func(ctx context.Context, req *fixture.HealthRequest) (addressResponse, error) {
		return res, nil
	})
	reg.Ready()

	contract := apitest.New(t, reg)
	fake := &fakeTB{TB: t}
	rec := contract.Do(fake, e, "address.get", httptest.NewRequest(http.MethodGet, "/address", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"ip":"","pointer":null}`, rec.Body.String())
	require.Len(t, fake.errors, 1)
	require.Contains(t, fake.errors[0], "address.get 200 response does not match the contract")
	require.Contains(t, fake.errors[0], "ip must be a valid ip")

	res.IP = net.ParseIP("192.0.2.1")
	fake = &fakeTB{TB: t}
	rec = contract.Do(fake, e, "address.get", httptest.NewRequest(http.MethodGet, "/address", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"ip":"192.0.2.1","pointer":null}`, rec.Body.String())
	require.Empty(t, fake.errors)
}

// TestContractHead: rest.ErrorHandler answers a HEAD request with the status
// alone, so the body of a HEAD operation's response is not checked, success
// or error.
func TestContractHead(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	g := reg.Group("/probes")
	api.Handle(g, http.MethodHead, "", "found", func(ctx context.Context, req *fixture.HealthRequest) (fixture.HealthResponse, error) {
		return fixture.HealthResponse{OK: true}, nil
	})
	api.Handle(g, http.MethodHead, "/missing", "missing", func(ctx context.Context, req *fixture.HealthRequest) (fixture.HealthResponse, error) {
		return fixture.HealthResponse{}, ez.New(ez.ENOTFOUND, "no probe", nil)
	})
	reg.Ready()

	contract := apitest.New(t, reg)
	fake := &fakeTB{TB: t}
	rec := contract.Do(fake, e, "probes.missing", httptest.NewRequest(http.MethodHead, "/probes/missing", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Empty(t, rec.Body.String())
	require.Empty(t, fake.errors)

	rec = contract.Do(fake, e, "probes.found", httptest.NewRequest(http.MethodHead, "/probes", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, fake.errors)

	contract.AssertResponse(fake, "probes.found", http.StatusNotFound, nil)
	require.Empty(t, fake.errors)
}

// TestContractEmptyBody: an empty body fails where the contract expects
// JSON, even for a Res whose schema allows null.
func TestContractEmptyBody(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.GET[fixture.HealthRequest, []fixture.HealthResponse](reg.Group("/checks"), "", "list", stub)
	reg.Ready()

	contract := apitest.New(t, reg)
	fake := &fakeTB{TB: t}
	contract.AssertResponse(fake, "checks.list", http.StatusOK, []byte(" "))
	require.Equal(t, []string{"apitest: checks.list 200 response has no body"}, fake.errors)

	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "checks.list", http.StatusNotFound, nil)
	require.Equal(t, []string{"apitest: checks.list 404 response has no body"}, fake.errors)

	// A sent null still passes.
	fake = &fakeTB{TB: t}
	contract.AssertResponse(fake, "checks.list", http.StatusOK, []byte("null"))
	require.Empty(t, fake.errors)
}
