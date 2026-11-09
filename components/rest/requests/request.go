package requests

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/vanclief/compose/types"
)

const DEFAULT_TIMEOUT = 15 * time.Second

type Request interface {
	GetID() string
	GetIP() string
	GetClient() string
	GetHeader() http.Header
	GetBody() Body
	SetBody(Body)
	GetCreatedAt() time.Time
	GetContext() context.Context
	SetContext(context.Context)
	GetLocale() string
	SetLocale(string)
}

type StandardRequest struct {
	ID        string
	IP        string
	Client    string
	Header    http.Header
	Body      Body
	CreatedAt time.Time
	Context   context.Context
	Cancel    context.CancelFunc
	Locale    string
	// Method is the HTTP method, set by WithMethod. Authenticate needs it to
	// apply the CSRF rule of WithCookie; empty counts as unsafe.
	Method string
}

// requestIDHeader is the header key SetRequestID stores the request ID under
// and New reads it from. It is in lower case on purpose: net/http gives the
// name of every header it receives its canonical spelling (Compose-Request-Id),
// over HTTP/1 and HTTP/2 alike, so nothing a client sends lands under this
// key and only server code, through SetRequestID, puts a value there.
const requestIDHeader = "compose-request-id"

// SetRequestID stores id in header as the ID New gives the request it builds
// from header. rest.Configure does it for every request, with the id the
// response carries as X-Request-Id; an app with a middleware stack of its own
// calls it from a middleware to the same end. A client cannot set this ID,
// see requestIDHeader. The entry stays in header, so a dump of the request
// headers or a copy of them onto an outbound request shows it, as
// compose-request-id; a server receiving that copy canonicalises the name,
// so it does not read it as its own.
func SetRequestID(header http.Header, id string) {
	//lint:ignore SA1008 the key is not canonical on purpose, see requestIDHeader
	header[requestIDHeader] = []string{id}
}

// requestIDKey is the context key New stores the request ID under. An
// unexported type keeps it from colliding with keys set by other packages.
type requestIDKey struct{}

// RequestID returns the request ID New stored in ctx, or "" when ctx does not
// come from a request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// Option is a function that modifies the Request.
type Option func(*StandardRequest)

// WithTimeout returns an Option that sets the timeout for the request context.
// It replaces the previous deadline and releases the timer of the context it
// replaces, keeping the context values.
func WithTimeout(timeout time.Duration) Option {
	return func(req *StandardRequest) {
		previousCancel := req.Cancel
		req.Context, req.Cancel = context.WithTimeout(context.WithoutCancel(req.Context), timeout)

		if previousCancel != nil {
			previousCancel()
		}
	}
}

// WithMethod returns an Option that records the HTTP method of the request,
// for example c.Request().Method. Pass it when Authenticate may accept a
// cookie: the CSRF rule treats a request without a method as unsafe, so a
// cookie then needs an allowed Origin on every method.
func WithMethod(method string) Option {
	return func(req *StandardRequest) {
		req.Method = method
	}
}

// New returns the request of one handler call. Its ID is the one
// SetRequestID stored in header, which under rest.Configure is the id the
// response carries as X-Request-Id and the request log line as id, and a new
// UUID when none was stored. No header a client sends sets it, X-Request-Id
// included.
func New(header http.Header, ip string, opts ...Option) *StandardRequest {
	id := ""
	//lint:ignore SA1008 the key is not canonical on purpose, see requestIDHeader
	stored := header[requestIDHeader]
	if len(stored) > 0 {
		id = stored[0]
	}
	if id == "" {
		id = uuid.New().String()
	}

	ctx := context.WithValue(context.Background(), requestIDKey{}, id)
	ctx, cancel := context.WithTimeout(ctx, DEFAULT_TIMEOUT)

	locale, _ := types.NewLocaleString(header.Get("Accept-Language"))

	request := &StandardRequest{
		ID:        id,
		Client:    header.Get("Client"),
		IP:        ip,
		Header:    header,
		Context:   ctx,
		CreatedAt: time.Now(),
		Cancel:    cancel,
		Locale:    locale.String(),
	}

	// Apply each Option to the new request
	for _, opt := range opts {
		opt(request)
	}

	return request
}

func (r *StandardRequest) GetID() string {
	return r.ID
}

func (r *StandardRequest) GetIP() string {
	return r.IP
}

func (r *StandardRequest) GetClient() string {
	return r.Client
}

func (r *StandardRequest) GetHeader() http.Header {
	return r.Header
}

func (r *StandardRequest) GetBody() Body {
	return r.Body
}

func (r *StandardRequest) GetContext() context.Context {
	return r.Context
}

func (r *StandardRequest) GetCreatedAt() time.Time {
	return r.CreatedAt
}

func (r *StandardRequest) SetBody(body Body) {
	r.Body = body
}

func (r *StandardRequest) SetContext(ctx context.Context) {
	r.Context = ctx
}

// GetLocale returns the locale used to localize the response, e.g. "es-MX".
// New negotiates it from the Accept-Language header, so it is always set.
func (r *StandardRequest) GetLocale() string {
	return r.Locale
}

// SetLocale overrides the negotiated locale, e.g. with an authenticated
// user's saved preference.
func (r *StandardRequest) SetLocale(locale string) {
	r.Locale = locale
}

type Body interface {
	Validate() error
}
