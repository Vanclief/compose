package requests

import (
	"github.com/vanclief/ez"
)

type AuthenticatedRequest struct {
	StandardRequest
	APIKey    string
	APISecret string
}

func (r StandardRequest) Authenticate() (*AuthenticatedRequest, error) {
	const op = "StandardRequest.Authenticate"

	authRequest := &AuthenticatedRequest{
		StandardRequest: r,
		APIKey:          r.Header.Get("API-KEY"),
		APISecret:       r.Header.Get("API-SECRET"),
	}

	if authRequest.APIKey == "" {
		return nil, ez.New(op, ez.EINVALID, "Request is missing API-KEY authentication header", nil)
	} else if authRequest.APISecret == "" {
		return nil, ez.New(op, ez.EINVALID, "Request is missing API-SECRET authentication header", nil)
	}

	return authRequest, nil
}
