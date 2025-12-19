package s3

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/vanclief/ez"
)

func (c *Client) ListFiles(ctx context.Context, input *s3.ListObjectsInput) (*s3.ListObjectsOutput, error) {
	const op = "Client.ListFiles"

	if input == nil {
		return nil, ez.New(op, ez.EINVALID, "input is required", nil)
	}

	input.Bucket = aws.String(c.Bucket)

	objects, err := c.s3.ListObjects(ctx, input)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return objects, nil
}

func (c *Client) UploadFile(ctx context.Context, input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	const op = "Client.UploadFile"

	if input == nil {
		return nil, ez.New(op, ez.EINVALID, "input is required", nil)
	}

	input.Bucket = aws.String(c.Bucket)

	res, err := c.s3.PutObject(ctx, input)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return res, nil
}

func (c *Client) CopyFile(ctx context.Context, input *s3.CopyObjectInput) (*s3.CopyObjectOutput, error) {
	const op = "Client.CopyFile"

	if input == nil {
		return nil, ez.New(op, ez.EINVALID, "input is required", nil)
	}

	input.Bucket = aws.String(c.Bucket)
	if input.CopySource == nil || *input.CopySource == "" {
		return nil, ez.New(op, ez.EINVALID, "CopySource is required", nil)
	}

	// Keeps existing metadata
	if input.MetadataDirective == "" {
		input.MetadataDirective = types.MetadataDirectiveCopy
	}

	res, err := c.s3.CopyObject(ctx, input)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return res, nil
}

func (c *Client) DeleteFile(ctx context.Context, input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	const op = "Client.DeleteFile"

	if input == nil {
		return nil, ez.New(op, ez.EINVALID, "input is required", nil)
	}

	input.Bucket = aws.String(c.Bucket)

	result, err := c.s3.DeleteObject(ctx, input)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return result, nil
}

func (c *Client) FileExists(ctx context.Context, input *s3.HeadObjectInput) (bool, error) {
	const op = "Client.FileExists"

	if input == nil {
		return false, ez.New(op, ez.EINVALID, "input is required", nil)
	}

	input.Bucket = aws.String(c.Bucket)

	_, err := c.s3.HeadObject(ctx, input)
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return false, nil
		}
		return false, ez.Wrap(op, err)
	}

	return true, nil
}

func (c *Client) GetPrivateURL(ctx context.Context, input *s3.GetObjectInput) (string, error) {
	const op = "Client.GetPrivateURL"

	if input == nil {
		return "", ez.New(op, ez.EINVALID, "input is required", nil)
	}

	input.Bucket = aws.String(c.Bucket)

	req, err := c.presign.PresignGetObject(ctx, input, func(opts *s3.PresignOptions) {
		opts.Expires = 1440 * time.Minute
	})
	if err != nil {
		return "", ez.Wrap(op, err)
	}

	return req.URL, nil
}
