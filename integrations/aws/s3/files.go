package s3

import (
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/vanclief/ez"
)

func (c *Client) ListFiles(input *s3.ListObjectsInput) (*s3.ListObjectsOutput, error) {
	const op = "Client.ListFiles"

	input.Bucket = aws.String(c.Bucket)

	objects, err := c.s3.ListObjects(input)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return objects, nil
}

func (c *Client) UploadFile(input *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	const op = "Client.UploadFile"

	input.Bucket = aws.String(c.Bucket)

	res, err := c.s3.PutObject(input)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return res, nil
}

func (c *Client) CopyFile(input *s3.CopyObjectInput) (*s3.CopyObjectOutput, error) {
	const op = "Client.CopyFile"

	input.Bucket = aws.String(c.Bucket)
	if input.CopySource == nil || *input.CopySource == "" {
		return nil, ez.New(op, ez.EINVALID, "CopySource is required", nil)
	}

	// Keeps existing metadata
	if input.MetadataDirective == nil {
		input.MetadataDirective = aws.String(s3.MetadataDirectiveCopy)
	}

	res, err := c.s3.CopyObject(input)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return res, nil
}

func (c *Client) DeleteFile(input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	const op = "Client.DeleteFile"

	input.Bucket = aws.String(c.Bucket)

	result, err := c.s3.DeleteObject(input)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return result, nil
}

func (c *Client) FileExists(input *s3.HeadObjectInput) (bool, error) {
	const op = "Client.FileExists"

	input.Bucket = aws.String(c.Bucket)

	_, err := c.s3.HeadObject(input)
	if err != nil {
		awsErr, ok := err.(awserr.Error)
		if ok {
			if awsErr.Code() == s3.ErrCodeNoSuchKey || awsErr.Code() == "NotFound" {
				return false, nil
			}
		}
		return false, ez.Wrap(op, err)
	}

	return true, nil
}

func (c *Client) GetPrivateURL(input *s3.GetObjectInput) (string, error) {
	const op = "Client.GetPrivateURL"

	input.Bucket = aws.String(c.Bucket)

	req, _ := c.s3.GetObjectRequest(input)

	urlStr, err := req.Presign(1440 * time.Minute)
	if err != nil {
		return "", ez.Wrap(op, err)
	}

	return urlStr, nil
}
