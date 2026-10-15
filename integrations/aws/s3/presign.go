package s3

import (
	"context"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
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
	if input == nil {
		return nil, ez.New(ez.EINVALID, "input is required", nil)
	}

	input.Bucket = aws.String(c.Bucket)

	req, err := c.presign.PresignPutObject(ctx, input, func(opts *s3.PresignOptions) {
		if expires > 0 {
			opts.Expires = expires
		}
	})
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return &PresignedRequest{
		URL:     req.URL,
		Method:  req.Method,
		Headers: req.SignedHeader,
	}, nil
}

func (c *Client) PresignPostObject(ctx context.Context, input *s3.PutObjectInput, expires time.Duration, conditions ...interface{}) (*PresignedPostForm, error) {
	if input == nil {
		return nil, ez.New(ez.EINVALID, "input is required", nil)
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
		return nil, ez.Wrap(err)
	}

	return &PresignedPostForm{
		URL:    req.URL,
		Fields: req.Values,
	}, nil
}

// fixedTimePresigner signs a presigned request as of a chosen instant instead of now, so every
// URL built within one window is byte-identical and caches keyed by URL keep hitting. It wraps
// the SDK's default signer, keeping its settings, and only overrides the signing time.
type fixedTimePresigner struct {
	next        s3.HTTPPresignerV4
	signingTime time.Time
}

func (p *fixedTimePresigner) PresignHTTP(ctx context.Context, credentials aws.Credentials, r *http.Request, payloadHash string, service string, region string, _ time.Time, optFns ...func(*v4.SignerOptions)) (string, http.Header, error) {
	return p.next.PresignHTTP(ctx, credentials, r, payloadHash, service, region, p.signingTime, optFns...)
}

// PresignGetObject presigns a GET for input.Key, signed as of signingTime and valid for expires
// from that instant. Two calls with the same input and signingTime return the same URL. With a
// non-zero signingTime, expires is required and signingTime plus expires must be in the future.
// A zero signingTime signs as of now. Headers holds the signed headers (e.g. Range) the caller
// must send with the request. The URL points at the bucket's origin host; a CDN in front of the
// bucket that forwards the query string serves it too.
func (c *Client) PresignGetObject(ctx context.Context, input *s3.GetObjectInput, signingTime time.Time, expires time.Duration) (*PresignedRequest, error) {
	if input == nil {
		return nil, ez.New(ez.EINVALID, "input is required", nil)
	}

	if !signingTime.IsZero() {
		if expires <= 0 {
			return nil, ez.New(ez.EINVALID, "expires must be positive when signingTime is set", nil)
		}
		if !signingTime.Add(expires).After(time.Now()) {
			return nil, ez.New(ez.EINVALID, "signingTime plus expires must be in the future", nil)
		}
	}

	input.Bucket = aws.String(c.Bucket)

	req, err := c.presign.PresignGetObject(ctx, input, func(opts *s3.PresignOptions) {
		if expires > 0 {
			opts.Expires = expires
		}
		if !signingTime.IsZero() {
			opts.Presigner = &fixedTimePresigner{next: opts.Presigner, signingTime: signingTime.UTC()}
		}
	})
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return &PresignedRequest{
		URL:     req.URL,
		Method:  req.Method,
		Headers: req.SignedHeader,
	}, nil
}
