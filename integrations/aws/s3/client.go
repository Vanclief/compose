package s3

import (
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/vanclief/ez"
)

// Client contains the DO configuration and methods
type Client struct {
	Region          string
	Bucket          string
	AccessKeyID     string
	secretAccessKey string
	s3              *s3.S3
	URL             string
}

func NewClient(url, region, accessKey, secretKey, bucket string) (*Client, error) {
	const op = "s3.NewClient"

	if region == "" {
		return nil, ez.New(op, ez.EINVALID, "Region cannot be empty", nil)
	} else if accessKey == "" {
		return nil, ez.New(op, ez.EINVALID, "AccessKey cannot be empty", nil)
	} else if secretKey == "" {
		return nil, ez.New(op, ez.EINVALID, "SecretKey cannot be empty", nil)
	}

	endpoint := fmt.Sprintf("https://%s.%s", region, url)

	s3Config := &aws.Config{
		Credentials: credentials.NewStaticCredentials(accessKey, secretKey, ""),
		Endpoint:    &endpoint,
		Region:      aws.String(region),
	}

	newSession, err := session.NewSession(s3Config)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	s3Client := s3.New(newSession)

	client := &Client{
		Region:          region,
		AccessKeyID:     accessKey,
		secretAccessKey: secretKey,
		s3:              s3Client,
		Bucket:          bucket,
		URL:             fmt.Sprintf("https://%s.%s.cdn.%s", bucket, region, url),
	}

	return client, nil
}
