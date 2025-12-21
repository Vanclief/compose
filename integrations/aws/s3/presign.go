package s3

import (
	"context"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/vanclief/ez"
)

type PresignedRequest struct {
	URL     string      `json:"url"`
	Method  string      `json:"method"`
	Headers http.Header `json:"headers,omitempty"`
}

type PresignedPostForm struct {
	URL    string            `json:"url"`
	Fields map[string]string `json:"fields"`
}

func (c *Client) PresignPutObject(ctx context.Context, input *s3.PutObjectInput, expires time.Duration) (*PresignedRequest, error) {
	const op = "Client.PresignPutObject"

	if input == nil {
		return nil, ez.New(op, ez.EINVALID, "input is required", nil)
	}

	input.Bucket = aws.String(c.Bucket)

	req, err := c.presign.PresignPutObject(ctx, input, func(opts *s3.PresignOptions) {
		if expires > 0 {
			opts.Expires = expires
		}
	})
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return &PresignedRequest{
		URL:     req.URL,
		Method:  req.Method,
		Headers: req.SignedHeader,
	}, nil
}

func (c *Client) PresignPostObject(ctx context.Context, input *s3.PutObjectInput, expires time.Duration, conditions ...interface{}) (*PresignedPostForm, error) {
	const op = "Client.PresignPostObject"

	if input == nil {
		return nil, ez.New(op, ez.EINVALID, "input is required", nil)
	}

	input.Bucket = aws.String(c.Bucket)

	req, err := c.presign.PresignPostObject(ctx, input, func(opts *s3.PresignPostOptions) {
		if expires > 0 {
			opts.Expires = expires
		}
		if len(conditions) > 0 {
			opts.Conditions = append(opts.Conditions, conditions...)
		}
	})
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return &PresignedPostForm{
		URL:    req.URL,
		Fields: req.Values,
	}, nil
}
