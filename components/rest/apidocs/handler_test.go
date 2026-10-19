package apidocs

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestPageHandlerETag(t *testing.T) {
	page, err := New([]byte(renderSpec), Options{AssetsURL: "/docs/assets"})
	require.NoError(t, err)
	require.NotEmpty(t, page.ETag)

	e := echo.New()
	e.GET("/docs", page.Handler())

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, page.ETag, rec.Header().Get("ETag"))
	require.Equal(t, string(page.HTML), rec.Body.String())

	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	req.Header.Set("If-None-Match", page.ETag)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotModified, rec.Code)
	require.Empty(t, rec.Body.String())

	req = httptest.NewRequest(http.MethodGet, "/docs", nil)
	req.Header.Set("If-None-Match", `"stale"`)
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestNewRejectsInvalidSpec(t *testing.T) {
	_, err := New([]byte("not: [an openapi"), Options{})
	require.Error(t, err)
}

func TestAssets(t *testing.T) {
	cases := []struct {
		path        string
		status      int
		contentType string
	}{
		{"/apidocs.css", http.StatusOK, "text/css; charset=utf-8"},
		{"/apidocs.js", http.StatusOK, "text/javascript; charset=utf-8"},
		{"/docs/assets/apidocs.js", http.StatusOK, "text/javascript; charset=utf-8"},
		{"/docs/assets/apidocs.js?v=" + assetVersion("apidocs.js"), http.StatusOK, "text/javascript; charset=utf-8"},
		{"/apidocs.html", http.StatusNotFound, ""},
		{"/", http.StatusNotFound, ""},
	}

	for _, tc := range cases {
		rec := httptest.NewRecorder()
		Assets().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		require.Equal(t, tc.status, rec.Code, tc.path)
		if tc.status != http.StatusOK {
			continue
		}
		require.Equal(t, tc.contentType, rec.Header().Get("Content-Type"), tc.path)
		require.NotEmpty(t, rec.Header().Get("ETag"), tc.path)
		require.NotEmpty(t, rec.Header().Get("Cache-Control"), tc.path)
		require.NotEmpty(t, rec.Body.Bytes(), tc.path)
	}

	etag := embeddedAssets["apidocs.js"].etag
	req := httptest.NewRequest(http.MethodGet, "/apidocs.js", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	Assets().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotModified, rec.Code)
}

func TestAssetsHandlerEcho(t *testing.T) {
	e := echo.New()
	e.GET("/docs/assets/*", AssetsHandler())

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/assets/apidocs.css", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "text/css; charset=utf-8", rec.Header().Get("Content-Type"))

	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs/assets/other.txt", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}
