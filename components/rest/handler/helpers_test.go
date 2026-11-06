package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestGetQueryParams(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{name: "absent", query: "", want: nil},
		{name: "absent with other params", query: "?other=a", want: nil},
		{name: "single", query: "?status=open", want: []string{"open"}},
		{name: "repeated", query: "?status=open&status=closed", want: []string{"open", "closed"}},
		{name: "comma separated", query: "?status=open,closed", want: []string{"open", "closed"}},
		{name: "mixed", query: "?status=open,closed&status=draft", want: []string{"open", "closed", "draft"}},
		{name: "whitespace trimmed", query: "?status=%20open%20,%20closed", want: []string{"open", "closed"}},
		{name: "empty entries dropped", query: "?status=open,,%20,&status=&status=closed", want: []string{"open", "closed"}},
		{name: "only empties", query: "?status=&status=,%20", want: nil},
	}

	h := &BaseHandler{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/items"+tt.query, nil)
			c := echo.New().NewContext(req, httptest.NewRecorder())

			require.Equal(t, tt.want, h.GetQueryParams(c, "status"))
		})
	}
}

func TestGetQueryParamBool(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		want     bool
		wantCode string
	}{
		{name: "absent", query: "", want: false},
		{name: "true", query: "?archived=true", want: true},
		{name: "one", query: "?archived=1", want: true},
		{name: "false", query: "?archived=false", want: false},
		{name: "unparsable", query: "?archived=maybe", wantCode: ez.EINVALID},
	}

	h := &BaseHandler{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/items"+tt.query, nil)
			c := echo.New().NewContext(req, httptest.NewRecorder())

			got, err := h.GetQueryParamBool(c, "archived")
			if tt.wantCode != "" {
				require.Equal(t, tt.wantCode, ez.ErrorCode(err))
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
