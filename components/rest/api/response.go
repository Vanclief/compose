package api

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/vanclief/ez"

	"github.com/vanclief/compose/components/rest/api/internal/huma"
	"github.com/vanclief/compose/components/rest/api/openapi"
	"github.com/vanclief/compose/components/rest/requests"
)

// Blob is a non-JSON response body (a PDF, CSV or ZIP file). A handler whose
// Res is Blob (registered with ContentType) returns it as the operation's
// primary representation; a Produces handler returns it as an alternate one.
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

// producer is an alternate representation declared with Produces.
type producer struct {
	mediaType string
	reqType   reflect.Type
	fn        func(context.Context, requests.Body) (Blob, error)
}

// ContentType declares the media type of an operation whose handler returns
// Blob: required then, and rejected on any other Res. The media type is a
// lowercase type/subtype without parameters or wildcards, never
// application/json. Operation option.
func ContentType(mediaType string) Option {
	return Option{name: "api.ContentType", routeOnly: true, op: func(op *operation) {
		op.contentType = mediaType
	}}
}

// Produces adds an alternate 200 representation: when the client negotiates
// mediaType, fn runs instead of the operation's handler. Req must be the
// operation's request type. mediaType follows ContentType's rules, must not
// repeat and must differ from the primary media type (application/json for a
// JSON Res, the ContentType for a Blob Res).
//
// The pipeline picks the first Accept entry, in header order, that matches a
// declared type (type/* and */* match the first declared type in that
// range), defaulting to the primary one; q-values are ignored. Exactly one
// handler runs. Every answer of the operation's pipeline, its errors
// included, carries Vary: Accept; an error raised by middleware in front of
// it does not. Operation option. Panics when fn is nil.
func Produces[Req any, P interface {
	*Req
	requests.Body
}](mediaType string, fn func(context.Context, P) (Blob, error)) Option {
	if fn == nil {
		panic(ez.New(ez.EINVALID, "api: Produces needs a function", nil))
	}
	p := producer{
		mediaType: mediaType,
		reqType:   reflect.TypeFor[Req](),
		fn: func(ctx context.Context, body requests.Body) (Blob, error) {
			return fn(ctx, body.(P))
		},
	}
	return Option{name: "api.Produces", routeOnly: true, op: func(op *operation) {
		op.producers = append(op.producers, p)
	}}
}

// checkProduces validates ContentType and the producers against Res and the
// request type, and sets the operation's media types.
func checkProduces(op *operation) error {
	primary := echo.MIMEApplicationJSON
	if op.blob {
		if op.contentType == "" {
			return ez.New(ez.EINVALID, "api.Blob responses need api.ContentType with their media type", nil)
		}
		err := checkMediaType("ContentType", op.contentType)
		if err != nil {
			return err
		}
		primary = op.contentType
	} else if op.contentType != "" {
		return ez.New(ez.EINVALID, fmt.Sprintf("api.ContentType needs a handler returning api.Blob, not %s", op.resType), nil)
	}

	op.produces = []string{primary}
	for _, p := range op.producers {
		err := checkMediaType("Produces", p.mediaType)
		if err != nil {
			return err
		}
		if p.mediaType == primary {
			return ez.New(ez.EINVALID, fmt.Sprintf("Produces %q repeats the primary media type", p.mediaType), nil)
		}
		if slices.Contains(op.produces, p.mediaType) {
			return ez.New(ez.EINVALID, fmt.Sprintf("Produces lists %q twice", p.mediaType), nil)
		}
		if p.reqType != op.reqType {
			return ez.New(ez.EINVALID, fmt.Sprintf("Produces %q handler takes %s, but the operation's request type is %s", p.mediaType, p.reqType, op.reqType), nil)
		}
		op.produces = append(op.produces, p.mediaType)
	}
	return nil
}

// checkMediaType checks a declared media type: lowercase type/subtype, no
// parameters or wildcards, not application/json.
func checkMediaType(option, mediaType string) error {
	parsed, params, err := mime.ParseMediaType(mediaType)
	if err != nil || len(params) > 0 || parsed != mediaType || strings.Contains(mediaType, "*") {
		return ez.New(ez.EINVALID, fmt.Sprintf("%s media type %q must be a lowercase type/subtype without parameters or wildcards", option, mediaType), err)
	}
	if mediaType == echo.MIMEApplicationJSON {
		return ez.New(ez.EINVALID, fmt.Sprintf("%s cannot declare application/json: a handler returning a JSON Res produces it", option), nil)
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

// writeResponse writes what the handler returned: a Blob as is, anything
// else as JSON.
func writeResponse(c *echo.Context, response any, mediaType string) error {
	blob, ok := response.(Blob)
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
