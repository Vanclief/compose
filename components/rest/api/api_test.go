package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest"
	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/internal/fixture"
	"github.com/vanclief/compose/components/rest/apitest"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/compose/drivers/databases/relational/postgres/pagination"
	"github.com/vanclief/compose/types"
)

const sentinel = "ZZZ_SENTINEL"

var projectID = uuid.MustParse("0192f3a1-0000-7000-8000-000000000001")

type server struct {
	e   *echo.Echo
	reg *api.Registry
	app *fixture.App
}

// newEcho returns an Echo instance that answers errors with
// rest.ErrorHandler and gives every request an id, as rest.Configure does:
// on the response, where the error envelope takes it from, and where
// requests.New reads it. It runs no other middleware, so nothing reads the
// body before the registry does.
func newEcho() *echo.Echo {
	e := echo.New()
	e.HTTPErrorHandler = rest.ErrorHandler()
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			id := uuid.New().String()
			requests.SetRequestID(c.Request().Header, id)
			c.Response().Header().Set(echo.HeaderXRequestID, id)
			return next(c)
		}
	})
	return e
}

func newServer(t *testing.T) *server {
	t.Helper()

	e := newEcho()
	app := &fixture.App{}
	reg := api.New(e,
		api.Info("auditor3", "1.0.0"),
		api.Description("fixture API"),
		api.APIKeyHeader("API-KEY"),
		api.Server("https://api.example.com"),
		api.Authenticate(app.Authenticate),
	)

	rg := reg.Group("/runs", api.Timeout(60*time.Second))
	api.GET(rg, "", "list", app.List, api.Doc("List runs", "Cursor paginated."))
	api.GET(rg, "/:id", "get", app.Get)
	api.POST(rg, "", "create", app.Create)
	api.PATCH(rg, "/:id", "update", app.Update)
	api.PUT(rg, "/:id", "replace", app.Update, api.Hidden())
	api.POST(rg, "/archive", "archive", app.Archive, api.Hidden(), api.MaxBodyBytes(1024))
	api.GET(rg, "/:id/report", "report", app.Report, api.ContentType("application/pdf"))
	api.GET(rg, "/export", "export", app.Export, api.Produces("text/csv", app.ExportCSV))
	api.POST(rg, "/:id/artifacts", "upload", app.Upload, api.MaxBodyBytes(4096))
	api.PATCH(rg, "/:id/edit", "edit", app.Edit)

	hg := reg.Group("/health")
	api.GET(hg, "", "check", app.Health, api.Public(), api.Deprecated())

	reg.Ready()

	return &server{e: e, reg: reg, app: app}
}

func (s *server) do(method, target, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] == "" {
			req.Header.Del(header[i])
			continue
		}
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	s.e.ServeHTTP(rec, req)
	return rec
}

// requireError asserts status and envelope, that the message contains want,
// and that it never echoes the sentinel.
func requireError(t *testing.T, rec *httptest.ResponseRecorder, status int, want string) handler.ErrorResponse {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())

	var resp handler.ErrorResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	require.NotEmpty(t, resp.Error.RequestID)
	require.Contains(t, resp.Error.Message, want)
	require.NotContains(t, rec.Body.String(), sentinel)
	if status == http.StatusBadRequest {
		require.Equal(t, ez.EINVALID, resp.Error.Code)
	}
	return resp
}

func TestListQuery(t *testing.T) {
	s := newServer(t)

	rec := s.do(http.MethodGet, "/runs?limit=10&status=running&project_id="+projectID.String(), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := s.app.Last().(*fixture.ListRequest)
	require.Equal(t, 10, got.Limit)
	require.Equal(t, fixture.RunningStatus, got.Status)
	require.Equal(t, projectID, got.ProjectID)

	// Unknown (case-mismatched) query keys are ignored, never bound; the
	// handler sees the default limit Validate applied.
	rec = s.do(http.MethodGet, "/runs?LIMIT=5000&Status=bogus", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = s.app.Last().(*fixture.ListRequest)
	require.Equal(t, pagination.DEFAULT_LIMIT, got.Limit)
	require.Equal(t, fixture.RunStatus(""), got.Status)

	rec = s.do(http.MethodGet, "/runs?limit=5000", "")
	requireError(t, rec, http.StatusBadRequest, "query limit must be <= 1000")

	rec = s.do(http.MethodGet, "/runs?limit=abc", "")
	requireError(t, rec, http.StatusBadRequest, "query limit must be integer")

	rec = s.do(http.MethodGet, "/runs?status=bogus", "")
	requireError(t, rec, http.StatusBadRequest, "query status must be one of")

	rec = s.do(http.MethodGet, "/runs?status="+sentinel, "")
	requireError(t, rec, http.StatusBadRequest, "must be one of")

	rec = s.do(http.MethodGet, "/runs?cursor=&limit=", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = s.app.Last().(*fixture.ListRequest)
	require.Equal(t, "", got.Cursor)
	require.Equal(t, pagination.DEFAULT_LIMIT, got.Limit)

	rec = s.do(http.MethodGet, "/runs?cursor=abc", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "abc", s.app.Last().(*fixture.ListRequest).Cursor)
}

func TestGetPath(t *testing.T) {
	s := newServer(t)

	rec := s.do(http.MethodGet, "/runs/not-a-uuid", "")
	requireError(t, rec, http.StatusBadRequest, "path id must be a valid uuid")

	rec = s.do(http.MethodGet, "/runs/"+fixture.KnownID.String(), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, fixture.KnownID, s.app.Last().(*fixture.GetRequest).RunID)

	rec = s.do(http.MethodGet, "/runs/"+fixture.MissingID.String(), "")
	resp := requireError(t, rec, http.StatusNotFound, "run not found")
	require.Equal(t, ez.ENOTFOUND, resp.Error.Code)
}

func TestUpdateBody(t *testing.T) {
	s := newServer(t)
	target := "/runs/" + fixture.KnownID.String()

	rec := s.do(http.MethodPatch, target, `{"max_total_runs":-1}`)
	requireError(t, rec, http.StatusBadRequest, "max_total_runs must be >= 0")

	rec = s.do(http.MethodPatch, target, `{"Status":"failed"}`)
	requireError(t, rec, http.StatusBadRequest, `did you mean "status"`)

	rec = s.do(http.MethodPatch, target, `{"STATUS":"`+sentinel+`"}`)
	requireError(t, rec, http.StatusBadRequest, `did you mean "status"`)

	rec = s.do(http.MethodPatch, target, `{"status":"pending"}`)
	requireError(t, rec, http.StatusBadRequest, "status must be one of")

	// Shape passes, the cross-field Validate still runs, before the handler.
	before := s.app.Last()
	rec = s.do(http.MethodPatch, target, `{"status":"failed"}`)
	requireError(t, rec, http.StatusBadRequest, "failure_reason is required when status is failed")
	require.Equal(t, before, s.app.Last(), "handler invoked despite Validate failing")

	rec = s.do(http.MethodPatch, target, `{"status":"failed","failure_reason":"x","max_total_runs":0}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := s.app.Last().(*fixture.UpdateRequest)
	require.Equal(t, fixture.KnownID, got.RunID)
	require.NotNil(t, got.Status)
	require.Equal(t, fixture.FailedStatus, *got.Status)
	require.NotNil(t, got.FailureReason)
	require.Equal(t, "x", *got.FailureReason)
	require.NotNil(t, got.MaxTotalRuns)
	require.Equal(t, 0, *got.MaxTotalRuns)

	rec = s.do(http.MethodPatch, target, `{"failure_reason":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Nil(t, s.app.Last().(*fixture.UpdateRequest).FailureReason)

	rec = s.do(http.MethodPatch, target, `{"unknown_key":1}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = s.do(http.MethodPatch, target, `{"status":"failed"} garbage`)
	requireError(t, rec, http.StatusBadRequest, "body is not valid JSON")

	rec = s.do(http.MethodPatch, target, `{"failure_reason":"x"}`, echo.HeaderContentType, "text/plain")
	requireError(t, rec, http.StatusBadRequest, "content type must be application/json")

	rec = s.do(http.MethodPatch, target, `{"failure_reason":"x"}`, echo.HeaderContentType, "application/merge-patch+json")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = s.do(http.MethodPatch, target, `{"failure_reason":"x"}`, echo.HeaderContentType, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = s.do(http.MethodPatch, target, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Hidden operations are served and validated.
	rec = s.do(http.MethodPut, target, `{"max_total_runs":-1}`)
	requireError(t, rec, http.StatusBadRequest, "max_total_runs must be >= 0")

	rec = s.do(http.MethodPost, "/runs/archive", `{}`)
	requireError(t, rec, http.StatusBadRequest, "reason is required")

	rec = s.do(http.MethodPost, "/runs/archive", `{"reason":"done"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestOptionalBody(t *testing.T) {
	s := newServer(t)
	target := "/runs/" + fixture.KnownID.String() + "/edit"

	rec := s.do(http.MethodPatch, target, `{"name":"x","status":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := s.app.Last().(*fixture.EditRequest)
	require.Equal(t, fixture.KnownID, got.RunID)
	require.True(t, got.Name.Set)
	require.False(t, got.Name.Null)
	require.Equal(t, "x", got.Name.Value)
	require.True(t, got.Status.Set)
	require.True(t, got.Status.Null)
	require.False(t, got.Config.Set)
	require.Equal(t, "x", decode(t, rec.Body.Bytes())["name"])

	rec = s.do(http.MethodPatch, target, `{"status":"failed","config":{"retries":2}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = s.app.Last().(*fixture.EditRequest)
	require.False(t, got.Name.Set)
	status, ok := got.Status.Get()
	require.True(t, ok)
	require.Equal(t, fixture.FailedStatus, status)
	config, ok := got.Config.Get()
	require.True(t, ok)
	require.Equal(t, 2, config.Retries)

	rec = s.do(http.MethodPatch, target, `{"name":5}`)
	requireError(t, rec, http.StatusBadRequest, "name must be string or null")

	rec = s.do(http.MethodPatch, target, `{"name":""}`)
	requireError(t, rec, http.StatusBadRequest, "name must be at least 1 characters")

	rec = s.do(http.MethodPatch, target, `{"status":"`+sentinel+`"}`)
	requireError(t, rec, http.StatusBadRequest, "status must be one of")

	// The case-collision check sees through the nullable wrapper.
	rec = s.do(http.MethodPatch, target, `{"Name":"x"}`)
	requireError(t, rec, http.StatusBadRequest, `Name is not a known field, did you mean "name"`)

	rec = s.do(http.MethodPatch, target, `{"config":{"Retries":1}}`)
	requireError(t, rec, http.StatusBadRequest, `config.Retries is not a known field, did you mean "retries"`)

	rec = s.do(http.MethodPatch, target, `{"config":{"retries":-1}}`)
	requireError(t, rec, http.StatusBadRequest, "config.retries must be >= 0")
}

func TestCreate(t *testing.T) {
	s := newServer(t)

	rec := s.do(http.MethodPost, "/runs", `{}`)
	requireError(t, rec, http.StatusBadRequest, "name is required")

	// Nested keys are matched case-insensitively by encoding/json too, so the
	// collision check must recurse through objects, nullable refs and arrays.
	rec = s.do(http.MethodPost, "/runs", `{"name":"x","config":{"Retries":1}}`)
	requireError(t, rec, http.StatusBadRequest, `config.Retries is not a known field, did you mean "retries"`)

	rec = s.do(http.MethodPost, "/runs", `{"name":"x","steps":[{"retries":1},{"RETRIES":"`+sentinel+`"}]}`)
	requireError(t, rec, http.StatusBadRequest, `steps.1.RETRIES is not a known field, did you mean "retries"`)

	rec = s.do(http.MethodPost, "/runs", `{"name":"x","config":{"retries":-1}}`)
	requireError(t, rec, http.StatusBadRequest, "config.retries must be >= 0")

	rec = s.do(http.MethodPost, "/runs", `{"name":"x","config":null,"steps":[{"retries":2}]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = s.do(http.MethodPost, "/runs", "")
	requireError(t, rec, http.StatusBadRequest, "body is required")

	rec = s.do(http.MethodPost, "/runs", `{"name":""}`)
	requireError(t, rec, http.StatusBadRequest, "name must be at least 1 characters")

	rec = s.do(http.MethodPost, "/runs", `{"name":1}`)
	requireError(t, rec, http.StatusBadRequest, "name must be string")

	rec = s.do(http.MethodPost, "/runs", `{"name":"nightly"}`, "X-Trace", "abc")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := s.app.Last().(*fixture.CreateRequest)
	require.Equal(t, "nightly", got.Name)
	require.Equal(t, "abc", got.Trace)
}

func TestPublic(t *testing.T) {
	s := newServer(t)

	rec := s.do(http.MethodGet, "/health", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, `{"ok":true}`, rec.Body.String())
}

func TestNotReady(t *testing.T) {
	e := newEcho()
	app := &fixture.App{}
	reg := api.New(e, api.Authenticate(app.Authenticate))
	g := reg.Group("/health")
	api.GET(g, "", "check", app.Health)

	s := &server{e: e, reg: reg}
	rec := s.do(http.MethodGet, "/health", "")
	resp := requireError(t, rec, http.StatusInternalServerError, "")
	require.Equal(t, ez.EINTERNAL, resp.Error.Code)

	reg.Ready()
	rec = s.do(http.MethodGet, "/health", "")
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestConcurrentRequests(t *testing.T) {
	s := newServer(t)

	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 25; j++ {
				rec := s.do(http.MethodGet, "/runs?limit=10&status=running", "")
				if rec.Code != http.StatusOK {
					t.Errorf("status %d: %s", rec.Code, rec.Body.String())
					return
				}
				rec = s.do(http.MethodGet, "/runs?limit=5000", "")
				if rec.Code != http.StatusBadRequest {
					t.Errorf("status %d: %s", rec.Code, rec.Body.String())
					return
				}
			}
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}

func TestTimeParams(t *testing.T) {
	s := newServer(t)

	rec := s.do(http.MethodGet, "/runs?at=2026-10-11T10:00:00Z", "", "X-Since", "Sun, 11 Oct 2026 10:00:00 GMT")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := s.app.Last().(*fixture.ListRequest)
	want := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	require.True(t, got.At.Equal(want), got.At)
	require.NotNil(t, got.Since)
	require.True(t, got.Since.Equal(want), got.Since)

	// santhosh's date-time accepts these, time.Time rejects them: a client
	// error, not a 500.
	for _, at := range []string{"2026-10-11t10:00:00z", "2026-12-31T23:59:60Z"} {
		rec = s.do(http.MethodGet, "/runs?at="+at, "")
		resp := requireError(t, rec, http.StatusBadRequest, "query at is invalid")
		require.NotContains(t, resp.Error.Message, at)
	}

	// date-time-http is not asserted by santhosh; parsing decides.
	rec = s.do(http.MethodGet, "/runs", "", "X-Since", "2026-10-11T10:00:00Z")
	requireError(t, rec, http.StatusBadRequest, "header x-since is invalid")

	rec = s.do(http.MethodGet, "/runs?at="+sentinel+"&limit=1", "", "X-Since", sentinel)
	requireError(t, rec, http.StatusBadRequest, "query at must be a valid date-time")
}

type bigIntRequest struct {
	requests.NoRules
	U *uint64 `json:"-" query:"u"`
	I *int64  `json:"-" query:"i"`
}

// TestBigIntParams: a uint64 parameter takes values at or above 2^63, and a
// signed one answers them with a 400.
func TestBigIntParams(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	var got *bigIntRequest
	api.GET(reg.Group("/big"), "", "get", func(ctx context.Context, req *bigIntRequest) (fixture.HealthResponse, error) {
		got = req
		return fixture.HealthResponse{}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodGet, "/big?u=18446744073709551615", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, got.U)
	require.Equal(t, uint64(18446744073709551615), *got.U)

	rec = s.do(http.MethodGet, "/big?i=9223372036854775808", "")
	requireError(t, rec, http.StatusBadRequest, "query i is invalid")

	// An unsigned parameter takes one leading plus sign, as a signed one does.
	rec = s.do(http.MethodGet, "/big?u=%2B5", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, got.U)
	require.Equal(t, uint64(5), *got.U)
	for _, u := range []string{"-5", "%2B%2B5"} {
		rec = s.do(http.MethodGet, "/big?u="+u, "")
		requireError(t, rec, http.StatusBadRequest, "query u")
	}

	// A multipart integer above int64 decodes into an int field as a 400.
	body, contentType := form(t,
		part{name: "category", value: "log"},
		part{name: "attempt", value: "9223372036854775808"},
		part{name: "manifest", value: "steps: 3", filename: "manifest.yaml"},
	)
	rec = newServer(t).do(http.MethodPost, "/runs/"+fixture.KnownID.String()+"/artifacts", body, echo.HeaderContentType, contentType)
	requireError(t, rec, http.StatusBadRequest, "form values could not be decoded")
}

// textParam accepts "ok" and refuses anything else with the error its text
// names: "wrapped" an ez.Wrap of a plain error, as the repo convention
// writes it, "internal" an ez EINTERNAL, anything else a plain error.
type textParam string

func (p *textParam) UnmarshalText(text []byte) error {
	switch string(text) {
	case "ok":
		*p = textParam(text)
		return nil
	case "wrapped":
		return ez.Wrap(errors.New("unknown value"))
	case "internal":
		return ez.New(ez.EINTERNAL, "lookup table missing", nil)
	}
	return errors.New("unknown value")
}

type textParamRequest struct {
	requests.NoRules
	P textParam `json:"-" query:"p"`
}

// TestTextParamErrors: a TextUnmarshaler's error refuses client input, so it
// is a 400 whatever its ez code.
func TestTextParamErrors(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.GET[textParamRequest, fixture.HealthResponse](reg.Group("/text"), "", "get", stub)
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodGet, "/text?p=ok", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, value := range []string{"bad", "wrapped", "internal"} {
		rec = s.do(http.MethodGet, "/text?p="+value, "")
		resp := requireError(t, rec, http.StatusBadRequest, "query p is invalid")
		require.Equal(t, "query p is invalid", resp.Error.Message, value)
	}
}

type localeRequest struct {
	requests.NoRules
	Locale  types.Locale   `json:"-" query:"locale"`
	Pointer *types.Locale  `json:"-" query:"pointer"`
	List    []types.Locale `json:"-" query:"list"`
}

// TestLocaleParams: a types.Locale parameter is normalised as in a JSON
// body, an unsupported value becoming the default locale.
func TestLocaleParams(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	var got *localeRequest
	api.GET(reg.Group("/locale"), "", "get", func(ctx context.Context, req *localeRequest) (fixture.HealthResponse, error) {
		got = req
		return fixture.HealthResponse{}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodGet, "/locale?locale=es&pointer=es&list=es,"+sentinel, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, types.Locale("es-MX"), got.Locale)
	require.NotNil(t, got.Pointer)
	require.Equal(t, types.Locale("es-MX"), *got.Pointer)
	require.Equal(t, []types.Locale{"es-MX", "en-US"}, got.List)

	rec = s.do(http.MethodGet, "/locale?locale="+sentinel+"&pointer="+sentinel, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, types.Locale("en-US"), got.Locale)
	require.NotNil(t, got.Pointer)
	require.Equal(t, types.Locale("en-US"), *got.Pointer)

	rec = s.do(http.MethodGet, "/locale", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, types.Locale(""), got.Locale)
	require.Nil(t, got.Pointer)
	require.Nil(t, got.List)
}

func TestMaxBody(t *testing.T) {
	s := newServer(t)
	huge := `{"reason":"` + strings.Repeat("a", 2<<20) + `"}`

	rec := s.do(http.MethodPost, "/runs/archive", huge)
	requireError(t, rec, http.StatusBadRequest, "body exceeds 1024 bytes")

	rec = s.do(http.MethodPost, "/runs", huge)
	requireError(t, rec, http.StatusBadRequest, "body exceeds 1048576 bytes")

	// MaxBodyBytes on New is the default, overridden per group or operation.
	e := newEcho()
	app := &fixture.App{}
	reg := api.New(e, api.MaxBodyBytes(64), api.Authenticate(app.Authenticate))
	api.POST(reg.Group("/small"), "", "archive", app.Archive)
	api.POST(reg.Group("/big", api.MaxBodyBytes(1024)), "", "archive", app.Archive)
	api.POST(reg.Group("/route"), "", "archive", app.Archive, api.MaxBodyBytes(1024))
	reg.Ready()
	small := &server{e: e, reg: reg}

	body := `{"reason":"` + strings.Repeat("a", 100) + `"}`
	rec = small.do(http.MethodPost, "/small", body)
	requireError(t, rec, http.StatusBadRequest, "body exceeds 64 bytes")

	rec = small.do(http.MethodPost, "/big", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = small.do(http.MethodPost, "/route", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestMapKeysNotEchoed(t *testing.T) {
	s := newServer(t)
	key := `evil\nkey<script>` + strings.Repeat("A", 200)

	rec := s.do(http.MethodPost, "/runs", `{"name":"x","labels":{"`+key+`":{"retries":-1}}}`)
	resp := requireError(t, rec, http.StatusBadRequest, "labels.*.retries must be >= 0")
	requireNoKey(t, resp.Error.Message)

	rec = s.do(http.MethodPost, "/runs", `{"name":"x","labels":{"`+key+`":{"Retries":1}}}`)
	resp = requireError(t, rec, http.StatusBadRequest, `labels.*.Retries is not a known field, did you mean "retries"`)
	requireNoKey(t, resp.Error.Message)

	rec = s.do(http.MethodPost, "/runs", `{"name":"x","labels":{"ok":{"retries":1}}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, s.app.Last().(*fixture.CreateRequest).Labels["ok"].Retries)
}

// TestNullMap: a map field accepts null, as a slice field does, and the
// handler receives a nil map.
func TestNullMap(t *testing.T) {
	s := newServer(t)

	rec := s.do(http.MethodPost, "/runs", `{"name":"x","labels":null,"steps":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := s.app.Last().(*fixture.CreateRequest)
	require.Nil(t, got.Labels)
	require.Nil(t, got.Steps)
}

func requireNoKey(t *testing.T, msg string) {
	t.Helper()
	require.NotContains(t, msg, "<script>")
	require.NotContains(t, msg, "\n")
	require.NotContains(t, msg, "AAAA")
}

func TestMergedErrors(t *testing.T) {
	s := newServer(t)

	rec := s.do(http.MethodPost, "/runs", `{"name":"","config":{"Retries":1},"steps":[{"retries":-1}]}`)
	resp := requireError(t, rec, http.StatusBadRequest, `config.Retries is not a known field, did you mean "retries"`)
	require.Contains(t, resp.Error.Message, "name must be at least 1 characters")
	require.Contains(t, resp.Error.Message, "steps.0.retries must be >= 0")
}

func TestBlob(t *testing.T) {
	s := newServer(t)
	target := "/runs/" + fixture.KnownID.String() + "/report"

	// A Blob operation writes the app's bytes whatever the Accept header says.
	for _, accept := range []string{"", "application/json", "application/pdf"} {
		rec := s.do(http.MethodGet, target, "", echo.HeaderAccept, accept)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "application/pdf", rec.Header().Get(echo.HeaderContentType))
		require.Equal(t, `attachment; filename="run report.pdf"`, rec.Header().Get(echo.HeaderContentDisposition))
		require.Equal(t, "%PDF-1.7 "+fixture.KnownID.String(), rec.Body.String())
		require.Equal(t, "application/pdf", s.app.LastAccept())
	}

	// Errors keep the JSON envelope.
	rec := s.do(http.MethodGet, "/runs/"+fixture.MissingID.String()+"/report", "", echo.HeaderAccept, "application/pdf")
	requireError(t, rec, http.StatusNotFound, "run not found")
	require.Equal(t, echo.MIMEApplicationJSON, rec.Header().Get(echo.HeaderContentType))

	rec = s.do(http.MethodGet, "/runs/not-a-uuid/report", "")
	requireError(t, rec, http.StatusBadRequest, "path id must be a valid uuid")
}

func TestNegotiation(t *testing.T) {
	s := newServer(t)

	tests := []struct {
		accept string
		want   string
	}{
		{"", echo.MIMEApplicationJSON},
		{"*/*", echo.MIMEApplicationJSON},
		{"image/png", echo.MIMEApplicationJSON},
		{"text/csv", "text/csv"},
		{"TEXT/CSV; charset=utf-8", "text/csv"},
		// Accept order decides; q-values are ignored.
		{"text/csv;q=0.1, application/json", "text/csv"},
		{"application/json, text/csv", echo.MIMEApplicationJSON},
		{"application/xml, text/*", "text/csv"},
		{"image/png, */*", echo.MIMEApplicationJSON},
	}
	for _, tt := range tests {
		rec := s.do(http.MethodGet, "/runs/export?status=running", "", echo.HeaderAccept, tt.accept)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, tt.want, s.app.LastAccept(), tt.accept)
		require.Equal(t, tt.want, rec.Header().Get(echo.HeaderContentType), tt.accept)
		require.Equal(t, fixture.RunningStatus, s.app.Last().(*fixture.ExportRequest).Status)
		if tt.want == "text/csv" {
			require.Equal(t, fixture.ExportCSV, rec.Body.String())
			require.Equal(t, `attachment; filename=runs.csv`, rec.Header().Get(echo.HeaderContentDisposition))
			continue
		}
		require.Empty(t, rec.Header().Get(echo.HeaderContentDisposition))
		var resp fixture.ListResponse
		err := json.Unmarshal(rec.Body.Bytes(), &resp)
		require.NoError(t, err)
		require.Len(t, resp.Runs, 1)
	}

	// Validation errors are JSON even when CSV was negotiated.
	rec := s.do(http.MethodGet, "/runs/export?status=bogus", "", echo.HeaderAccept, "text/csv")
	requireError(t, rec, http.StatusBadRequest, "query status must be one of")

	// Operations without Produces negotiate application/json.
	rec = s.do(http.MethodGet, "/runs", "", echo.HeaderAccept, "text/csv")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, echo.MIMEApplicationJSON, s.app.LastAccept())
}

// part is a multipart form value, or a file when filename is set.
type part struct {
	name     string
	value    string
	filename string
}

// form encodes parts as multipart/form-data and returns the body and its
// Content-Type.
func form(t *testing.T, parts ...part) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p.filename == "" {
			err := w.WriteField(p.name, p.value)
			require.NoError(t, err)
			continue
		}
		fw, err := w.CreateFormFile(p.name, p.filename)
		require.NoError(t, err)
		_, err = fw.Write([]byte(p.value))
		require.NoError(t, err)
	}
	err := w.Close()
	require.NoError(t, err)
	return buf.String(), w.FormDataContentType()
}

func TestMultipart(t *testing.T) {
	s := newServer(t)
	target := "/runs/" + fixture.KnownID.String() + "/artifacts"
	upload := func(parts ...part) *httptest.ResponseRecorder {
		body, contentType := form(t, parts...)
		return s.do(http.MethodPost, target, body, echo.HeaderContentType, contentType)
	}
	manifest := part{name: "manifest", value: "steps: 3", filename: "manifest.yaml"}

	rec := upload(
		part{name: "category", value: "log"},
		part{name: "attempt", value: "2"},
		part{name: "tags", value: "a"},
		part{name: "tags", value: "b"},
		manifest,
		part{name: "files", value: "1", filename: "one.log"},
		part{name: "files", value: "2", filename: "two.log"},
	)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"manifest":"steps: 3","files":["one.log","two.log"]}`, rec.Body.String())
	got := s.app.Last().(*fixture.UploadRequest)
	require.Equal(t, fixture.KnownID, got.RunID)
	require.Equal(t, "log", got.Category)
	require.NotNil(t, got.Attempt)
	require.Equal(t, 2, *got.Attempt)
	require.Equal(t, []string{"a", "b"}, got.Tags)
	require.Equal(t, "manifest.yaml", got.Manifest.Filename)
	require.Len(t, got.Files, 2)

	// Optional values and files may be absent; empty values are absent.
	rec = upload(part{name: "category", value: "report"}, part{name: "attempt", value: ""}, manifest)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = s.app.Last().(*fixture.UploadRequest)
	require.Nil(t, got.Attempt)
	require.Nil(t, got.Files)

	// An integer is read as query and path parameters read it: leading
	// zeros and a plus sign are accepted.
	for value, want := range map[string]int{"007": 7, "+5": 5} {
		rec = upload(part{name: "category", value: "log"}, part{name: "attempt", value: value}, manifest)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got = s.app.Last().(*fixture.UploadRequest)
		require.NotNil(t, got.Attempt)
		require.Equal(t, want, *got.Attempt)
	}

	// Required values and files are reported together; case-mismatched keys
	// are ignored, never bound.
	rec = upload(part{name: "Category", value: "log"})
	resp := requireError(t, rec, http.StatusBadRequest, "category is required")
	require.Contains(t, resp.Error.Message, "manifest is required")

	rec = upload(part{name: "category", value: sentinel}, part{name: "attempt", value: "abc"}, manifest)
	resp = requireError(t, rec, http.StatusBadRequest, "attempt must be integer")
	require.Contains(t, resp.Error.Message, "category must be one of")

	rec = upload(part{name: "category", value: "log"}, part{name: "attempt", value: "0"}, manifest)
	requireError(t, rec, http.StatusBadRequest, "attempt must be >= 1")

	rec = upload(part{name: "category", value: "log"}, manifest, manifest)
	requireError(t, rec, http.StatusBadRequest, "manifest must be a single file")

	rec = upload(part{name: "category", value: "log"}, part{name: "manifest", value: strings.Repeat("a", 8192), filename: "big.yaml"})
	requireError(t, rec, http.StatusBadRequest, "body exceeds 4096 bytes")

	rec = s.do(http.MethodPost, target, `{"category":"log"}`)
	requireError(t, rec, http.StatusBadRequest, "content type must be multipart/form-data")

	rec = s.do(http.MethodPost, target, "--x\r\nbroken", echo.HeaderContentType, "multipart/form-data; boundary=x")
	requireError(t, rec, http.StatusBadRequest, "body is not a valid multipart form")

	body, contentType := form(t, part{name: "category", value: "log"}, manifest)
	rec = s.do(http.MethodPost, "/runs/not-a-uuid/artifacts", body, echo.HeaderContentType, contentType)
	requireError(t, rec, http.StatusBadRequest, "path id must be a valid uuid")
}

// optionalUploadRequest is a multipart request whose values and file are all
// optional.
type optionalUploadRequest struct {
	requests.NoRules
	Note *string               `json:"note"`
	File *multipart.FileHeader `json:"-" form:"file"`
}

// TestMultipartAllOptional: the document requires a multipart body even
// when every part is optional, as the server does.
func TestMultipartAllOptional(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST[optionalUploadRequest, nodeResponse](reg.Group("/notes"), "", "upload", stub)
	reg.Ready()
	s := &server{e: e, reg: reg}

	raw, err := reg.Spec(api.V31)
	require.NoError(t, err)
	require.Equal(t, true, dig(t, decode(t, raw), "paths", "/notes", "post", "requestBody", "required"))

	rec := s.do(http.MethodPost, "/notes", "")
	requireError(t, rec, http.StatusBadRequest, "content type must be multipart/form-data")

	body, contentType := form(t)
	rec = s.do(http.MethodPost, "/notes", body, echo.HeaderContentType, contentType)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// FormMeta is embedded in formEmbedRequest through a json tag with no name.
type FormMeta struct {
	Note    string `json:"note"`
	Attempt *int   `json:"attempt"`
}

type formEmbedRequest struct {
	requests.NoRules
	FormMeta `json:",omitempty"`
	File     *multipart.FileHeader `json:"-" form:"file"`
}

// TestMultipartEmbeddedNamelessTag: an embedded struct whose json tag has no
// name has its fields promoted, as encoding/json does, so each is a form
// value of its own.
func TestMultipartEmbeddedNamelessTag(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	var got *formEmbedRequest
	api.POST(reg.Group("/notes"), "", "upload", func(ctx context.Context, req *formEmbedRequest) (fixture.HealthResponse, error) {
		got = req
		return fixture.HealthResponse{}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	body, contentType := form(t, part{name: "note", value: "hello"}, part{name: "attempt", value: "3"})
	rec := s.do(http.MethodPost, "/notes", body, echo.HeaderContentType, contentType)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "hello", got.Note)
	require.NotNil(t, got.Attempt)
	require.Equal(t, 3, *got.Attempt)

	body, contentType = form(t, part{name: "attempt", value: "x"})
	rec = s.do(http.MethodPost, "/notes", body, echo.HeaderContentType, contentType)
	requireError(t, rec, http.StatusBadRequest, "attempt must be integer")
}

type tagsHeaderRequest struct {
	requests.NoRules
	Tags []string `json:"-" header:"x-tags" enum:"a,b"`
}

// TestHeaderList: an array header is a comma-separated list on one or more
// lines, with optional spaces around its items.
func TestHeaderList(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	var got *tagsHeaderRequest
	api.GET(reg.Group("/tags"), "", "list", func(ctx context.Context, req *tagsHeaderRequest) (fixture.HealthResponse, error) {
		got = req
		return fixture.HealthResponse{}, nil
	})
	reg.Ready()
	send := func(lines ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/tags", nil)
		for _, line := range lines {
			req.Header.Add("X-Tags", line)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec
	}

	for _, lines := range [][]string{{"a, b"}, {"a,b"}, {"a", "b"}, {" a ,\tb "}} {
		got = nil
		rec := send(lines...)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, []string{"a", "b"}, got.Tags, lines)
	}

	rec := send("a, c")
	requireError(t, rec, http.StatusBadRequest, "header x-tags.1 must be one of")
}

func TestNegotiationVary(t *testing.T) {
	s := newServer(t)

	// An operation with Produces varies by Accept, errors included.
	rec := s.do(http.MethodGet, "/runs/export?status=running", "", echo.HeaderAccept, "text/csv")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{echo.HeaderAccept}, rec.Header().Values(echo.HeaderVary))

	rec = s.do(http.MethodGet, "/runs/export?status=running", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{echo.HeaderAccept}, rec.Header().Values(echo.HeaderVary))

	rec = s.do(http.MethodGet, "/runs/export?status=bogus", "", echo.HeaderAccept, "text/csv")
	requireError(t, rec, http.StatusBadRequest, "query status must be one of")
	require.Equal(t, []string{echo.HeaderAccept}, rec.Header().Values(echo.HeaderVary))

	// Operations with one media type do not.
	rec = s.do(http.MethodGet, "/runs", "", echo.HeaderAccept, "text/csv")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, rec.Header().Values(echo.HeaderVary))

	rec = s.do(http.MethodGet, "/runs/"+fixture.KnownID.String()+"/report", "", echo.HeaderAccept, "application/pdf")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, rec.Header().Values(echo.HeaderVary))
}

// Node is a recursive request body.
type Node struct {
	Retries  int    `json:"retries" maximum:"3"`
	Children []Node `json:"children,omitempty"`
}

func (n *Node) Validate() error {
	return nil
}

// nodeResponse is the response of the recursive body operation.
type nodeResponse struct {
	OK bool `json:"ok"`
}

// nodeServer registers POST /nodes, taking a Node, and counts its calls.
func nodeServer() (*server, *int) {
	e := newEcho()
	calls := 0
	create := func(ctx context.Context, body *Node) (nodeResponse, error) {
		calls++
		return nodeResponse{OK: true}, nil
	}
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/nodes"), "", "create", create)
	reg.Ready()
	return &server{e: e, reg: reg}, &calls
}

// nested returns a Node body whose arrays and objects nest depth levels (an
// odd depth): depth/2 levels of {"children":[ around innermost.
func nested(depth int, innermost string) string {
	levels := depth / 2
	return strings.Repeat(`{"children":[`, levels) + innermost + strings.Repeat(`]}`, levels)
}

// A case-mismatched key is reported at the deepest nesting the body limit
// allows: its value is the 64th level.
func TestCollisionsAtMaxDepth(t *testing.T) {
	s, calls := nodeServer()

	rec := s.do(http.MethodPost, "/nodes", nested(63, `{"Children":[]}`))
	path := strings.Repeat("children.0.", 31) + "Children"
	requireError(t, rec, http.StatusBadRequest, path+` is not a known field, did you mean "children"`)
	require.Zero(t, *calls)
}

// freeForm is a request body with a field of any JSON value.
type freeForm struct {
	requests.NoRules
	Data any `json:"data"`
}

// A JSON body may nest 64 levels, on a recursive type and in a free-form
// field alike; at 65 it is refused before validation and the handler does
// not run.
func TestBodyDepthLimit(t *testing.T) {
	s, calls := nodeServer()

	rec := s.do(http.MethodPost, "/nodes", nested(63, `{"children":[]}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, *calls)

	// Past the limit the invalid value inside is never reached.
	rec = s.do(http.MethodPost, "/nodes", nested(63, `{"children":[{"retries":999}]}`))
	requireError(t, rec, http.StatusBadRequest, "body is nested deeper than 64 levels")
	require.Equal(t, 1, *calls)

	e := newEcho()
	freeCalls := 0
	create := func(ctx context.Context, body *freeForm) (nodeResponse, error) {
		freeCalls++
		return nodeResponse{OK: true}, nil
	}
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/free"), "", "create", create)
	reg.Ready()
	free := &server{e: e, reg: reg}

	// The body object is the first level, the arrays the rest.
	arrays := func(n int) string {
		return `{"data":` + strings.Repeat("[", n) + strings.Repeat("]", n) + `}`
	}
	rec = free.do(http.MethodPost, "/free", arrays(63))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, freeCalls)

	rec = free.do(http.MethodPost, "/free", arrays(64))
	requireError(t, rec, http.StatusBadRequest, "body is nested deeper than 64 levels")
	require.Equal(t, 1, freeCalls)
}

// doWithin runs the request and fails the test when it has not answered
// after d.
func doWithin(t *testing.T, d time.Duration, s *server, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- s.do(method, target, body)
	}()
	select {
	case rec := <-done:
		return rec
	case <-time.After(d):
		t.Fatalf("%s %.40s still running after %s", method, target, d)
		return nil
	}
}

// floatsRequest takes non-negative numbers in the query.
type floatsRequest struct {
	requests.NoRules
	V []float64 `json:"-" query:"v" minimum:"0"`
}

// The validator parses every number it checks against a bound or an
// integer type with big.Rat, at milliseconds each for an exponent like
// 1e-999999; it sees canonical text, so 2,000 of them answer at once.
func TestHugeExponents(t *testing.T) {
	s, calls := nodeServer()
	children := func(n string) string {
		items := strings.TrimSuffix(strings.Repeat(`{"retries":`+n+`},`, 2000), ",")
		return `{"children":[` + items + `]}`
	}

	// An underflow is 0 to the validator; encoding/json refuses it for an int.
	rec := doWithin(t, 5*time.Second, s, http.MethodPost, "/nodes", children("1e-999999"))
	requireError(t, rec, http.StatusBadRequest, "body could not be decoded")

	// A number no float64 holds is past every bound to the validator.
	rec = doWithin(t, 5*time.Second, s, http.MethodPost, "/nodes", children("1e999999"))
	requireError(t, rec, http.StatusBadRequest, "retries must be <= 3")
	require.Zero(t, *calls)

	e := newEcho()
	var got *floatsRequest
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/floats"), "", "list", func(ctx context.Context, req *floatsRequest) (nodeResponse, error) {
		got = req
		return nodeResponse{OK: true}, nil
	})
	reg.Ready()
	floats := &server{e: e, reg: reg}

	values := strings.TrimSuffix(strings.Repeat("1e-999999,", 2000), ",")
	rec = doWithin(t, 5*time.Second, floats, http.MethodGet, "/floats?v="+values, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, got.V, 2000)
	require.Zero(t, got.V[0])
}

// exactRequest holds integers a float64 cannot hold exactly.
type exactRequest struct {
	requests.NoRules
	I int64  `json:"i"`
	U uint64 `json:"u"`
}

// Canonical number text never routes an integer through float64.
func TestExactIntegers(t *testing.T) {
	e := newEcho()
	var got *exactRequest
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/exact"), "", "create", func(ctx context.Context, body *exactRequest) (nodeResponse, error) {
		got = body
		return nodeResponse{OK: true}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodPost, "/exact", `{"i":9007199254740993,"u":12345678901234567890}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, int64(9007199254740993), got.I)
	require.Equal(t, uint64(12345678901234567890), got.U)
}

// pageRequest embeds the pagination parameters through a pointer.
type pageRequest struct {
	*pagination.CursorRequest
	Name string `json:"-" query:"name"`
	Page *int   `json:"-" query:"page"`
}

// An embedded pointer carrying parameters is allocated even when none of
// them is sent, so the Validate promoted from it runs; a pointer parameter
// that is not sent stays nil.
func TestEmbeddedPointerParams(t *testing.T) {
	e := newEcho()
	var got *pageRequest
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/pages"), "", "list", func(ctx context.Context, req *pageRequest) (nodeResponse, error) {
		got = req
		return nodeResponse{OK: true}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodGet, "/pages?name=x", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, got.CursorRequest)
	// Zero but for the default limit Validate applied.
	require.Equal(t, pagination.CursorRequest{Limit: pagination.DEFAULT_LIMIT}, *got.CursorRequest)
	require.Equal(t, "x", got.Name)
	require.Nil(t, got.Page)

	rec = s.do(http.MethodGet, "/pages?limit=5&page=2", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 5, got.Limit)
	require.NotNil(t, got.Page)
	require.Equal(t, 2, *got.Page)
}

func TestMultipartRemovesSpilledFiles(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	spilled := func() []string {
		matches, err := filepath.Glob(filepath.Join(tmp, "multipart-*"))
		require.NoError(t, err)
		return matches
	}

	e := newEcho()
	// Middleware that replaces the request, as sentryecho does, so net/http
	// does not clean up the form parsed on the copy.
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), tenantKey{}, "t")))
			return next(c)
		}
	})
	var seen []string
	var size int64
	upload := func(ctx context.Context, body *fixture.UploadRequest) (nodeResponse, error) {
		seen = spilled()
		size = body.Manifest.Size
		return nodeResponse{OK: true}, nil
	}
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/runs"), "/:id/artifacts", "upload", upload, api.MaxBodyBytes(64<<20))
	reg.Ready()
	srv := httptest.NewServer(e)
	defer srv.Close()

	const fileSize = 33 << 20
	body, contentType := form(t, part{name: "category", value: "log"}, part{name: "manifest", value: strings.Repeat("a", fileSize), filename: "big.yaml"})
	res, err := http.Post(srv.URL+"/runs/"+fixture.KnownID.String()+"/artifacts", contentType, strings.NewReader(body))
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, res.Body)
	require.NoError(t, err)
	err = res.Body.Close()
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, int64(fileSize), size)
	require.NotEmpty(t, seen, "the file did not spill to disk")
	require.Empty(t, spilled())
}

// rawRequest carries free-form JSON the handler reads itself.
type rawRequest struct {
	requests.NoRules
	Data json.RawMessage `json:"data"`
}

// A number no float64 holds still reaches a json.RawMessage field as the
// client wrote it, and is refused where a Go number would have to hold it.
func TestOutOfRangeNumbers(t *testing.T) {
	e := newEcho()
	var got string
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/raw"), "", "create", func(ctx context.Context, req *rawRequest) (nodeResponse, error) {
		got = string(req.Data)
		return nodeResponse{OK: true}, nil
	})
	api.POST(reg.Group("/any"), "", "create", func(ctx context.Context, req *freeForm) (nodeResponse, error) {
		return nodeResponse{OK: true}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodPost, "/raw", `{"data":{"x":[1e999999,-1e400]}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, `{"x":[1e999999,-1e400]}`, got)

	rec = s.do(http.MethodPost, "/any", `{"data":1e999999}`)
	requireError(t, rec, http.StatusBadRequest, "body could not be decoded")
}

// amountRequest carries a json.Number, as a webhook amount does.
type amountRequest struct {
	requests.NoRules
	Amount json.Number `json:"amount" required:"true"`
}

type amountResponse struct {
	Amount json.Number `json:"amount"`
}

// TestJSONNumber: a json.Number is a JSON number both ways: the handler gets
// the client's digits exactly, the response passes the contract, and a
// string is refused.
func TestJSONNumber(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	var got json.Number
	api.POST(reg.Group("/amounts"), "", "create", func(ctx context.Context, req *amountRequest) (amountResponse, error) {
		got = req.Amount
		return amountResponse{Amount: req.Amount}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodPost, "/amounts", `{"amount":12345678901234567890.10}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, json.Number("12345678901234567890.10"), got)

	contract := apitest.New(t, reg)
	req := httptest.NewRequest(http.MethodPost, "/amounts", strings.NewReader(`{"amount":12.50}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec = contract.Do(t, e, "amounts.create", req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"amount":12.50}`, rec.Body.String())

	rec = s.do(http.MethodPost, "/amounts", `{"amount":"`+sentinel+`"}`)
	requireError(t, rec, http.StatusBadRequest, "amount must be number")
}

type addressRequest struct {
	requests.NoRules
	IP net.IP `json:"ip"`
}

// TestNetIP: a net.IP takes both address families; its decoder refuses a
// bad address.
func TestNetIP(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	var got net.IP
	api.POST(reg.Group("/addresses"), "", "create", func(ctx context.Context, req *addressRequest) (nodeResponse, error) {
		got = req.IP
		return nodeResponse{OK: true}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	for _, ip := range []string{"192.0.2.1", "2001:db8::1", "::1"} {
		rec := s.do(http.MethodPost, "/addresses", `{"ip":"`+ip+`"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.True(t, net.ParseIP(ip).Equal(got), ip)
	}

	rec := s.do(http.MethodPost, "/addresses", `{"ip":"`+sentinel+`"}`)
	requireError(t, rec, http.StatusBadRequest, "body could not be decoded")
}
