package apidocs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"
)

// Page is a reference page rendered once, typically at startup, and served
// from memory.
type Page struct {
	HTML []byte
	ETag string
}

// New parses spec (YAML or JSON) and renders it with opts.
func New(spec []byte, opts Options) (*Page, error) {
	doc, err := Parse(spec)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	html, err := Render(doc, opts)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return &Page{HTML: html, ETag: etag(html)}, nil
}

// Handler serves the page as text/html. Clients that send the current ETag in
// If-None-Match get 304 Not Modified. The page is marked private because the
// spec behind it is usually served to authenticated users only.
func (p *Page) Handler() echo.HandlerFunc {
	return func(c echo.Context) error {
		header := c.Response().Header()
		header.Set("ETag", p.ETag)
		header.Set("Cache-Control", "private, no-cache")

		if etagMatches(c.Request().Header.Get("If-None-Match"), p.ETag) {
			return c.NoContent(http.StatusNotModified)
		}

		return c.Blob(http.StatusOK, "text/html; charset=utf-8", p.HTML)
	}
}

type asset struct {
	data        []byte
	contentType string
	etag        string
}

var embeddedAssets = loadAssets()

func loadAssets() map[string]asset {
	types := map[string]string{
		"apidocs.css": "text/css; charset=utf-8",
		"apidocs.js":  "text/javascript; charset=utf-8",
	}

	out := map[string]asset{}
	for name, contentType := range types {
		data, err := assetFS.ReadFile("assets/" + name)
		if err != nil {
			panic("apidocs: missing embedded asset " + name)
		}
		out[name] = asset{data: data, contentType: contentType, etag: etag(data)}
	}
	return out
}

// assetVersion is the ETag of an embedded asset without quotes. Pages link
// assets with ?v=<version> so a new page never runs a cached older script.
func assetVersion(name string) string {
	return strings.Trim(embeddedAssets[name].etag, `"`)
}

// Assets serves apidocs.css and apidocs.js from the embedded files, matched on
// the last path segment so it works with or without http.StripPrefix; the
// ?v= version query that Render appends is ignored. Every other path is a
// 404. Responses carry an ETag and a short max-age, so a deploy is picked up
// within minutes and revalidation is a cheap 304.
//
// Mount Assets on a public route: the page loads it with <link> and
// <script src>, which cannot send an Authorization header.
func Assets() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := embeddedAssets[path.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", a.contentType)
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("ETag", a.etag)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.data))
	})
}

// AssetsHandler adapts Assets to Echo, for routes like
// e.GET("/docs/assets/*", apidocs.AssetsHandler()) paired with
// Options.AssetsURL = "/docs/assets".
func AssetsHandler() echo.HandlerFunc {
	return echo.WrapHandler(Assets())
}

func etag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

func etagMatches(header, tag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == tag {
			return true
		}
	}
	return false
}
