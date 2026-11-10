package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
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
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/compose/components/rest/requests"
	"github.com/vanclief/compose/drivers/databases/relational/postgres/pagination"
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

func TestCollisionsAtAnyDepth(t *testing.T) {
	e := newEcho()
	calls := 0
	create := func(ctx context.Context, body *Node) (nodeResponse, error) {
		calls++
		return nodeResponse{OK: true}, nil
	}
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/nodes"), "", "create", create)
	reg.Ready()
	s := &server{e: e, reg: reg}

	const levels = 40
	body := strings.Repeat(`{"children":[`, levels) + `{"Retries":999}` + strings.Repeat(`]}`, levels)
	rec := s.do(http.MethodPost, "/nodes", body)
	path := strings.Repeat("children.0.", levels) + "Retries"
	requireError(t, rec, http.StatusBadRequest, path+` is not a known field, did you mean "retries"`)
	require.Zero(t, calls)
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
