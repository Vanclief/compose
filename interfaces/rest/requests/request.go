package requests

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/vanclief/ez"
)

type Request struct {
	ID        string
	APIKey    string
	APISecret string
	IP        string
	Client    string
	Body      Body
	Context   context.Context
	Cancel    context.CancelFunc
}

// Option is a function that modifies the Request.
type Option func(*Request)

// WithTimeout returns an Option that sets the timeout for the request context.
func WithTimeout(timeout time.Duration) Option {
	return func(req *Request) {
		req.Context, req.Cancel = context.WithTimeout(req.Context, timeout)
	}
}

func New(header http.Header, ip string, opts ...Option) *Request {
	id := uuid.New().String()

	ctx := context.WithValue(context.Background(), "request-id", id)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)

	request := &Request{
		ID:        id,
		APIKey:    header.Get("API-KEY"),
		APISecret: header.Get("API-SECRET"),
		Client:    header.Get("Client"),
		IP:        ip,
		Context:   ctx,
		Cancel:    cancel,
	}

	return request
}

func (r *Request) SetBody(body Body) {
	r.Body = body
}

func (r *Request) VerifyHeaders() error {
	const op = "Request.VerifyHeaders"

	if r.APIKey == "" {
		return ez.New(op, ez.EINVALID, "Request is missing API-KEY authentication header", nil)
	} else if r.APISecret == "" {
		return ez.New(op, ez.EINVALID, "Request is missing API-SECRET authentication header", nil)
	}

	return nil
}

type Body interface {
	Validate() error
}
