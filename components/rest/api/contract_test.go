package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/internal/fixture"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/compose/components/rest/requests"
)

// userKey is the context key the test authenticators store the user under.
type userKey struct{}

// tenantKey is the context key the test Echo middleware stores a value under.
type tenantKey struct{}

// whoRequest has no parameters and no body.
type whoRequest struct {
	requests.NoRules
}

// whoResponse reports what the handler found in its context.
type whoResponse struct {
	User      string `json:"user"`
	Tenant    string `json:"tenant"`
	RequestID string `json:"request_id"`
}

// who returns the values the authenticator and the middleware put in ctx.
func who(ctx context.Context, body *whoRequest) (whoResponse, error) {
	user, _ := ctx.Value(userKey{}).(string)
	tenant, _ := ctx.Value(tenantKey{}).(string)
	return whoResponse{User: user, Tenant: tenant, RequestID: requests.RequestID(ctx)}, nil
}

// authAs returns an authenticator that counts its calls and stores user in
// the request context.
func authAs(user string, calls *int) api.AuthenticateFunc {
	return func(request requests.Request) error {
		*calls++
		request.SetContext(context.WithValue(request.GetContext(), userKey{}, user))
		return nil
	}
}

// reject returns an authenticator that counts its calls and rejects every
// request.
func reject(calls *int) api.AuthenticateFunc {
	return func(request requests.Request) error {
		*calls++
		return ez.New(ez.ENOTAUTHENTICATED, "no", nil)
	}
}

// whoAmI sends a request and decodes the 200 whoResponse.
func whoAmI(t *testing.T, s *server, target string) whoResponse {
	t.Helper()
	rec := s.do(http.MethodGet, target, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp whoResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	return resp
}

func TestAuthenticateRuns(t *testing.T) {
	e := echo.New()
	calls := 0
	reg := api.New(e, api.Authenticate(authAs("alice", &calls)))
	api.GET(reg.Group("/me"), "", "get", who)
	reg.Ready()
	s := &server{e: e, reg: reg}

	resp := whoAmI(t, s, "/me")
	require.Equal(t, "alice", resp.User)
	require.Equal(t, 1, calls)
}

func TestAuthenticateRejects(t *testing.T) {
	e := echo.New()
	app := &fixture.App{}
	calls := 0
	reg := api.New(e, api.Authenticate(reject(&calls)))
	api.GET(reg.Group("/runs"), "/:id", "get", app.Get)
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodGet, "/runs/"+fixture.KnownID.String(), "")
	resp := requireError(t, rec, http.StatusUnauthorized, "no")
	require.Equal(t, ez.ENOTAUTHENTICATED, resp.Error.Code)
	require.Equal(t, 1, calls)
	require.Nil(t, app.Last(), "handler invoked despite the authenticator rejecting")
}

func TestPublicSkipsAuthenticate(t *testing.T) {
	e := echo.New()
	calls := 0
	reg := api.New(e, api.Authenticate(authAs("alice", &calls)))
	g := reg.Group("/me")
	api.GET(g, "", "get", who)
	api.GET(g, "/public", "public", who, api.Public())
	reg.Ready()
	s := &server{e: e, reg: reg}

	resp := whoAmI(t, s, "/me/public")
	require.Equal(t, "", resp.User)
	require.Equal(t, 0, calls)

	// The protected route of the same registry still authenticates.
	resp = whoAmI(t, s, "/me")
	require.Equal(t, "alice", resp.User)
	require.Equal(t, 1, calls)
}

func TestAuthenticateInheritance(t *testing.T) {
	e := echo.New()
	callsA := 0
	callsB := 0
	reg := api.New(e, api.Authenticate(authAs("A", &callsA)))
	open := reg.Group("/open", api.Public())
	api.GET(open, "/locked", "locked", who, api.Authenticate(authAs("B", &callsB)))
	api.GET(open, "/free", "free", who)
	api.GET(reg.Group("/closed"), "", "get", who)
	reg.Ready()
	s := &server{e: e, reg: reg}

	// The route's Authenticate restores protection under a public group.
	resp := whoAmI(t, s, "/open/locked")
	require.Equal(t, "B", resp.User)
	require.Equal(t, 0, callsA)
	require.Equal(t, 1, callsB)

	// Its sibling stays public.
	resp = whoAmI(t, s, "/open/free")
	require.Equal(t, "", resp.User)
	require.Equal(t, 0, callsA)
	require.Equal(t, 1, callsB)

	// Another group inherits the registry's authenticator.
	resp = whoAmI(t, s, "/closed")
	require.Equal(t, "A", resp.User)
	require.Equal(t, 1, callsA)
	require.Equal(t, 1, callsB)

	public := map[string]bool{}
	for _, op := range reg.Operations() {
		public[op.OperationID] = op.Public
	}
	require.Equal(t, map[string]bool{"open.locked": false, "open.free": true, "closed.get": false}, public)
}

func TestReadyRequiresAuthenticate(t *testing.T) {
	build := func(opts ...api.Option) *api.Registry {
		reg := api.New(echo.New())
		api.GET(reg.Group("/me"), "", "get", who, opts...)
		return reg
	}

	msg := panicMessage(t, func() {
		build().Ready()
	})
	require.Contains(t, msg, "operations without an authenticator")
	require.Contains(t, msg, "me.get")

	require.NotPanics(t, func() {
		build(api.Public()).Ready()
	})
}

func TestPublicAndAuthenticateConflict(t *testing.T) {
	calls := 0
	auth := authAs("alice", &calls)
	newGroup := func() *api.Group {
		return api.New(echo.New(), api.Authenticate(auth)).Group("/me")
	}

	tests := []struct {
		fn   func()
		want string
	}{
		{func() { api.GET(newGroup(), "", "get", who, api.Public(), api.Authenticate(auth)) }, "api: GET /me (me.get): api.Public and api.Authenticate cannot both be passed to an operation"},
		{func() { api.GET(newGroup(), "", "get", who, api.Authenticate(auth), api.Public()) }, "api.Public and api.Authenticate cannot both be passed to an operation"},
		{func() { api.New(echo.New(), api.Public(), api.Authenticate(auth)) }, "api: New: api.Public and api.Authenticate cannot both be passed to api.New"},
		{func() { api.New(echo.New(), api.Authenticate(auth), api.Public()) }, "api.Public and api.Authenticate cannot both be passed to api.New"},
	}
	for _, tt := range tests {
		msg := panicMessage(t, tt.fn)
		require.Contains(t, msg, tt.want)
	}
}

func TestValidateBeforeAuthenticate(t *testing.T) {
	e := echo.New()
	app := &fixture.App{}
	calls := 0
	reg := api.New(e, api.Authenticate(reject(&calls)))
	api.PATCH(reg.Group("/runs"), "/:id", "update", app.Update)
	reg.Ready()
	s := &server{e: e, reg: reg}
	target := "/runs/" + fixture.KnownID.String()

	rec := s.do(http.MethodPatch, target, `{"status":"failed"}`)
	requireError(t, rec, http.StatusBadRequest, "failure_reason is required when status is failed")
	require.Equal(t, 0, calls)

	// A valid body reaches the authenticator, which rejects it.
	rec = s.do(http.MethodPatch, target, `{"status":"failed","failure_reason":"x"}`)
	requireError(t, rec, http.StatusUnauthorized, "no")
	require.Equal(t, 1, calls)
	require.Nil(t, app.Last())
}

func TestSchemaBeforeValidate(t *testing.T) {
	s := newServer(t)
	target := "/runs/" + fixture.KnownID.String()

	// The body breaks the schema (max_total_runs) and the cross-field rule
	// (failed without failure_reason): only the schema is reported, so
	// Validate never ran.
	rec := s.do(http.MethodPatch, target, `{"max_total_runs":-1,"status":"failed"}`)
	resp := requireError(t, rec, http.StatusBadRequest, "max_total_runs must be >= 0")
	require.NotContains(t, resp.Error.Message, "failure_reason")
	require.Nil(t, s.app.Last())
}

func TestContractHandlerNotInvokedOnFailure(t *testing.T) {
	e := echo.New()
	app := &fixture.App{}
	calls := 0
	auth := func(request requests.Request) error {
		calls++
		if request.GetHeader().Get("X-Reject") != "" {
			return ez.New(ez.ENOTAUTHENTICATED, "rejected", nil)
		}
		return nil
	}
	reg := api.New(e, api.Authenticate(auth))
	api.PATCH(reg.Group("/runs"), "/:id", "update", app.Update)
	reg.Ready()
	s := &server{e: e, reg: reg}
	target := "/runs/" + fixture.KnownID.String()

	tests := []struct {
		name   string
		body   string
		header []string
		status int
		want   string
	}{
		{"schema", `{"max_total_runs":-1}`, nil, http.StatusBadRequest, "max_total_runs must be >= 0"},
		{"validate", `{"status":"failed"}`, nil, http.StatusBadRequest, "failure_reason is required"},
		{"authenticate", `{"status":"failed","failure_reason":"x"}`, []string{"X-Reject", "1"}, http.StatusUnauthorized, "rejected"},
	}
	for _, tt := range tests {
		rec := s.do(http.MethodPatch, target, tt.body, tt.header...)
		requireError(t, rec, tt.status, tt.want)
		require.Nil(t, app.Last(), tt.name)
	}
	require.Equal(t, 1, calls)

	rec := s.do(http.MethodPatch, target, `{"status":"failed","failure_reason":"x"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, app.Last())
}

// withTenant is Echo middleware that puts tenant on the request context.
func withTenant(tenant string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			ctx := context.WithValue(c.Request().Context(), tenantKey{}, tenant)
			c.SetRequest(c.Request().WithContext(ctx))
			return next(c)
		}
	}
}

func TestHandlerSeesInboundContext(t *testing.T) {
	e := echo.New()
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/route"), "", "get", who, api.Middleware(withTenant("route")))
	g := reg.Group("/group")
	g.Use(withTenant("group"))
	api.GET(g, "", "get", who)
	reg.Ready()
	s := &server{e: e, reg: reg}

	resp := whoAmI(t, s, "/route")
	require.Equal(t, "route", resp.Tenant)
	require.NotEmpty(t, resp.RequestID)

	resp = whoAmI(t, s, "/group")
	require.Equal(t, "group", resp.Tenant)
	require.NotEmpty(t, resp.RequestID)
}

func TestRequestCarriesAuthenticatorContextOnError(t *testing.T) {
	e := echo.New()
	calls := 0

	var echoContext context.Context
	var handled error
	var request requests.Request
	observe := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := next(c)
			echoContext = c.Request().Context()
			handled = handler.HandledError(c)
			request = api.Request(c)
			return err
		}
	}

	failing := func(ctx context.Context, body *fixture.CreateRequest) (fixture.Run, error) {
		return fixture.Run{}, ez.New(ez.ECONFLICT, "busy", nil)
	}
	reg := api.New(e, api.Authenticate(authAs("alice", &calls)))
	api.POST(reg.Group("/runs"), "", "create", failing, api.Timeout(time.Minute), api.Middleware(observe))
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodPost, "/runs", `{"name":"nightly"}`)
	resp := requireError(t, rec, http.StatusConflict, "busy")

	// The authenticator's values reach middleware through api.Request only:
	// the Echo request's context is not replaced.
	require.NotNil(t, request)
	after := request.GetContext()
	require.Equal(t, "alice", after.Value(userKey{}))
	require.Equal(t, resp.Error.RequestID, requests.RequestID(after))
	// The registry cancels the request context when the operation returns.
	require.ErrorIs(t, after.Err(), context.Canceled)

	require.NotNil(t, echoContext)
	require.Nil(t, echoContext.Value(userKey{}))
	require.Empty(t, requests.RequestID(echoContext))

	require.Error(t, handled)
	require.Equal(t, ez.ECONFLICT, ez.ErrorCode(handled))
	require.Equal(t, "busy", ez.ErrorMessage(handled))

	require.NotNil(t, request)
	require.Equal(t, resp.Error.RequestID, request.GetID())
	body, ok := request.GetBody().(*fixture.CreateRequest)
	require.True(t, ok, "body is %T", request.GetBody())
	require.Equal(t, "nightly", body.Name)
}

func TestErrorTranslator(t *testing.T) {
	e := echo.New()
	calls := 0

	var translatedID string
	translate := func(err error, request requests.Request) error {
		translatedID = request.GetID()
		return ez.New(ez.ENOTFOUND, "ocupado: "+ez.ErrorMessage(err), err)
	}

	var handled error
	observe := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := next(c)
			handled = handler.HandledError(c)
			return err
		}
	}

	failing := func(ctx context.Context, body *fixture.CreateRequest) (fixture.Run, error) {
		return fixture.Run{}, ez.New(ez.ECONFLICT, "busy", nil)
	}
	reg := api.New(e, api.ErrorTranslator(translate), api.Authenticate(authAs("alice", &calls)))
	api.POST(reg.Group("/runs"), "", "create", failing, api.Middleware(observe))
	reg.Ready()
	s := &server{e: e, reg: reg}

	// The translated message is written; the code and status stay the
	// original error's.
	rec := s.do(http.MethodPost, "/runs", `{"name":"nightly"}`)
	resp := requireError(t, rec, http.StatusConflict, "ocupado: busy")
	require.Equal(t, ez.ECONFLICT, resp.Error.Code)
	require.Equal(t, resp.Error.RequestID, translatedID)

	// handler.HandledError reports the original error.
	require.Error(t, handled)
	require.Equal(t, ez.ECONFLICT, ez.ErrorCode(handled))
	require.Equal(t, "busy", ez.ErrorMessage(handled))

	// Pipeline errors are translated too.
	rec = s.do(http.MethodPost, "/runs", `{"name":""}`)
	requireError(t, rec, http.StatusBadRequest, "ocupado: ")
}

func TestContextRequestNilOutsideRegistry(t *testing.T) {
	e := echo.New()
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/me"), "", "get", who)
	reg.Ready()

	observed := false
	var handled error
	var request requests.Request
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := next(c)
			observed = true
			handled = handler.HandledError(c)
			request = api.Request(c)
			return err
		}
	})
	e.GET("/plain", func(c *echo.Context) error {
		return c.String(http.StatusOK, "plain")
	})
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodGet, "/plain", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, observed)
	require.Nil(t, request)
	require.NoError(t, handled)

	// The registered route on the same Echo instance does set the request.
	whoAmI(t, s, "/me")
	require.NotNil(t, request)
}

func TestContextInboundCancellation(t *testing.T) {
	e := echo.New()
	started := make(chan struct{})
	var seen error
	wait := func(ctx context.Context, body *whoRequest) (whoResponse, error) {
		close(started)
		<-ctx.Done()
		seen = ctx.Err()
		return whoResponse{}, ez.Wrap(ctx.Err())
	}
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/wait"), "", "get", wait)
	reg.Ready()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/wait", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		e.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not unblock when the inbound context was cancelled")
	}
	require.ErrorIs(t, seen, context.Canceled)
	require.NotEqual(t, http.StatusOK, rec.Code)
}

func TestContextRouteTimeoutWins(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	var seen error
	wait := func(ctx context.Context, body *whoRequest) (whoResponse, error) {
		deadline, hasDeadline = ctx.Deadline()
		<-ctx.Done()
		seen = ctx.Err()
		return whoResponse{}, nil
	}

	e := echo.New()
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/short"), "", "get", wait, api.Timeout(50*time.Millisecond))
	api.GET(reg.Group("/long"), "", "get", wait, api.Timeout(time.Minute))
	reg.Ready()

	// The route timeout replaces the 15s default.
	start := time.Now()
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/short", nil))
	elapsed := time.Since(start)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, hasDeadline)
	require.GreaterOrEqual(t, deadline.Sub(start), 50*time.Millisecond)
	require.Less(t, deadline.Sub(start), time.Second)
	require.Less(t, elapsed, time.Second)
	require.ErrorIs(t, seen, context.DeadlineExceeded)

	// An inbound deadline shorter than the route timeout wins.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	inbound, _ := ctx.Deadline()
	start = time.Now()
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/long", nil).WithContext(ctx))
	elapsed = time.Since(start)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, hasDeadline)
	require.Equal(t, inbound, deadline)
	require.Less(t, elapsed, time.Second)
	require.ErrorIs(t, seen, context.DeadlineExceeded)
}

func TestProducesSelectsHandler(t *testing.T) {
	e := echo.New()
	app := &fixture.App{}
	jsonCalls := 0
	csvCalls := 0
	export := func(ctx context.Context, body *fixture.ExportRequest) (fixture.ListResponse, error) {
		jsonCalls++
		return app.Export(ctx, body)
	}
	exportCSV := func(ctx context.Context, body *fixture.ExportRequest) (api.Blob, error) {
		csvCalls++
		return app.ExportCSV(ctx, body)
	}
	reg := api.New(e, api.Authenticate(app.Authenticate))
	api.GET(reg.Group("/runs"), "/export", "export", export, api.Produces("text/csv", exportCSV))
	reg.Ready()
	s := &server{e: e, reg: reg}

	tests := []struct {
		accept   string
		want     string
		wantJSON int
		wantCSV  int
	}{
		{"text/csv", "text/csv", 0, 1},
		{echo.MIMEApplicationJSON, echo.MIMEApplicationJSON, 1, 0},
		{"", echo.MIMEApplicationJSON, 1, 0},
	}
	for _, tt := range tests {
		jsonCalls = 0
		csvCalls = 0
		rec := s.do(http.MethodGet, "/runs/export", "", echo.HeaderAccept, tt.accept)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, tt.wantJSON, jsonCalls, tt.accept)
		require.Equal(t, tt.wantCSV, csvCalls, tt.accept)
		require.Equal(t, tt.want, app.LastAccept(), tt.accept)
		require.Equal(t, tt.want, rec.Header().Get(echo.HeaderContentType), tt.accept)
	}
}

func TestContentTypePrimaryBlob(t *testing.T) {
	s := newServer(t)
	target := "/runs/" + fixture.KnownID.String() + "/report"

	for _, accept := range []string{"", "application/pdf", "text/csv"} {
		rec := s.do(http.MethodGet, target, "", echo.HeaderAccept, accept)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "application/pdf", rec.Header().Get(echo.HeaderContentType), accept)
		require.Equal(t, `attachment; filename="run report.pdf"`, rec.Header().Get(echo.HeaderContentDisposition), accept)
		require.Equal(t, "%PDF-1.7 "+fixture.KnownID.String(), rec.Body.String())
		require.Equal(t, "application/pdf", s.app.LastAccept(), accept)
	}
}

func TestRegistrationRejectsMedia(t *testing.T) {
	app := &fixture.App{}
	newRegistry := func() *api.Registry {
		return api.New(echo.New(), api.Authenticate(app.Authenticate))
	}
	newGroup := func() *api.Group {
		return newRegistry().Group("/runs")
	}
	exportJSON := func(opts ...api.Option) {
		api.GET[fixture.ExportRequest, fixture.ListResponse](newGroup(), "/export", "export", stub, opts...)
	}
	exportBlob := func(opts ...api.Option) {
		api.GET[fixture.ExportRequest, api.Blob](newGroup(), "/export", "export", stub, opts...)
	}

	tests := []struct {
		name string
		fn   func()
		want string
	}{
		{"produces json", func() { exportJSON(api.Produces(echo.MIMEApplicationJSON, app.ExportCSV)) }, "api: GET /runs/export (runs.export): Produces cannot declare application/json"},
		{"duplicate media type", func() { exportJSON(api.Produces("text/csv", app.ExportCSV), api.Produces("text/csv", app.ExportCSV)) }, `Produces lists "text/csv" twice`},
		{"producer request type", func() { exportJSON(api.Produces("application/pdf", app.Report)) }, `Produces "application/pdf" handler takes fixture.ReportRequest, but the operation's request type is fixture.ExportRequest`},
		{"content type on json", func() { exportJSON(api.ContentType("text/csv")) }, "api.ContentType needs a handler returning api.Blob, not fixture.ListResponse"},
		{"blob without content type", func() { exportBlob() }, "api.Blob responses need api.ContentType with their media type"},
		{"producer repeats primary", func() { exportBlob(api.ContentType("text/csv"), api.Produces("text/csv", app.ExportCSV)) }, `Produces "text/csv" repeats the primary media type`},
		{"interface response", func() {
			api.GET(newGroup(), "/export", "export", func(ctx context.Context, body *fixture.ExportRequest) (any, error) {
				return nil, nil
			})
		}, "response type interface {} is an interface: the handler must return a concrete type"},
		{"blob pointer response", func() {
			api.GET[fixture.ExportRequest, *api.Blob](newGroup(), "/export", "export", stub, api.ContentType("text/csv"))
		}, "the handler must return api.Blob, not *api.Blob"},
		{"nil handler", func() { api.GET[fixture.ExportRequest, fixture.ListResponse](newGroup(), "/export", "export", nil) }, "api: GET /runs/export (runs.export): nil handler"},
		{"nil authenticator", func() { api.Authenticate(nil) }, "api: Authenticate needs a function"},
		{"registry option on group", func() { newRegistry().Group("/runs", api.Info("x", "1")) }, "api: group /runs: api.Info is a registry option: pass it to api.New, not to a group"},
		{"registry option on route", func() { exportJSON(api.Info("x", "1")) }, "api.Info is a registry option: pass it to api.New, not to an operation"},
		{"route option on New", func() { api.New(echo.New(), api.ContentType("text/csv")) }, "api: New: api.ContentType is an operation option: pass it to the operation, not to api.New"},
	}
	for _, tt := range tests {
		msg := panicMessage(t, tt.fn)
		require.Contains(t, msg, tt.want, tt.name)
	}
}

func TestOperationsPublic(t *testing.T) {
	s := newServer(t)

	byID := map[string]api.OperationInfo{}
	for _, op := range s.reg.Operations() {
		byID[op.OperationID] = op
		require.Equal(t, op.OperationID == "health.check", op.Public, op.OperationID)
	}
	require.Equal(t, []string{echo.MIMEApplicationJSON, "text/csv"}, byID["runs.export"].Produces)
	require.Equal(t, []string{"application/pdf"}, byID["runs.report"].Produces)
	require.Equal(t, []string{echo.MIMEApplicationJSON}, byID["runs.get"].Produces)
}
