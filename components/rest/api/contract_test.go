package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest"
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
	e := newEcho()
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
	e := newEcho()
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
	e := newEcho()
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
	e := newEcho()
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
		reg := api.New(newEcho())
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

// readCounter is a request body that counts the reads made of it.
type readCounter struct {
	r     io.Reader
	reads int
}

func (b *readCounter) Read(p []byte) (int, error) {
	b.reads++
	return b.r.Read(p)
}

func TestAuthenticateBeforeBody(t *testing.T) {
	e := newEcho()
	app := &fixture.App{}
	calls := 0
	var bodies []requests.Body
	auth := func(request requests.Request) error {
		bodies = append(bodies, request.GetBody())
		return reject(&calls)(request)
	}
	reg := api.New(e, api.Authenticate(auth))
	api.PATCH(reg.Group("/runs"), "/:id", "update", app.Update)
	reg.Ready()
	target := "/runs/" + fixture.KnownID.String()

	// A body that breaks the schema, and one that passes it but breaks the
	// cross-field rule of Validate: the authenticator rejects both before the
	// body is read, so neither the schema nor Validate reports anything.
	for _, body := range []string{`{"max_total_runs":-1}`, `{"status":"failed"}`} {
		counter := &readCounter{r: strings.NewReader(body)}
		req := httptest.NewRequest(http.MethodPatch, target, counter)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		resp := requireError(t, rec, http.StatusUnauthorized, "no")
		require.Equal(t, ez.ENOTAUTHENTICATED, resp.Error.Code, body)
		require.Zero(t, counter.reads, body)
	}
	require.Equal(t, 2, calls)
	require.Equal(t, []requests.Body{nil, nil}, bodies)
	require.Nil(t, app.Last(), "handler invoked despite the authenticator rejecting")
}

// The authenticator runs before a multipart body is parsed too.
func TestAuthenticateBeforeMultipartBody(t *testing.T) {
	e := newEcho()
	app := &fixture.App{}
	calls := 0
	reg := api.New(e, api.Authenticate(reject(&calls)))
	api.POST(reg.Group("/runs"), "/:id/artifacts", "upload", app.Upload)
	reg.Ready()
	target := "/runs/" + fixture.KnownID.String() + "/artifacts"

	// A body that is not a multipart form, and a form that breaks the
	// schema and misses the required file: the authenticator rejects both
	// before a byte of the body is read.
	valid, contentType := form(t, part{name: "category", value: sentinel})
	bodies := map[string]string{
		"--x\r\nbroken": "multipart/form-data; boundary=x",
		valid:           contentType,
	}
	for body, contentType := range bodies {
		counter := &readCounter{r: strings.NewReader(body)}
		req := httptest.NewRequest(http.MethodPost, target, counter)
		req.Header.Set(echo.HeaderContentType, contentType)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)

		resp := requireError(t, rec, http.StatusUnauthorized, "no")
		require.Equal(t, ez.ENOTAUTHENTICATED, resp.Error.Code, body)
		require.Zero(t, counter.reads, body)
		require.Nil(t, req.MultipartForm, body)
	}
	require.Equal(t, 2, calls)
	require.Nil(t, app.Last(), "handler invoked despite the authenticator rejecting")
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
	require.Equal(t, []handler.FieldError{{In: "body", Name: "max_total_runs", Message: "must be >= 0"}}, resp.Error.Fields)
	require.Nil(t, s.app.Last())

	// A Validate failure is the app's own error: it lists no fields.
	rec = s.do(http.MethodPatch, target, `{"status":"failed"}`)
	resp = requireError(t, rec, http.StatusBadRequest, "failure_reason is required")
	require.Nil(t, resp.Error.Fields)
	require.NotContains(t, rec.Body.String(), "fields")
}

func TestManyFailingFieldsAreCapped(t *testing.T) {
	s := newServer(t)

	// 150 array items break the schema: the answer lists the first 100 of
	// them, in index order, and says there are more, whatever the request
	// held. The validator stops at the cap, so it has no count of the rest.
	body := `{"name":"x","steps":[1` + strings.Repeat(",1", 149) + `]}`
	rec := s.do(http.MethodPost, "/runs", body)
	resp := requireError(t, rec, http.StatusBadRequest, "steps.0 must be object")
	require.Len(t, resp.Error.Fields, 100)
	for i, field := range resp.Error.Fields {
		require.Equal(t, handler.FieldError{In: "body", Name: "steps." + strconv.Itoa(i), Message: "must be object"}, field)
	}
	require.True(t, strings.HasSuffix(resp.Error.Message, "; steps.99 must be object; and more"), resp.Error.Message)
	require.Nil(t, s.app.Last())

	// Exactly 100 failing items are all listed, with nothing appended.
	body = `{"name":"x","steps":[1` + strings.Repeat(",1", 99) + `]}`
	rec = s.do(http.MethodPost, "/runs", body)
	resp = requireError(t, rec, http.StatusBadRequest, "steps.0 must be object")
	require.Len(t, resp.Error.Fields, 100)
	require.True(t, strings.HasSuffix(resp.Error.Message, "; steps.99 must be object"), resp.Error.Message)
}

type longKeyRequest struct {
	requests.NoRules
	Counts map[string]map[string]int `json:"counts"`
}

// A client-chosen key does not make the walk of the violations under it
// slower: 20,000 failing values under a 200 KB key cost about what they
// cost under a short one, where every comparison of two of them once
// walked the key. The route admits them with MaxBodyValues.
func TestLongKeyDoesNotSlowTheWalk(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/counts"), "", "create", func(ctx context.Context, body *longKeyRequest) (whoResponse, error) {
		return whoResponse{}, nil
	}, api.MaxBodyValues(30000))
	reg.Ready()
	s := &server{e: e, reg: reg}

	values := make([]string, 20000)
	for i := range values {
		values[i] = fmt.Sprintf(`"v%d":""`, i)
	}
	timed := func(key string) time.Duration {
		body := `{"counts":{"` + key + `":{` + strings.Join(values, ",") + `}}}`
		start := time.Now()
		rec := s.do(http.MethodPost, "/counts", body)
		elapsed := time.Since(start)
		resp := requireError(t, rec, http.StatusBadRequest, "counts.*.* must be integer")
		require.Equal(t, "counts.*.* must be integer", resp.Error.Message)
		return elapsed
	}
	short := timed("k")
	long := timed(strings.Repeat("k", 200*1024))
	require.Less(t, long, 5*short+200*time.Millisecond, "long key %v, short key %v", long, short)
}

// capItem declares zeta before alpha, both required.
type capItem struct {
	Zeta  string `json:"zeta" required:"true"`
	Alpha string `json:"alpha" required:"true"`
}

type capItems struct {
	requests.NoRules
	Items []capItem `json:"items"`
	One   string    `json:"one" required:"true"`
}

// TestCapKeepsTheFirstFieldsByName: past the cap the fields listed are the
// first 100 by name, as NewFieldsError orders them, wherever the validator
// reports them: a missing property of the body comes after the items before
// it in name, and an item's own missing names come in name order, not in
// the order the type declares them.
func TestCapKeepsTheFirstFieldsByName(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/items"), "", "create", func(ctx context.Context, body *capItems) (whoResponse, error) {
		return whoResponse{}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodPost, "/items", `{"items":[{}`+strings.Repeat(",{}", 49)+`]}`)
	resp := requireError(t, rec, http.StatusBadRequest, "items.0.alpha is required; items.0.zeta is required; items.1.alpha is required")
	require.Len(t, resp.Error.Fields, 100)
	require.True(t, strings.HasSuffix(resp.Error.Message, "; items.49.alpha is required; items.49.zeta is required; and more"), resp.Error.Message)
}

type capPairSub struct {
	X string `json:"x" required:"true"`
}

// capPair has a key "a" that "a-b" continues with a character that sorts
// before a dot.
type capPair struct {
	A  *capPairSub `json:"a"`
	AB string      `json:"a-b"`
	B  string      `json:"b"`
}

type capPairs struct {
	requests.NoRules
	Pairs []capPair        `json:"pairs"`
	M     map[string][]int `json:"m"`
}

// TestCapListsTheFieldsOfKeysInMessageOrder: past the cap the fields of
// map keys, all shown as "*", are the first 100 by name whichever key they
// come from, and a key's own fields come before a sibling "a-b" while the
// fields under it come after, as "a.x" sorts.
func TestCapListsTheFieldsOfKeysInMessageOrder(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/pairs"), "", "create", func(ctx context.Context, body *capPairs) (whoResponse, error) {
		return whoResponse{}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	items := func(item string) string {
		return `{"pairs":[` + item + strings.Repeat(","+item, 33) + `]}`
	}
	rec := s.do(http.MethodPost, "/pairs", items(`{"a":{},"a-b":1,"b":1}`))
	resp := requireError(t, rec, http.StatusBadRequest, "pairs.0.a-b must be string; pairs.0.a.x is required; pairs.0.b must be string")
	require.True(t, strings.HasSuffix(resp.Error.Message, "; pairs.32.b must be string; pairs.33.a-b must be string; and more"), resp.Error.Message)

	rec = s.do(http.MethodPost, "/pairs", items(`{"a":1,"a-b":1,"b":1}`))
	resp = requireError(t, rec, http.StatusBadRequest, "pairs.0.a must be object or null; pairs.0.a-b must be string; pairs.0.b must be string")
	require.True(t, strings.HasSuffix(resp.Error.Message, "; pairs.32.b must be string; pairs.33.a must be object or null; and more"), resp.Error.Message)

	// Key "a" fails at indices 60 to 119 and key "b" at 0 to 59: the
	// fields listed are m.*.0 to m.*.99.
	bad := func(from, to int) string {
		parts := make([]string, to)
		for i := range parts {
			parts[i] = "1"
			if i >= from {
				parts[i] = `"x"`
			}
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	for range 10 {
		rec = s.do(http.MethodPost, "/pairs", `{"m":{"a":`+bad(60, 120)+`,"b":`+bad(0, 60)+`}}`)
		resp = requireError(t, rec, http.StatusBadRequest, "m.*.0 must be integer; m.*.1 must be integer; m.*.2 must be integer")
		require.Len(t, resp.Error.Fields, 100)
		require.True(t, strings.HasSuffix(resp.Error.Message, "; m.*.98 must be integer; m.*.99 must be integer; and more"), resp.Error.Message)
	}
}

type capParams struct {
	requests.NoRules
	Q     int       `json:"-" query:"q" maximum:"5"`
	H     int       `json:"-" header:"X-H" minimum:"1"`
	Items []capItem `json:"items"`
}

// TestCapKeepsTheParametersFirst: the fields of the query and the headers
// come before those of the body, also when the body alone fails on more
// than the cap.
func TestCapKeepsTheParametersFirst(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/params"), "", "create", func(ctx context.Context, body *capParams) (whoResponse, error) {
		return whoResponse{}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	want := []string{"query q must be <= 5", "header x-h must be >= 1"}
	for i := range 49 {
		want = append(want, fmt.Sprintf("items.%d.alpha is required", i), fmt.Sprintf("items.%d.zeta is required", i))
	}
	rec := s.do(http.MethodPost, "/params?q=9", `{"items":[{}`+strings.Repeat(",{}", 59)+`]}`, "X-H", "0")
	resp := requireError(t, rec, http.StatusBadRequest, "query q must be <= 5")
	require.Equal(t, strings.Join(want, "; ")+"; and more", resp.Error.Message)
	require.Len(t, resp.Error.Fields, 100)
}

type capDuplicates struct {
	requests.NoRules
	A string         `json:"a"`
	M map[string]int `json:"m"`
	Z string         `json:"z"`
}

// TestCapCountsDistinctFields: 300 map values failing alike are one field,
// m.*, so the fields after it are listed and the message has no "and
// more".
func TestCapCountsDistinctFields(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/dups"), "", "create", func(ctx context.Context, body *capDuplicates) (whoResponse, error) {
		return whoResponse{}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	values := make([]string, 300)
	for i := range values {
		values[i] = fmt.Sprintf(`"k%d":"x"`, i)
	}
	rec := s.do(http.MethodPost, "/dups", `{"a":1,"m":{`+strings.Join(values, ",")+`},"z":1}`)
	resp := requireError(t, rec, http.StatusBadRequest, "m.* must be integer")
	require.Equal(t, "a must be string; m.* must be integer; z must be string", resp.Error.Message)
	require.Len(t, resp.Error.Fields, 3)
}

// capRow has eight required fields, so each {} item of capRows fails on
// eight of them.
type capRow struct {
	A string `json:"a" required:"true"`
	B string `json:"b" required:"true"`
	C string `json:"c" required:"true"`
	D string `json:"d" required:"true"`
	E string `json:"e" required:"true"`
	F string `json:"f" required:"true"`
	G string `json:"g" required:"true"`
	H string `json:"h" required:"true"`
}

type capRows struct {
	requests.NoRules
	Rows []capRow `json:"rows"`
}

func TestFailingBodyCostStopsAtTheCap(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/cap"), "", "rows", func(ctx context.Context, body *capRows) (whoResponse, error) {
		return whoResponse{}, nil
	}, api.MaxBodyValues(1<<20))
	reg.Ready()
	s := &server{e: e, reg: reg}

	// A body just under the default byte limit of {} items, which the route
	// admits with MaxBodyValues, fails on about 2.6 million fields. Mapping
	// every one of them to a field, before the cap kept 100, allocated about
	// 2.5 GB; the whole request now allocates about 0.5 GB, most of it the
	// validator's own errors.
	var b strings.Builder
	b.WriteString(`{"rows":[{}`)
	for b.Len() < 975*1024 {
		b.WriteString(",{}")
	}
	b.WriteString("]}")
	body := b.String()

	var rec *httptest.ResponseRecorder
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	rec = s.do(http.MethodPost, "/cap", body)
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	require.Less(t, allocated, uint64(1<<30), "validating a %d-byte body allocated %d bytes", len(body), allocated)
	resp := requireError(t, rec, http.StatusBadRequest, "rows.0.a is required; rows.0.b is required")
	require.Len(t, resp.Error.Fields, 100)
	require.True(t, strings.HasSuffix(resp.Error.Message, "; rows.12.d is required; and more"), resp.Error.Message)
}

type valuesRequest struct {
	requests.NoRules
	Items []any `json:"items"`
}

type valuesResponse struct {
	Items int `json:"items"`
}

func countItems(ctx context.Context, body *valuesRequest) (valuesResponse, error) {
	return valuesResponse{Items: len(body.Items)}, nil
}

// valuePieces are array items and how many values each holds: an object's
// keys hold none, and the brackets, commas and quotes in a string are text.
var valuePieces = []struct {
	text   string
	values int
}{
	{`1`, 1},
	{`-2.5e3`, 1},
	{`true`, 1},
	{`null`, 1},
	{`"a{b[c,d\"e\\"`, 1},
	{`"\\"`, 1},
	{`[]`, 1},
	{`{ }`, 1},
	{`[ 1 , [ ] ]`, 3},
	{`{"k,{[\"":"v]}","n":{"m":[null]}}`, 5},
	{"\n[\t{}\r]", 2},
}

// valuesBody returns a valuesRequest body that holds exactly n values, n at
// least 2: the body object, its items array and valuePieces in turn, then
// 1s.
func valuesBody(n int) string {
	var items []string
	count := 2
	for i := 0; count+valuePieces[i%len(valuePieces)].values <= n; i++ {
		piece := valuePieces[i%len(valuePieces)]
		items = append(items, piece.text)
		count += piece.values
	}
	for count < n {
		items = append(items, "1")
		count++
	}
	return `{"items":[` + strings.Join(items, " ,") + "]}"
}

// jsonValues decodes body and counts its values: every scalar, array and
// object one, an object's keys none.
func jsonValues(t *testing.T, body string) int {
	t.Helper()
	var v any
	err := json.Unmarshal([]byte(body), &v)
	require.NoError(t, err)
	var count func(v any) int
	count = func(v any) int {
		n := 1
		switch v := v.(type) {
		case []any:
			for _, item := range v {
				n += count(item)
			}
		case map[string]any:
			for _, item := range v {
				n += count(item)
			}
		}
		return n
	}
	return count(v)
}

// TestMaxBodyValues: a JSON body holds at most DefaultMaxBodyValues values,
// every scalar, array and object counting one, an object's keys none, and
// the brackets, commas and quotes in a string nothing. One more is a 400
// before the body is decoded, so the handler is not called.
func TestMaxBodyValues(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	calls := 0
	api.POST(reg.Group("/values"), "", "count", func(ctx context.Context, body *valuesRequest) (valuesResponse, error) {
		calls++
		return countItems(ctx, body)
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	body := valuesBody(api.DefaultMaxBodyValues)
	require.Equal(t, api.DefaultMaxBodyValues, jsonValues(t, body))
	rec := s.do(http.MethodPost, "/values", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 1, calls)

	body = valuesBody(api.DefaultMaxBodyValues + 1)
	require.Equal(t, api.DefaultMaxBodyValues+1, jsonValues(t, body))
	rec = s.do(http.MethodPost, "/values", body)
	resp := requireError(t, rec, http.StatusBadRequest, "body holds more than 10000 values")
	require.Equal(t, "body holds more than 10000 values", resp.Error.Message)
	require.Empty(t, resp.Error.Fields)
	require.Equal(t, 1, calls)

	// A malformed body within the limit keeps its error; past it, the count
	// refuses it first. Commas inside an unterminated string are text, and
	// the decoder refuses it.
	tests := []struct {
		body string
		want string
	}{
		{`{"items":[1,2}`, "body is not valid JSON"},
		{`{"items":[` + strings.Repeat("1,", 20000), "body holds more than 10000 values"},
		{`{"items":[,,` + strings.Repeat(",", 20000) + `]}`, "body holds more than 10000 values"},
		{`{"items":["` + strings.Repeat(",", 20000), "body is not valid JSON"},
	}
	for _, tt := range tests {
		rec = s.do(http.MethodPost, "/values", tt.body)
		resp = requireError(t, rec, http.StatusBadRequest, tt.want)
		require.Equal(t, tt.want, resp.Error.Message)
	}
	require.Equal(t, 1, calls)
}

// TestMaxBodyValuesOption: MaxBodyValues on New is the default of every
// operation, which a group's or the operation's own overrides, raising or
// lowering it.
func TestMaxBodyValuesOption(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public(), api.MaxBodyValues(20000))
	api.POST(reg.Group("/new"), "", "count", countItems)
	api.POST(reg.Group("/group", api.MaxBodyValues(4)), "", "count", countItems)
	api.POST(reg.Group("/route"), "", "count", countItems, api.MaxBodyValues(3))
	reg.Ready()
	raised := &server{e: e, reg: reg}

	e = newEcho()
	reg = api.New(e, api.Public(), api.MaxBodyValues(5))
	api.POST(reg.Group("/new"), "", "count", countItems)
	api.POST(reg.Group("/group", api.MaxBodyValues(20000)), "", "count", countItems)
	api.POST(reg.Group("/route", api.MaxBodyValues(4)), "", "count", countItems, api.MaxBodyValues(20000))
	reg.Ready()
	lowered := &server{e: e, reg: reg}

	tests := []struct {
		s      *server
		target string
		limit  int
	}{
		{raised, "/new", 20000},
		{raised, "/group", 4},
		{raised, "/route", 3},
		{lowered, "/new", 5},
		{lowered, "/group", 20000},
		{lowered, "/route", 20000},
	}
	for _, tt := range tests {
		rec := tt.s.do(http.MethodPost, tt.target, valuesBody(tt.limit))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tt.target, rec.Body.String())

		rec = tt.s.do(http.MethodPost, tt.target, valuesBody(tt.limit+1))
		want := fmt.Sprintf("body holds more than %d values", tt.limit)
		resp := requireError(t, rec, http.StatusBadRequest, want)
		require.Equal(t, want, resp.Error.Message, tt.target)
	}
}

// allocated returns the bytes the heap allocated while fn ran.
func allocated(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestManyValuesRefusedBeforeDecoding: a 1 MiB body of half a million
// failing values allocated about 0.8 GB, most of it the validator's errors;
// counting its values refuses it for little more than reading it costs.
func TestManyValuesRefusedBeforeDecoding(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/cap"), "", "rows", func(ctx context.Context, body *capRows) (whoResponse, error) {
		return whoResponse{}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	var b strings.Builder
	b.WriteString(`{"rows":[0`)
	for b.Len() < 1<<20-2 {
		b.WriteString(",0")
	}
	b.WriteString("]}")
	body := b.String()

	var rec *httptest.ResponseRecorder
	n := allocated(func() {
		rec = s.do(http.MethodPost, "/cap", body)
	})
	require.Less(t, n, uint64(16<<20), "refusing a %d-byte body allocated %d bytes", len(body), n)
	resp := requireError(t, rec, http.StatusBadRequest, "body holds more than 10000 values")
	require.Equal(t, "body holds more than 10000 values", resp.Error.Message)
}

type deepNode struct {
	Kids []deepNode `json:"kids"`
}

type deepRequest struct {
	requests.NoRules
	Root deepNode `json:"root"`
}

// TestWorstAdmittedBodyCost: within the default limit, a body that fails
// on every value at the deepest path the depth limit admits is the
// costliest for a schema of plain structs, as every error holds its path.
// It allocates about 40 MB; a path through nullable structs costs about
// twice that.
func TestWorstAdmittedBodyCost(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/deep"), "", "create", func(ctx context.Context, body *deepRequest) (whoResponse, error) {
		return whoResponse{}, nil
	})
	reg.Ready()
	s := &server{e: e, reg: reg}

	// The body object, 31 nodes and their kids arrays hold 63 values, and
	// the deepest array the rest, 9,937 strings that must be objects.
	levels := 31
	items := api.DefaultMaxBodyValues - 1 - 2*levels
	body := `{"root":` + strings.Repeat(`{"kids":[`, levels) +
		`"x"` + strings.Repeat(`,"x"`, items-1) +
		strings.Repeat(`]}`, levels) + `}`
	require.Equal(t, api.DefaultMaxBodyValues, jsonValues(t, body))

	var rec *httptest.ResponseRecorder
	n := allocated(func() {
		rec = s.do(http.MethodPost, "/deep", body)
	})
	require.Less(t, n, uint64(128<<20), "validating a %d-byte body allocated %d bytes", len(body), n)
	path := "root" + strings.Repeat(".kids.0", levels-1) + ".kids."
	resp := requireError(t, rec, http.StatusBadRequest, path+"0 must be object")
	require.Len(t, resp.Error.Fields, 100)
}

type paramItemsRequest struct {
	requests.NoRules
	Path  []int    `json:"-" path:"path"`
	IDs   []int    `json:"-" query:"ids"`
	Tags  []string `json:"-" query:"tags"`
	Codes []string `json:"-" header:"X-Codes"`
	One   string   `json:"-" query:"one"`
}

// paramItemsServer serves GET /items/:path, which answers how many items
// its array parameters received, behind an authenticator that rejects a
// request with X-Reject.
func paramItemsServer(t *testing.T) *server {
	t.Helper()
	e := newEcho()
	auth := func(request requests.Request) error {
		if request.GetHeader().Get("X-Reject") != "" {
			return ez.New(ez.ENOTAUTHENTICATED, "rejected", nil)
		}
		return nil
	}
	reg := api.New(e, api.Authenticate(auth))
	api.GET(reg.Group("/items"), "/:path", "list", func(ctx context.Context, body *paramItemsRequest) (valuesResponse, error) {
		return valuesResponse{Items: len(body.Path) + len(body.IDs) + len(body.Tags) + len(body.Codes)}, nil
	})
	reg.Ready()
	return &server{e: e, reg: reg}
}

// get sends a GET to target with the header lines given as name, value
// pairs, a name repeated adding a line.
func (s *server) get(target string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	s.e.ServeHTTP(rec, req)
	return rec
}

// ones returns n comma-separated 1s.
func ones(n int) string {
	return "1" + strings.Repeat(",1", n-1)
}

// TestMaxParameterValues: the array parameters of a request hold at most
// MaxParameterValues items together, in its path, query and headers, a
// value counting the items its commas separate, a repeated query value one
// item, and a header the non-empty items of all its lines. One more is a
// 400 before any is coerced; an unauthenticated request is refused first.
func TestMaxParameterValues(t *testing.T) {
	s := paramItemsServer(t)
	limit := api.MaxParameterValues
	want := fmt.Sprintf("parameters hold more than %d values", limit)

	tests := []struct {
		name   string
		target string
		header []string
		items  int
	}{
		{"path", "/items/" + ones(limit), nil, limit},
		{"query", "/items/1?ids=" + ones(limit-1), nil, limit},
		{"header", "/items/1", []string{"X-Codes", ones(limit - 1)}, limit},
		// A repeated query value is one item, commas and all; a header's
		// lines are split and their empty items dropped.
		{"mixed", "/items/1,1?tags=a,b&tags=c&one=x,y&ids=" + ones(limit-2-2-4), []string{"X-Codes", " a, b,, ,c ", "X-Codes", "d"}, limit},
	}
	for _, tt := range tests {
		rec := s.get(tt.target, tt.header...)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tt.name, rec.Body.String())
		var resp valuesResponse
		err := json.Unmarshal(rec.Body.Bytes(), &resp)
		require.NoError(t, err)
		require.Equal(t, tt.items, resp.Items, tt.name)

		// One item more.
		header := tt.header
		target := tt.target
		if len(header) > 0 {
			header = append(slices.Clone(header), "X-Codes", "e")
		} else {
			target = strings.Replace(target, "/items/", "/items/1,", 1)
		}
		rec = s.get(target, header...)
		errResp := requireError(t, rec, http.StatusBadRequest, want)
		require.Equal(t, want, errResp.Error.Message, tt.name)

		// Authentication comes first.
		rec = s.get(target, append(header, "X-Reject", "1")...)
		requireError(t, rec, http.StatusUnauthorized, "rejected")
	}
}

// TestManyParameterItemsRefusedBeforeCoercion: 1 MB of failing items in a
// query or a header allocated about 0.5 GB, most of it the validator's
// errors; counting them refuses the request for little more than parsing
// it costs.
func TestManyParameterItemsRefusedBeforeCoercion(t *testing.T) {
	s := paramItemsServer(t)
	items := "a" + strings.Repeat(",a", 500000)
	want := fmt.Sprintf("parameters hold more than %d values", api.MaxParameterValues)

	for _, header := range [][]string{nil, {"X-Codes", items}} {
		target := "/items/1?ids=" + items
		if header != nil {
			target = "/items/1"
		}
		var rec *httptest.ResponseRecorder
		n := allocated(func() {
			rec = s.get(target, header...)
		})
		require.Less(t, n, uint64(16<<20), "refusing %d bytes of items allocated %d bytes", len(items), n)
		resp := requireError(t, rec, http.StatusBadRequest, want)
		require.Equal(t, want, resp.Error.Message)
	}
}

// TestWorstAdmittedParameterCost: 10,000 failing query items, the most a
// request may hold, allocate about 10 MB.
func TestWorstAdmittedParameterCost(t *testing.T) {
	s := paramItemsServer(t)
	target := "/items/1?ids=a" + strings.Repeat(",a", api.MaxParameterValues-2)

	var rec *httptest.ResponseRecorder
	n := allocated(func() {
		rec = s.get(target)
	})
	require.Less(t, n, uint64(64<<20), "validating %d bytes of items allocated %d bytes", len(target), n)
	resp := requireError(t, rec, http.StatusBadRequest, "query ids.0 must be integer")
	require.Len(t, resp.Error.Fields, 100)
}

type formItemsRequest struct {
	requests.NoRules
	Name string                `json:"name"`
	IDs  []int                 `json:"ids"`
	Tags []string              `json:"tags"`
	File *multipart.FileHeader `json:"-" form:"file"`
}

// TestMaxBodyValuesForm: a multipart form is the body, and its values count
// toward the operation's MaxBodyValues as a JSON body's do: the body
// object, every field sent and the items of an array field, the items its
// commas separate in one value or one per repeated value. Files and empty
// values do not count.
func TestMaxBodyValuesForm(t *testing.T) {
	e := newEcho()
	reg := api.New(e, api.Public())
	upload := func(ctx context.Context, body *formItemsRequest) (valuesResponse, error) {
		return valuesResponse{Items: len(body.IDs) + len(body.Tags)}, nil
	}
	api.POST(reg.Group("/form"), "", "upload", upload)
	api.POST(reg.Group("/small"), "", "upload", upload, api.MaxBodyValues(5))
	reg.Ready()
	s := &server{e: e, reg: reg}
	post := func(target string, parts ...part) *httptest.ResponseRecorder {
		body, contentType := form(t, parts...)
		return s.do(http.MethodPost, target, body, echo.HeaderContentType, contentType)
	}
	limit := api.DefaultMaxBodyValues

	tests := []struct {
		name   string
		target string
		limit  int
		parts  func(ids int) []part
	}{
		// The body, name, ids and its items.
		{"one value", "/form", limit, func(ids int) []part {
			return []part{{name: "name", value: "x"}, {name: "ids", value: ones(ids - 3)}, {name: "file", value: "f", filename: "f.txt"}}
		}},
		// The body, tags and its two values, ids and its items.
		{"repeated", "/form", limit, func(ids int) []part {
			return []part{{name: "tags", value: "a,b"}, {name: "tags", value: "c"}, {name: "tags", value: ""}, {name: "ids", value: ones(ids - 5)}}
		}},
		{"option", "/small", 5, func(ids int) []part {
			return []part{{name: "name", value: "x"}, {name: "ids", value: ones(ids - 3)}}
		}},
	}
	for _, tt := range tests {
		rec := post(tt.target, tt.parts(tt.limit)...)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", tt.name, rec.Body.String())

		rec = post(tt.target, tt.parts(tt.limit+1)...)
		want := fmt.Sprintf("body holds more than %d values", tt.limit)
		resp := requireError(t, rec, http.StatusBadRequest, want)
		require.Equal(t, want, resp.Error.Message, tt.name)
	}

	// 1 MB of failing items allocated about 0.5 GB; counting them refuses
	// the form for little more than parsing it costs.
	body, contentType := form(t, part{name: "ids", value: "a" + strings.Repeat(",a", 500000)})
	var rec *httptest.ResponseRecorder
	n := allocated(func() {
		rec = s.do(http.MethodPost, "/form", body, echo.HeaderContentType, contentType)
	})
	require.Less(t, n, uint64(16<<20), "refusing a %d-byte form allocated %d bytes", len(body), n)
	want := fmt.Sprintf("body holds more than %d values", limit)
	resp := requireError(t, rec, http.StatusBadRequest, want)
	require.Equal(t, want, resp.Error.Message)
}

func TestContractHandlerNotInvokedOnFailure(t *testing.T) {
	e := newEcho()
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
	// The authenticator runs first, so every request reached it.
	require.Equal(t, 3, calls)

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
	e := newEcho()
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
	e := newEcho()
	calls := 0

	var echoContext context.Context
	var handled error
	var request requests.Request
	observe := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := next(c)
			echoContext = c.Request().Context()
			handled = err
			request = handler.Request(c)
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

	// The authenticator's values reach middleware through handler.Request
	// only:
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
	e := newEcho()
	calls := 0

	// Ready's check of the error handler reaches the translator with no
	// request, as an unknown route does.
	var translatedID string
	translate := func(c *echo.Context, err error) error {
		request := handler.Request(c)
		if request != nil {
			translatedID = request.GetID()
		}
		return ez.New(ez.ENOTFOUND, "ocupado: "+ez.ErrorMessage(err), err)
	}
	e.HTTPErrorHandler = rest.ErrorHandler(rest.WithErrorTranslator(translate))

	var handled error
	observe := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := next(c)
			handled = err
			return err
		}
	}

	failing := func(ctx context.Context, body *fixture.CreateRequest) (fixture.Run, error) {
		return fixture.Run{}, ez.New(ez.ECONFLICT, "busy", nil)
	}
	reg := api.New(e, api.Authenticate(authAs("alice", &calls)))
	api.POST(reg.Group("/runs"), "", "create", failing, api.Middleware(observe))
	reg.Ready()
	s := &server{e: e, reg: reg}

	// The translated message is written; the code and status stay the
	// original error's.
	rec := s.do(http.MethodPost, "/runs", `{"name":"nightly"}`)
	resp := requireError(t, rec, http.StatusConflict, "ocupado: busy")
	require.Equal(t, ez.ECONFLICT, resp.Error.Code)
	require.Equal(t, resp.Error.RequestID, translatedID)

	// Middleware receives the original error.
	require.Error(t, handled)
	require.Equal(t, ez.ECONFLICT, ez.ErrorCode(handled))
	require.Equal(t, "busy", ez.ErrorMessage(handled))

	// Pipeline errors are translated too.
	rec = s.do(http.MethodPost, "/runs", `{"name":""}`)
	requireError(t, rec, http.StatusBadRequest, "ocupado: ")
}

// Every registry error, a server error of the handler and a client error of
// the pipeline, reaches the app's reporter once and is answered in the
// envelope with the request's id.
func TestErrorReporter(t *testing.T) {
	e := newEcho()
	var reported []error
	report := func(c *echo.Context, err error) {
		reported = append(reported, err)
	}
	e.HTTPErrorHandler = rest.ErrorHandler(rest.WithErrorReporter(report))

	failure := ez.New(ez.EINTERNAL, "database down", nil)
	failing := func(ctx context.Context, body *fixture.CreateRequest) (fixture.Run, error) {
		return fixture.Run{}, failure
	}
	reg := api.New(e, api.Public())
	api.POST(reg.Group("/runs"), "", "create", failing)
	reg.Ready()
	s := &server{e: e, reg: reg}

	// Ready's check of the error handler was the first error reported.
	require.Len(t, reported, 1)
	reported = nil

	rec := s.do(http.MethodPost, "/runs", `{"name":"nightly"}`)
	resp := requireError(t, rec, http.StatusInternalServerError, "database down")
	require.Equal(t, ez.EINTERNAL, resp.Error.Code)
	require.Equal(t, rec.Header().Get(echo.HeaderXRequestID), resp.Error.RequestID)
	require.Len(t, reported, 1)
	require.Same(t, failure, reported[0])

	rec = s.do(http.MethodPost, "/runs", `{"name":""}`)
	resp = requireError(t, rec, http.StatusBadRequest, "name")
	require.Equal(t, rec.Header().Get(echo.HeaderXRequestID), resp.Error.RequestID)
	require.Len(t, reported, 2)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(reported[1]))
}

// errTaken is the Echo error takenRequest's Validate returns.
var errTaken = echo.NewHTTPError(http.StatusConflict, "taken")

// takenRequest fails its own Validate with an Echo error.
type takenRequest struct{}

func (r *takenRequest) Validate() error {
	return errTaken
}

// The authenticator's and Validate's errors reach Echo as returned, as the
// handler's do (TestErrorReporter): an Echo error keeps its status, and
// middleware receives the very same value.
func TestAppEchoErrorsReachEcho(t *testing.T) {
	var handled error
	observe := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := next(c)
			handled = err
			return err
		}
	}

	e := newEcho()
	unauthorized := func(request requests.Request) error {
		return echo.ErrUnauthorized
	}
	reg := api.New(e, api.Authenticate(unauthorized))
	api.GET(reg.Group("/me"), "", "get", who, api.Middleware(observe))
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodGet, "/me", "")
	requireError(t, rec, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	require.Same(t, echo.ErrUnauthorized, handled)

	e = newEcho()
	called := false
	taken := func(ctx context.Context, body *takenRequest) (whoResponse, error) {
		called = true
		return whoResponse{}, nil
	}
	reg = api.New(e, api.Public())
	api.POST(reg.Group("/names"), "", "claim", taken, api.Middleware(observe))
	reg.Ready()
	s = &server{e: e, reg: reg}

	handled = nil
	rec = s.do(http.MethodPost, "/names", "")
	requireError(t, rec, http.StatusConflict, "taken")
	require.Same(t, errTaken, handled)
	require.False(t, called, "handler invoked despite Validate failing")
}

func TestContextRequestNilOutsideRegistry(t *testing.T) {
	e := newEcho()
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
			handled = err
			request = handler.Request(c)
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
	e := newEcho()
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
	resp := requireError(t, rec, 499, "")
	require.Equal(t, ez.ECANCELED, resp.Error.Code)
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

	e := newEcho()
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
	e := newEcho()
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

// A handler that returns a nil pointer and no error is answered as a 500,
// not as a 200 null the document does not describe.
func TestNilPointerResponse(t *testing.T) {
	e := newEcho()
	missing := true
	check := func(ctx context.Context, req *fixture.HealthRequest) (*fixture.HealthResponse, error) {
		if missing {
			return nil, nil
		}
		return &fixture.HealthResponse{OK: true}, nil
	}
	reg := api.New(e, api.Public())
	api.GET(reg.Group("/health"), "", "check", check)
	reg.Ready()
	s := &server{e: e, reg: reg}

	rec := s.do(http.MethodGet, "/health", "")
	resp := requireError(t, rec, http.StatusInternalServerError, "the handler returned no response")
	require.Equal(t, ez.EINTERNAL, resp.Error.Code)

	missing = false
	rec = s.do(http.MethodGet, "/health", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"ok":true}`, rec.Body.String())
}
