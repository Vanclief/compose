package s3

import "fmt"

type clientOptions struct {
	baseEndpoint string

	publicBaseURL string
}

// ClientOption defines function type for client options.
type ClientOption func(*clientOptions)

// WithDigitalOceanEndpoint sets the S3 base endpoint for DigitalOcean Spaces
func WithDigitalOceanEndpoint(region, hostSuffix string) ClientOption {
	endpoint := fmt.Sprintf("https://%s.%s", region, hostSuffix)

	return func(o *clientOptions) {
		o.baseEndpoint = endpoint
	}
}

// WithDigitalOceanCDN sets the S3 base endpoint for DigitalOcean Spaces
func WithDigitalOceanCDN(bucket, region, url string) ClientOption {
	publicBaseURL := fmt.Sprintf("https://%s.%s.cdn.%s", bucket, region, url)

	return WithPublicBaseURL(publicBaseURL)
}

// WithPublicBaseURL overrides the public base URL stored in Client.URL.
// This is typically a CDN or custom domain (e.g. DO Spaces CDN, CloudFront).
func WithPublicBaseURL(publicBaseURL string) ClientOption {
	return func(o *clientOptions) {
		o.publicBaseURL = publicBaseURL
	}
}
