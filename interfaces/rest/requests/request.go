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

func New(header http.Header, ip string) *Request {
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
