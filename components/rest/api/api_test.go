package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/internal/fixture"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/compose/components/rest/requests"
)

const sentinel = "ZZZ_SENTINEL"

var projectID = uuid.MustParse("0192f3a1-0000-7000-8000-000000000001")

type server struct {
	e   *echo.Echo
	reg *api.Registry
	app *fixture.App
}

func newServer(t *testing.T) *server {
	t.Helper()

	e := echo.New()
	app := &fixture.App{}
	reg := api.New(e, handler.NewHandler(app),
		api.Info("auditor3", "1.0.0"),
		api.Description("fixture API"),
		api.APIKeyHeader("API-KEY"),
		api.Server("https://api.example.com"),
	)

	rg := reg.Group("/runs", api.Timeout(60*time.Second))
	api.GET[fixture.ListRequest, fixture.ListResponse](rg, "", "list", api.Doc("List runs", "Cursor paginated."))
	api.GET[fixture.GetRequest, fixture.Run](rg, "/:id", "get")
	api.POST[fixture.CreateRequest, fixture.Run](rg, "", "create")
	api.PATCH[fixture.UpdateRequest, fixture.Run](rg, "/:id", "update")
	api.PUT[fixture.UpdateRequest, fixture.Run](rg, "/:id", "replace", api.Hidden())
	api.POST[fixture.ArchiveRequest, fixture.Run](rg, "/archive", "archive", api.Hidden(), api.MaxBodyBytes(1024))
	api.GET[fixture.ReportRequest, api.Blob](rg, "/:id/report", "report", api.Produces("application/pdf"))
	api.GET[fixture.ExportRequest, fixture.ListResponse](rg, "/export", "export", api.Produces(echo.MIMEApplicationJSON, "text/csv"))

	hg := reg.Group("/health")
	api.GET[fixture.HealthRequest, fixture.HealthResponse](hg, "", "check", api.Public(), api.Deprecated())

	err := reg.Ready()
	require.NoError(t, err)

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

	// Unknown (case-mismatched) query keys are ignored, never bound.
	rec = s.do(http.MethodGet, "/runs?LIMIT=5000&Status=bogus", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got = s.app.Last().(*fixture.ListRequest)
	require.Equal(t, 0, got.Limit)
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
	require.Equal(t, 0, got.Limit)

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

	// Shape passes, the app's cross-field Validate still runs.
	rec = s.do(http.MethodPatch, target, `{"status":"failed"}`)
	requireError(t, rec, http.StatusBadRequest, "failure_reason is required when status is failed")

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
	e := echo.New()
	reg := api.New(e, handler.NewHandler(&fixture.App{}))
	g := reg.Group("/health")
	api.GET[fixture.HealthRequest, fixture.HealthResponse](g, "", "check")

	s := &server{e: e, reg: reg}
	rec := s.do(http.MethodGet, "/health", "")
	resp := requireError(t, rec, http.StatusInternalServerError, "")
	require.Equal(t, ez.EINTERNAL, resp.Error.Code)

	err := reg.Ready()
	require.NoError(t, err)
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

	// Registry default, overridden per group.
	e := echo.New()
	reg := api.New(e, handler.NewHandler(&fixture.App{}), api.MaxBody(64))
	api.POST[fixture.ArchiveRequest, fixture.Run](reg.Group("/small"), "", "archive")
	api.POST[fixture.ArchiveRequest, fixture.Run](reg.Group("/big", api.MaxBodyBytes(1024)), "", "archive")
	err := reg.Ready()
	require.NoError(t, err)
	small := &server{e: e, reg: reg}

	body := `{"reason":"` + strings.Repeat("a", 100) + `"}`
	rec = small.do(http.MethodPost, "/small", body)
	requireError(t, rec, http.StatusBadRequest, "body exceeds 64 bytes")

	rec = small.do(http.MethodPost, "/big", body)
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

// stubApp returns a fixed response for every request.
type stubApp struct {
	response any
}

func (a stubApp) HandleRequest(request requests.Request) (any, error) {
	return a.response, nil
}

func TestBlobNotReturned(t *testing.T) {
	for _, response := range []any{fixture.Run{}, []byte("raw"), (*api.Blob)(nil)} {
		e := echo.New()
		reg := api.New(e, handler.NewHandler(stubApp{response: response}))
		api.GET[fixture.HealthRequest, api.Blob](reg.Group("/report"), "", "get", api.Produces("application/pdf"))
		err := reg.Ready()
		require.NoError(t, err)

		rec := (&server{e: e, reg: reg}).do(http.MethodGet, "/report", "")
		resp := requireError(t, rec, http.StatusInternalServerError, "")
		require.Equal(t, ez.EINTERNAL, resp.Error.Code)
	}
}
