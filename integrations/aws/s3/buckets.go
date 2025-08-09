package s3

import (
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/vanclief/ez"
)

func (c *Client) ListBuckets() ([]*s3.Bucket, error) {
	const op = "Client.ListBuckets"

	spaces, err := c.s3.ListBuckets(nil)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return spaces.Buckets, nil
}
