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

func New(header http.Header, ip string, opts ...Option) *StandardRequest {
	id := uuid.New().String()

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
