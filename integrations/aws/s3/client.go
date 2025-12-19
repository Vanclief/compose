package s3

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/vanclief/ez"
)

// Client contains the DO configuration and methods
type Client struct {
	Region          string
	Bucket          string
	AccessKeyID     string
	secretAccessKey string
	s3              *s3.Client
	presign         *s3.PresignClient
	URL             string
}

func NewClient(ctx context.Context, url, region, accessKey, secretKey, bucket string) (*Client, error) {
	const op = "s3.NewClient"

	if region == "" {
		return nil, ez.New(op, ez.EINVALID, "Region cannot be empty", nil)
	} else if accessKey == "" {
		return nil, ez.New(op, ez.EINVALID, "AccessKey cannot be empty", nil)
	} else if secretKey == "" {
		return nil, ez.New(op, ez.EINVALID, "SecretKey cannot be empty", nil)
	}

	endpoint := fmt.Sprintf("https://%s.%s", region, url)

	awsCfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})

	client := &Client{
		Region:          region,
		AccessKeyID:     accessKey,
		secretAccessKey: secretKey,
		s3:              s3Client,
		presign:         s3.NewPresignClient(s3Client),
		Bucket:          bucket,
		URL:             fmt.Sprintf("https://%s.%s.cdn.%s", bucket, region, url),
	}

	return client, nil
}
