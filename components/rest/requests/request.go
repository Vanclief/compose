package requests

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
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
}

// Option is a function that modifies the Request.
type Option func(*StandardRequest)

// WithTimeout returns an Option that sets the timeout for the request context.
func WithTimeout(timeout time.Duration) Option {
	return func(req *StandardRequest) {
		req.Context, req.Cancel = context.WithTimeout(context.WithoutCancel(req.Context), timeout)
	}
}

func New(header http.Header, ip string, opts ...Option) *StandardRequest {
	id := uuid.New().String()

	ctx := context.WithValue(context.Background(), "request-id", id)
	ctx, cancel := context.WithTimeout(ctx, DEFAULT_TIMEOUT)

	request := &StandardRequest{
		ID:        id,
		Client:    header.Get("Client"),
		IP:        ip,
		Header:    header,
		Context:   ctx,
		CreatedAt: time.Now(),
		Cancel:    cancel,
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

type Body interface {
	Validate() error
}
