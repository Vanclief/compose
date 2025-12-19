package s3

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/vanclief/ez"
)

func (c *Client) ListBuckets(ctx context.Context) ([]types.Bucket, error) {
	const op = "Client.ListBuckets"

	spaces, err := c.s3.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return spaces.Buckets, nil
}
