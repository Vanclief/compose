package s3

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/vanclief/ez"
)

// Client contains the DO configuration and methods
type Client struct {
	Region    string
	Bucket    string
	s3        *s3.Client
	presign   *s3.PresignClient
	PublicURL string
}

func NewClient(ctx context.Context, region, accessKey, secretKey, bucket string, opts ...ClientOption) (*Client, error) {
	const op = "s3.NewClient"

	if region == "" {
		return nil, ez.New(op, ez.EINVALID, "Region cannot be empty", nil)
	} else if accessKey == "" {
		return nil, ez.New(op, ez.EINVALID, "AccessKey cannot be empty", nil)
	} else if secretKey == "" {
		return nil, ez.New(op, ez.EINVALID, "SecretKey cannot be empty", nil)
	}

	awsCfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	// Apply opts
	co := &clientOptions{}
	for _, opt := range opts {
		opt(co)
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if co.baseEndpoint != "" {
			o.BaseEndpoint = aws.String(co.baseEndpoint)
		}
	})

	client := &Client{
		Region:    region,
		s3:        s3Client,
		presign:   s3.NewPresignClient(s3Client),
		Bucket:    bucket,
		PublicURL: co.publicBaseURL,
	}

	return client, nil
}
