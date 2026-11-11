package s3

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/vanclief/ez"
)

// objectAPI is the part of the S3 API the Client calls. The SDK's *s3.Client implements it, and
// so does the in-memory store behind NewMemoryClient.
type objectAPI interface {
	ListObjects(ctx context.Context, input *s3.ListObjectsInput, optFns ...func(*s3.Options)) (*s3.ListObjectsOutput, error)
	PutObject(ctx context.Context, input *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	CopyObject(ctx context.Context, input *s3.CopyObjectInput, optFns ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
	DeleteObject(ctx context.Context, input *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	HeadObject(ctx context.Context, input *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	GetObject(ctx context.Context, input *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObjectAcl(ctx context.Context, input *s3.PutObjectAclInput, optFns ...func(*s3.Options)) (*s3.PutObjectAclOutput, error)
	GetObjectAcl(ctx context.Context, input *s3.GetObjectAclInput, optFns ...func(*s3.Options)) (*s3.GetObjectAclOutput, error)
	ListBuckets(ctx context.Context, input *s3.ListBucketsInput, optFns ...func(*s3.Options)) (*s3.ListBucketsOutput, error)
}

var _ objectAPI = (*s3.Client)(nil)

// Client contains the DO configuration and methods
type Client struct {
	Region    string
	Bucket    string
	s3        objectAPI
	presign   *s3.PresignClient
	PublicURL string
}

func NewClient(ctx context.Context, region, accessKey, secretKey, bucket string, opts ...ClientOption) (*Client, error) {
	if region == "" {
		return nil, ez.New(ez.EINVALID, "Region cannot be empty", nil)
	} else if accessKey == "" {
		return nil, ez.New(ez.EINVALID, "AccessKey cannot be empty", nil)
	} else if secretKey == "" {
		return nil, ez.New(ez.EINVALID, "SecretKey cannot be empty", nil)
	}

	awsCfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	)
	if err != nil {
		return nil, ez.Wrap(err)
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
