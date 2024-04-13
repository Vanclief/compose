package requests

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/vanclief/ez"
)

type AuthenticatedRequest struct {
	StandardRequest
	APIKey    string
	APISecret string
}

func NewAuthenticated(header http.Header, ip string, opts ...Option) AuthenticatedRequest {
	id := uuid.New().String()

	ctx := context.WithValue(context.Background(), "request-id", id)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)

	request := AuthenticatedRequest{
		StandardRequest: StandardRequest{
			ID:      id,
			Client:  header.Get("Client"),
			IP:      ip,
			Context: ctx,
			Cancel:  cancel,
		},
		APIKey:    header.Get("API-KEY"),
		APISecret: header.Get("API-SECRET"),
	}

	return request
}

func (r *AuthenticatedRequest) VerifyAPIHeaders() error {
	const op = "AuthenticatedRequest.VerifyAPIHeaders"

	if r.APIKey == "" {
		return ez.New(op, ez.EINVALID, "Request is missing API-KEY authentication header", nil)
	} else if r.APISecret == "" {
		return ez.New(op, ez.EINVALID, "Request is missing API-SECRET authentication header", nil)
	}

	return nil
}
