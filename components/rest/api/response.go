package api

import (
	"fmt"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/labstack/echo/v4"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/openapi"
)

// Blob is a non-JSON response body (a PDF, CSV or ZIP file). An operation
// registered with Res = Blob and Produces returns it from HandleRequest, as
// Blob or *Blob. An operation with a JSON Res that negotiates (Produces lists
// application/json and other types) returns Res for JSON and a Blob for the
// other types; requests.Request.GetAccept tells the app which was chosen.
type Blob struct {
	// ContentType is written as the Content-Type header. Empty means the
	// negotiated media type. It may carry parameters (text/plain;
	// charset=utf-8).
	ContentType string
	Body        []byte
	// Filename, when set, is sent as Content-Disposition: attachment.
	Filename string
}

var blobType = reflect.TypeFor[Blob]()

// Produces declares the operation's 200 response media types, without
// parameters or wildcards. Required when Res is Blob, which must not list
// application/json. On a JSON Res it must include application/json and makes
// the operation negotiate: the pipeline picks the first Accept entry, in
// header order, that matches a declared type (type/* and */* match the first
// declared type in that range), defaulting to the first declared type.
// q-values are ignored. Without Produces an operation produces
// application/json.
func Produces(mediaTypes ...string) Option {
	return func(op *operation) {
		op.produces = mediaTypes
	}
}

// checkProduces validates the declared media types against Res and sets the
// application/json default.
func checkProduces(op *operation) error {
	if len(op.produces) == 0 {
		if op.blob {
			return ez.New(ez.EINVALID, "api.Blob responses need api.Produces with their media types", nil)
		}
		op.produces = []string{echo.MIMEApplicationJSON}
		return nil
	}

	for i, mediaType := range op.produces {
		parsed, params, err := mime.ParseMediaType(mediaType)
		if err != nil || len(params) > 0 || parsed != mediaType || strings.Contains(mediaType, "*") {
			return ez.New(ez.EINVALID, fmt.Sprintf("Produces media type %q must be a lowercase type/subtype without parameters or wildcards", mediaType), err)
		}
		if slices.Contains(op.produces[:i], mediaType) {
			return ez.New(ez.EINVALID, fmt.Sprintf("Produces lists %q twice", mediaType), nil)
		}
	}

	hasJSON := slices.Contains(op.produces, echo.MIMEApplicationJSON)
	if op.blob && hasJSON {
		return ez.New(ez.EINVALID, "api.Blob responses cannot produce application/json: use a JSON Res and list application/json in Produces to negotiate", nil)
	}
	if !op.blob && !hasJSON {
		return ez.New(ez.EINVALID, fmt.Sprintf("Produces on JSON response type %s must include application/json: use api.Blob as Res when the operation never returns JSON", op.resType), nil)
	}
	return nil
}

// negotiate picks the response media type for an Accept header.
func negotiate(accept string, produces []string) string {
	for _, part := range strings.Split(accept, ",") {
		mediaRange, _, _ := strings.Cut(part, ";")
		mediaRange = strings.ToLower(strings.TrimSpace(mediaRange))
		for _, mediaType := range produces {
			if mediaMatches(mediaRange, mediaType) {
				return mediaType
			}
		}
	}
	return produces[0]
}

func mediaMatches(mediaRange, mediaType string) bool {
	if mediaRange == "*/*" || mediaRange == mediaType {
		return true
	}
	prefix, ok := strings.CutSuffix(mediaRange, "/*")
	return ok && strings.HasPrefix(mediaType, prefix+"/")
}

// writeResponse writes what HandleRequest returned: a Blob as is, anything
// else as JSON.
func writeResponse(c echo.Context, response any, mediaType string) error {
	blob, ok := asBlob(response)
	if !ok {
		return c.JSON(http.StatusOK, response)
	}

	contentType := blob.ContentType
	if contentType == "" {
		contentType = mediaType
	}
	if blob.Filename != "" {
		disposition := mime.FormatMediaType("attachment", map[string]string{"filename": blob.Filename})
		c.Response().Header().Set(echo.HeaderContentDisposition, disposition)
	}
	return c.Blob(http.StatusOK, contentType, blob.Body)
}

func asBlob(response any) (Blob, bool) {
	switch v := response.(type) {
	case Blob:
		return v, true
	case *Blob:
		if v != nil {
			return *v, true
		}
	}
	return Blob{}, false
}

// responseContent documents the 200 response: Res's schema for
// application/json, binary for every other declared type.
func (op *operation) responseContent(res *openapi.Result) map[string]*huma.MediaType {
	content := map[string]*huma.MediaType{}
	for _, mediaType := range op.produces {
		if mediaType == echo.MIMEApplicationJSON {
			content[mediaType] = &huma.MediaType{Schema: res.ResponseSchema(op.resType)}
			continue
		}
		content[mediaType] = &huma.MediaType{Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}
	}
	return content
}
