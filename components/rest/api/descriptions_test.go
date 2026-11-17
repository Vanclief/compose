package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"github.com/vanclief/compose/components/rest/api"
	"github.com/vanclief/compose/components/rest/api/descriptions"
	"github.com/vanclief/compose/components/rest/api/internal/described"
	"github.com/vanclief/compose/components/rest/apitest"
	"github.com/vanclief/compose/components/rest/handler"
	"github.com/vanclief/compose/components/rest/requests"
)

// describedPkg is the import path of the fixture the tests describe.
const describedPkg = "github.com/vanclief/compose/components/rest/api/internal/described"

// describedFile is what `compose descriptions` writes for the fixture: the
// reader runs over its source, so the keys the registry looks up and the
// keys the reader writes cannot drift apart.
func describedFile(t *testing.T) []byte {
	t.Helper()
	result, err := descriptions.Read("internal/described")
	require.NoError(t, err)
	require.Empty(t, result.Duplicates)
	require.Empty(t, result.MainPackages)
	data, err := descriptions.Marshal(result.Descriptions)
	require.NoError(t, err)
	return data
}

func describedRegistry(t *testing.T, opts ...api.Option) (*echo.Echo, *api.Registry) {
	t.Helper()

	e := newEcho()
	opts = append([]api.Option{api.Public()}, opts...)
	reg := api.New(e, opts...)
	g := reg.Group("/projects/:project/runs")
	api.POST(g, "", "create", described.Create)
	api.GET(g, "", "list", described.List)
	api.POST(g, "/logs", "upload", described.Upload)
	api.POST(g, "/archive", "archive", described.Archive, api.Hidden())
	reg.Ready()
	return e, reg
}

func TestDescriptionsReaderKeys(t *testing.T) {
	var comments map[string]string
	err := json.Unmarshal(describedFile(t), &comments)
	require.NoError(t, err)

	require.Equal(t, "Run is one execution of a job.", comments[describedPkg+".Run"])
	require.Equal(t, "Items are the entries of the page.", comments[describedPkg+".Page#Items"])
	require.Equal(t, "Email reaches the owner.", comments[describedPkg+".Owner#Email"])
	require.NotContains(t, comments, describedPkg+".Run#Record")
	require.NotContains(t, comments, describedPkg+".Run#Plain")
}

func TestDescriptionsDocuments(t *testing.T) {
	_, reg := describedRegistry(t, api.Descriptions(describedFile(t)))

	v31, err := reg.Spec(api.V31)
	require.NoError(t, err)
	v30, err := reg.Spec(api.V30)
	require.NoError(t, err)
	strict := reg.StrictSpec()

	for name, raw := range map[string][]byte{"3.1": v31, "3.0": v30, "strict": strict} {
		t.Run(name, func(t *testing.T) {
			doc := decode(t, raw)
			schemas := dig(t, doc, "components", "schemas")

			// Types: a struct, an enum, the request copy and a generic
			// instantiation.
			require.Equal(t, "Run is one execution of a job.", dig(t, schemas, "described.Run", "description"))
			require.Equal(t, "Status is where a run is.", dig(t, schemas, "described.Status", "description"))
			require.Equal(t, "Owner is the person who started a run.", dig(t, schemas, "described.Owner", "description"))
			require.Equal(t, "Owner is the person who started a run.", dig(t, schemas, "described.OwnerInput", "description"))
			require.Equal(t, "Page is one page of a list.", dig(t, schemas, "described.Page_described.Run", "description"))
			require.Equal(t, "CreateRequest starts a run.", dig(t, schemas, "described.CreateRequest", "description"))

			// Fields: a comment, a promoted field, the tag winning, an empty
			// tag hiding the comment, a field without a comment, a generic
			// field and fields whose schema is a $ref.
			run := dig(t, schemas, "described.Run", "properties")
			require.Equal(t, "Name is what the user calls the run.", dig(t, run, "name", "description"))
			require.Equal(t, "ID identifies the record.", dig(t, run, "id", "description"))
			// Promoted through an embedded pointer, and through two levels.
			require.Equal(t, "Author wrote the record.", dig(t, run, "author", "description"))
			require.Equal(t, "At is when it happened.", dig(t, run, "at", "description"))
			require.Equal(t, "Label from the tag.", dig(t, run, "label", "description"))
			require.NotContains(t, dig(t, run, "note"), "description")
			require.NotContains(t, dig(t, run, "plain"), "description")
			require.Equal(t, "Items are the entries of the page.",
				dig(t, schemas, "described.Page_described.Run", "properties", "items", "description"))
			require.Equal(t, "Owner started the run.", dig(t, run, "owner", "description"))
			require.Equal(t, "Status tells where the run is.", dig(t, run, "status", "description"))
			require.Equal(t, "Owner starts the new run.",
				dig(t, schemas, "described.CreateRequest", "properties", "owner", "description"))

			// Parameters: path (promoted from Scope), query and header.
			create := dig(t, doc, "paths", "/projects/{project}/runs", "post", "parameters")
			require.Equal(t, "Project is the project the run belongs to.", dig(t, create, "project", "description"))
			require.Equal(t, "Limit caps the steps of the run.", dig(t, create, "limit", "description"))
			require.Equal(t, "Trace follows the request across services.", dig(t, create, "x-trace", "description"))

			// Multipart: a form value and a file.
			form := dig(t, doc, "paths", "/projects/{project}/runs/logs", "post", "requestBody", "content", "multipart/form-data", "schema", "properties")
			require.Equal(t, "Log is the file to attach.", dig(t, form, "log", "description"))
			require.Equal(t, "Comment says what the log shows.", dig(t, form, "comment", "description"))
			require.Equal(t, "Extras are more files to attach.", dig(t, form, "extras", "description"))
			require.Equal(t, "array", dig(t, form, "extras", "type"))

			// A type only a hidden operation takes is in the strict
			// document alone, described there.
			if name == "strict" {
				require.Equal(t, "ArchiveRequest archives the runs of a project. Only a hidden operation\ntakes it.",
					dig(t, schemas, "described.ArchiveRequest", "description"))
				require.Equal(t, "Reason says why the runs are archived.",
					dig(t, schemas, "described.ArchiveRequest", "properties", "reason", "description"))
			} else {
				require.NotContains(t, schemas, "described.ArchiveRequest")
			}
		})
	}
}

// Tagged and Commented differ only in where the descriptions of their fields
// come from: Tagged's doc tags, Commented's comments.
type Tagged struct {
	requests.NoRules
	Limit  int               `json:"-" query:"limit" doc:"Limit caps the list."`
	Owner  *described.Owner  `json:"owner,omitempty" doc:"Owner starts it."`
	Lead   described.Owner   `json:"lead" doc:"Lead runs it."`
	Status *described.Status `json:"status,omitempty" doc:"Status filters it."`
	Kind   described.Status  `json:"kind" enum:"done" doc:"Kind narrows it."`
	Tags   []string          `json:"tags,omitempty" doc:"Tags label it."`
}

type Commented struct {
	requests.NoRules
	Limit  int               `json:"-" query:"limit"`
	Owner  *described.Owner  `json:"owner,omitempty"`
	Lead   described.Owner   `json:"lead"`
	Status *described.Status `json:"status,omitempty"`
	Kind   described.Status  `json:"kind" enum:"done"`
	Tags   []string          `json:"tags,omitempty"`
}

// TestDescriptionsSameAsTag checks the rule: a field without a doc tag is
// described exactly as if it carried doc:"<its comment>", in every
// document, a $ref, a nullable $ref and a narrowed enum included.
func TestDescriptionsSameAsTag(t *testing.T) {
	key := descriptions.Key(reflect.TypeFor[Commented]())
	comments, err := json.Marshal(map[string]string{
		key + "#Limit":  "Limit caps the list.",
		key + "#Owner":  "Owner starts it.",
		key + "#Lead":   "Lead runs it.",
		key + "#Status": "Status filters it.",
		key + "#Kind":   "Kind narrows it.",
		key + "#Tags":   "Tags label it.",
	})
	require.NoError(t, err)

	reg := api.New(newEcho(), api.Public(), api.Descriptions(comments))
	api.POST(reg.Group("/tagged"), "", "create", func(_ context.Context, _ *Tagged) (described.Owner, error) {
		return described.Owner{}, nil
	})
	api.POST(reg.Group("/commented"), "", "create", func(_ context.Context, _ *Commented) (described.Owner, error) {
		return described.Owner{}, nil
	})

	v31, err := reg.Spec(api.V31)
	require.NoError(t, err)
	v30, err := reg.Spec(api.V30)
	require.NoError(t, err)

	for name, raw := range map[string][]byte{"3.1": v31, "3.0": v30, "strict": reg.StrictSpec()} {
		t.Run(name, func(t *testing.T) {
			doc := decode(t, raw)
			schemas := dig(t, doc, "components", "schemas")
			tagged := dig(t, schemas, "api_test.Tagged", "properties")
			require.Equal(t, tagged, dig(t, schemas, "api_test.Commented", "properties"))
			require.Equal(t, "Owner starts it.", dig(t, tagged, "owner", "description"))
			require.Equal(t, "Lead runs it.", dig(t, tagged, "lead", "description"))
			require.Equal(t,
				dig(t, doc, "paths", "/tagged", "post", "parameters"),
				dig(t, doc, "paths", "/commented", "post", "parameters"))
			require.Equal(t, "Limit caps the list.", dig(t, doc, "paths", "/commented", "post", "parameters", "limit", "description"))
		})
	}
}

// TestDescriptionsWithoutOption checks that a registry without the option,
// or with an empty file, publishes no description from a comment.
func TestDescriptionsWithoutOption(t *testing.T) {
	_, without := describedRegistry(t)
	_, empty := describedRegistry(t, api.Descriptions([]byte("{}\n")))
	_, with := describedRegistry(t, api.Descriptions(describedFile(t)))

	for _, v := range []api.Version{api.V31, api.V30} {
		a, err := without.Spec(v)
		require.NoError(t, err)
		b, err := empty.Spec(v)
		require.NoError(t, err)
		c, err := with.Spec(v)
		require.NoError(t, err)
		require.Equal(t, string(a), string(b))
		require.NotEqual(t, string(a), string(c))
		require.NotContains(t, string(a), "one execution of a job")
		require.Contains(t, string(a), "Label from the tag.")
	}
	require.Equal(t, string(without.StrictSpec()), string(empty.StrictSpec()))
}

func TestDescriptionsPanics(t *testing.T) {
	for _, data := range []string{"", "null", "[]", `{"a": 1}`, `{"a": "b"`, `"text"`} {
		msg := panicMessage(t, func() {
			api.Descriptions([]byte(data))
		})
		require.Equal(t, "api: Descriptions: the data is not a JSON object of strings, as compose descriptions writes it", msg, data)
	}

	msg := panicMessage(t, func() {
		api.Descriptions([]byte(`{"a": "b", "c": null}`))
	})
	require.Equal(t, "api: Descriptions: the data is not a JSON object of strings, as compose descriptions writes it: c is null", msg)

	msg = panicMessage(t, func() {
		api.New(newEcho(), api.Descriptions([]byte("{}")), api.Descriptions([]byte("{}")))
	})
	require.Equal(t, "api: New: api.Descriptions is passed twice: pass one file", msg)

	msg = panicMessage(t, func() {
		api.New(newEcho()).Group("/x", api.Descriptions([]byte("{}")))
	})
	require.Contains(t, msg, "api.Descriptions is a registry option: pass it to api.New, not to a group")
}

// TestDescriptionsValidation checks that descriptions are annotations: every
// request is answered the same with and without them.
func TestDescriptionsValidation(t *testing.T) {
	without, _ := describedRegistry(t)
	with, _ := describedRegistry(t, api.Descriptions(describedFile(t)))

	logForm, logType := describedForm(t, "comment", "a note", "log", "")
	noLogForm, noLogType := describedForm(t, "comment", "a note")
	cases := []struct {
		method, target, contentType, body string
	}{
		{http.MethodPost, "/projects/p/runs?limit=2", echo.MIMEApplicationJSON, `{"name":"nightly","owner":{"email":"a@b.c"}}`},
		{http.MethodPost, "/projects/p/runs?limit=0", echo.MIMEApplicationJSON, `{"name":""}`},
		{http.MethodPost, "/projects/p/runs?limit=x", echo.MIMEApplicationJSON, `{"owner":{"email":5}}`},
		{http.MethodPost, "/projects/p/runs", echo.MIMEApplicationJSON, `{"name":"a","extra":true}`},
		{http.MethodGet, "/projects/p/runs", "", ""},
		{http.MethodPost, "/projects/p/runs/logs", logType, logForm},
		{http.MethodPost, "/projects/p/runs/logs", noLogType, noLogForm},
	}

	answer := func(e *echo.Echo, method, target, contentType, body string) (int, string) {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if contentType != "" {
			req.Header.Set(echo.HeaderContentType, contentType)
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code < http.StatusBadRequest {
			return rec.Code, rec.Body.String()
		}

		var resp handler.ErrorResponse
		err := json.Unmarshal(rec.Body.Bytes(), &resp)
		require.NoError(t, err)
		resp.Error.RequestID = ""
		out, err := json.Marshal(resp)
		require.NoError(t, err)
		return rec.Code, string(out)
	}

	codes := map[int]bool{}
	for _, c := range cases {
		codeWithout, bodyWithout := answer(without, c.method, c.target, c.contentType, c.body)
		codeWith, bodyWith := answer(with, c.method, c.target, c.contentType, c.body)
		require.Equal(t, codeWithout, codeWith, c.target+" "+c.body)
		require.Equal(t, bodyWithout, bodyWith, c.target+" "+c.body)
		codes[codeWith] = true
	}
	require.True(t, codes[http.StatusOK] && codes[http.StatusBadRequest], "the cases answer both a 200 and a 400")

	// The apitest contract compiles the described strict document and
	// judges a response as it does without descriptions.
	for _, opts := range [][]api.Option{nil, {api.Descriptions(describedFile(t))}} {
		e, reg := describedRegistry(t, opts...)
		contract := apitest.New(t, reg)
		req := httptest.NewRequest(http.MethodPost, "/projects/p/runs", strings.NewReader(`{"name":"nightly"}`))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := contract.Do(t, e, "runs.create", req)
		require.Equal(t, http.StatusOK, rec.Code)

		fake := &fakeTB{TB: t}
		contract.AssertResponse(fake, "runs.create", http.StatusOK, []byte(`{"name":5}`))
		require.NotEmpty(t, fake.errors)
	}
}

// describedForm builds a multipart body from name, value pairs. A pair whose
// value is "" is sent as a file of that name.
func describedForm(t *testing.T, pairs ...string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] == "" {
			part, err := w.CreateFormFile(pairs[i], pairs[i]+".txt")
			require.NoError(t, err)
			_, err = part.Write([]byte("log line"))
			require.NoError(t, err)
			continue
		}
		err := w.WriteField(pairs[i], pairs[i+1])
		require.NoError(t, err)
	}
	err := w.Close()
	require.NoError(t, err)
	return buf.String(), w.FormDataContentType()
}
