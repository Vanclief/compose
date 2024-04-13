package requests

import (
	"github.com/vanclief/ez"
)

type AuthenticatedRequest struct {
	StandardRequest
	APIKey    string
	APISecret string
}

func (r StandardRequest) Authenticate() AuthenticatedRequest {
	request := AuthenticatedRequest{
		StandardRequest: r,
		APIKey:          r.Header.Get("API-KEY"),
		APISecret:       r.Header.Get("API-SECRET"),
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
