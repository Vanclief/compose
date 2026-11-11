package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestObjectBehaviorMemory(t *testing.T) {
	runObjectBehavior(t, NewMemoryClient("memory-bucket"))
}

func TestObjectBehaviorS3(t *testing.T) {
	client, err := newTestClient()
	if err != nil {
		if os.Getenv("COMPOSE_TEST_AWS") != "" {
			t.Fatalf("AWS testing is enabled but the S3 client could not be created: %v", err)
		}

		t.Skipf("AWS credentials are not available: %v", err)
	}

	runObjectBehavior(t, client)
}

// runObjectBehavior checks what an app sees through the public Client methods. It runs against
// the memory client and against real S3, which keeps the memory client honest, so it must not
// reach into the memory store. Keys live under a fresh prefix and are deleted on cleanup because
// the real bucket is shared.
func runObjectBehavior(t *testing.T, client *Client) {
	ctx := context.Background()
	prefix := "compose-test/" + randomHex(t) + "/"

	upload := func(t *testing.T, key, body, contentType string, acl types.ObjectCannedACL) {
		t.Helper()

		input := &s3.PutObjectInput{
			Key:  aws.String(key),
			Body: strings.NewReader(body),
			ACL:  acl,
		}
		if contentType != "" {
			input.ContentType = aws.String(contentType)
		}

		_, err := client.UploadFile(ctx, input)
		require.NoError(t, err)

		t.Cleanup(func() {
			_, err := client.DeleteFile(context.Background(), &s3.DeleteObjectInput{Key: aws.String(key)})
			assert.NoError(t, err)
		})
	}

	t.Run("upload then exists and downloads the same bytes and content type", func(t *testing.T) {
		key := prefix + "upload/file.txt"
		upload(t, key, "The contents of the file.", "text/plain", "")

		exists, err := client.FileExists(ctx, &s3.HeadObjectInput{Key: aws.String(key)})
		require.NoError(t, err)
		require.True(t, exists)

		data, err := client.DownloadBytes(ctx, &s3.GetObjectInput{Key: aws.String(key)})
		require.NoError(t, err)
		require.Equal(t, "The contents of the file.", string(data))

		output, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String(key)})
		require.NoError(t, err)
		defer output.Body.Close()
		require.Equal(t, "text/plain", aws.ToString(output.ContentType))

		body, err := io.ReadAll(output.Body)
		require.NoError(t, err)
		require.Equal(t, "The contents of the file.", string(body))
	})

	t.Run("list returns the prefix's keys in order and MaxKeys truncates", func(t *testing.T) {
		listPrefix := prefix + "list/"
		upload(t, listPrefix+"b", "b", "", "")
		upload(t, listPrefix+"c", "c", "", "")
		upload(t, listPrefix+"a", "a", "", "")

		all, err := client.ListFiles(ctx, &s3.ListObjectsInput{Prefix: aws.String(listPrefix)})
		require.NoError(t, err)
		require.Equal(t, []string{listPrefix + "a", listPrefix + "b", listPrefix + "c"}, objectKeys(all))
		require.False(t, aws.ToBool(all.IsTruncated))

		first, err := client.ListFiles(ctx, &s3.ListObjectsInput{Prefix: aws.String(listPrefix), MaxKeys: aws.Int32(1)})
		require.NoError(t, err)
		require.Equal(t, []string{listPrefix + "a"}, objectKeys(first))
		require.True(t, aws.ToBool(first.IsTruncated))

		next, err := client.ListFiles(ctx, &s3.ListObjectsInput{Prefix: aws.String(listPrefix), Marker: aws.String(listPrefix + "a"), MaxKeys: aws.Int32(1)})
		require.NoError(t, err)
		require.Equal(t, []string{listPrefix + "b"}, objectKeys(next))

		none, err := client.ListFiles(ctx, &s3.ListObjectsInput{Prefix: aws.String(listPrefix), MaxKeys: aws.Int32(0)})
		require.NoError(t, err)
		require.Empty(t, objectKeys(none))
		require.False(t, aws.ToBool(none.IsTruncated))
	})

	t.Run("copy makes an object with the same bytes", func(t *testing.T) {
		source := prefix + "copy/source.txt"
		target := prefix + "copy/target.txt"
		upload(t, source, "copy me", "text/plain", "")

		_, err := client.CopyFile(ctx, &s3.CopyObjectInput{
			CopySource: aws.String(client.Bucket + "/" + source),
			Key:        aws.String(target),
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := client.DeleteFile(context.Background(), &s3.DeleteObjectInput{Key: aws.String(target)})
			assert.NoError(t, err)
		})

		data, err := client.DownloadBytes(ctx, &s3.GetObjectInput{Key: aws.String(target)})
		require.NoError(t, err)
		require.Equal(t, "copy me", string(data))
	})

	t.Run("a copy is private, keeps the content type and metadata, and REPLACE drops them", func(t *testing.T) {
		source := prefix + "copy-meta/source.txt"
		kept := prefix + "copy-meta/kept.txt"
		replaced := prefix + "copy-meta/replaced.txt"

		_, err := client.UploadFile(ctx, &s3.PutObjectInput{
			Key:         aws.String(source),
			Body:        strings.NewReader("copy me"),
			ContentType: aws.String(" text/plain "),
			Metadata:    map[string]string{"Mixed-Case": " value "},
			ACL:         types.ObjectCannedACLPublicRead,
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			for _, key := range []string{source, kept, replaced} {
				_, err := client.DeleteFile(context.Background(), &s3.DeleteObjectInput{Key: aws.String(key)})
				assert.NoError(t, err)
			}
		})

		// Without REPLACE the copy keeps the source's content type and metadata, not the input's
		_, err = client.CopyFile(ctx, &s3.CopyObjectInput{
			CopySource:  aws.String(client.Bucket + "/" + source),
			Key:         aws.String(kept),
			ContentType: aws.String("application/json"),
			Metadata:    map[string]string{"other": "ignored"},
		})
		require.NoError(t, err)

		object, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String(kept)})
		require.NoError(t, err)
		object.Body.Close()
		require.Equal(t, "text/plain", aws.ToString(object.ContentType), "the space around a content type is not stored")
		require.Equal(t, map[string]string{"mixed-case": "value"}, object.Metadata, "metadata keys come back lowercased and values trimmed")
		require.Zero(t, object.LastModified.Nanosecond(), "LastModified holds whole seconds")

		acl, err := client.GetObjectACL(ctx, &s3.GetObjectAclInput{Key: aws.String(kept)})
		require.NoError(t, err)
		require.False(t, grantsPublicRead(acl.Grants), "a copy does not carry the source's ACL")

		_, err = client.CopyFile(ctx, &s3.CopyObjectInput{
			CopySource:        aws.String(client.Bucket + "/" + source),
			Key:               aws.String(replaced),
			MetadataDirective: types.MetadataDirectiveReplace,
		})
		require.NoError(t, err)

		object, err = client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String(replaced)})
		require.NoError(t, err)
		object.Body.Close()
		require.Equal(t, "binary/octet-stream", aws.ToString(object.ContentType))
		require.Empty(t, object.Metadata)
	})

	t.Run("an upload that names no content type is application/octet-stream", func(t *testing.T) {
		key := prefix + "content-type/file"
		upload(t, key, "data", "", "")

		object, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String(key)})
		require.NoError(t, err)
		object.Body.Close()
		require.Equal(t, "application/octet-stream", aws.ToString(object.ContentType))
		require.Nil(t, object.Metadata, "an upload with no metadata has none")
	})

	t.Run("delete then the object no longer exists", func(t *testing.T) {
		key := prefix + "delete/file.txt"
		upload(t, key, "delete me", "", "")

		_, err := client.DeleteFile(ctx, &s3.DeleteObjectInput{Key: aws.String(key)})
		require.NoError(t, err)

		exists, err := client.FileExists(ctx, &s3.HeadObjectInput{Key: aws.String(key)})
		require.NoError(t, err)
		require.False(t, exists)
	})

	t.Run("a missing key", func(t *testing.T) {
		key := prefix + "missing/file.txt"

		_, err := client.DeleteFile(ctx, &s3.DeleteObjectInput{Key: aws.String(key)})
		require.NoError(t, err, "deleting a missing key is not an error")

		exists, err := client.FileExists(ctx, &s3.HeadObjectInput{Key: aws.String(key)})
		require.NoError(t, err)
		require.False(t, exists)

		_, err = client.DownloadBytes(ctx, &s3.GetObjectInput{Key: aws.String(key)})
		require.Error(t, err)
		var apiErr smithy.APIError
		require.True(t, errors.As(err, &apiErr), "got %v", err)
		require.Equal(t, "NoSuchKey", apiErr.ErrorCode())
		var noSuchKey *types.NoSuchKey
		require.True(t, errors.As(err, &noSuchKey), "got %v", err)
		var responseErr *awshttp.ResponseError
		require.True(t, errors.As(err, &responseErr), "got %v", err)
		require.Equal(t, 404, responseErr.HTTPStatusCode())
		var s3Err s3.ResponseError
		require.True(t, errors.As(err, &s3Err), "got %v", err)

		_, err = client.GetObjectACL(ctx, &s3.GetObjectAclInput{Key: aws.String(key)})
		noSuchKey = nil
		require.True(t, errors.As(err, &noSuchKey), "got %v", err)

		_, err = client.SetObjectACL(ctx, &s3.PutObjectAclInput{Key: aws.String(key), ACL: types.ObjectCannedACLPrivate})
		noSuchKey = nil
		require.True(t, errors.As(err, &noSuchKey), "got %v", err)
	})

	t.Run("a copy from a missing source is NoSuchKey with status 404", func(t *testing.T) {
		_, err := client.CopyFile(ctx, &s3.CopyObjectInput{
			CopySource: aws.String(client.Bucket + "/" + prefix + "missing/source.txt"),
			Key:        aws.String(prefix + "missing/target.txt"),
		})
		require.Error(t, err)
		var apiErr smithy.APIError
		require.True(t, errors.As(err, &apiErr), "got %v", err)
		require.Equal(t, "NoSuchKey", apiErr.ErrorCode())
		var responseErr *awshttp.ResponseError
		require.True(t, errors.As(err, &responseErr), "got %v", err)
		require.Equal(t, 404, responseErr.HTTPStatusCode())
		var s3Err s3.ResponseError
		require.True(t, errors.As(err, &s3Err), "got %v", err)
	})

	t.Run("a copy onto itself without REPLACE and an ACL write with no ACL are bad requests", func(t *testing.T) {
		key := prefix + "bad-request/file.txt"
		upload(t, key, "data", "", "")

		// The error code differs by provider, so only the status is asserted
		_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String(client.Bucket + "/" + key), Key: aws.String(key)})
		require.Error(t, err)
		var responseErr *awshttp.ResponseError
		require.True(t, errors.As(err, &responseErr), "got %v", err)
		require.Equal(t, 400, responseErr.HTTPStatusCode())

		_, err = client.SetObjectACL(ctx, &s3.PutObjectAclInput{Key: aws.String(key)})
		require.Error(t, err)
		responseErr = nil
		require.True(t, errors.As(err, &responseErr), "got %v", err)
		require.Equal(t, 400, responseErr.HTTPStatusCode())
	})

	t.Run("public-read upload made private", func(t *testing.T) {
		key := prefix + "acl/file.txt"
		upload(t, key, "public for now", "text/plain", types.ObjectCannedACLPublicRead)

		acl, err := client.GetObjectACL(ctx, &s3.GetObjectAclInput{Key: aws.String(key)})
		require.NoError(t, err)
		require.True(t, grantsPublicRead(acl.Grants), "public-read grants AllUsers READ")

		_, err = client.SetObjectACL(ctx, &s3.PutObjectAclInput{Key: aws.String(key), ACL: types.ObjectCannedACLPrivate})
		require.NoError(t, err)

		acl, err = client.GetObjectACL(ctx, &s3.GetObjectAclInput{Key: aws.String(key)})
		require.NoError(t, err)
		require.False(t, grantsPublicRead(acl.Grants), "private grants AllUsers nothing")
	})
}

func randomHex(t *testing.T) string {
	t.Helper()

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(t, err)

	return hex.EncodeToString(buf)
}

func objectKeys(output *s3.ListObjectsOutput) []string {
	keys := []string{}
	for _, object := range output.Contents {
		keys = append(keys, aws.ToString(object.Key))
	}

	return keys
}

func grantsPublicRead(grants []types.Grant) bool {
	for _, grant := range grants {
		if grant.Grantee == nil || grant.Permission != types.PermissionRead {
			continue
		}
		if grant.Grantee.Type == types.TypeGroup && aws.ToString(grant.Grantee.URI) == allUsersGroupURI {
			return true
		}
	}

	return false
}

func TestMemoryClientRejectsWhatItDoesNotModel(t *testing.T) {
	ctx := context.Background()
	client := NewMemoryClient("memory-bucket")

	_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("file.txt"), Body: strings.NewReader("data")})
	require.NoError(t, err)

	// field is the input field the error must name, empty for a value the store does not model.
	cases := []struct {
		name  string
		field string
		call  func() error
	}{
		{"ranged download", "Range", func() error {
			_, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String("file.txt"), Range: aws.String("bytes=0-1")})
			return err
		}},
		{"list with a delimiter", "Delimiter", func() error {
			_, err := client.ListFiles(ctx, &s3.ListObjectsInput{Delimiter: aws.String("/")})
			return err
		}},
		{"list with an encoding type", "EncodingType", func() error {
			_, err := client.ListFiles(ctx, &s3.ListObjectsInput{EncodingType: types.EncodingTypeUrl})
			return err
		}},
		{"upload with another canned ACL", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), ACL: types.ObjectCannedACLPublicReadWrite})
			return err
		}},
		{"upload with a grant header", "GrantRead", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), GrantRead: aws.String(`uri="` + allUsersGroupURI + `"`)})
			return err
		}},
		{"upload with IfNoneMatch", "IfNoneMatch", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), IfNoneMatch: aws.String("*")})
			return err
		}},
		{"upload with ContentDisposition", "ContentDisposition", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), ContentDisposition: aws.String("attachment")})
			return err
		}},
		{"upload with CacheControl", "CacheControl", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), CacheControl: aws.String("no-cache")})
			return err
		}},
		{"upload with ServerSideEncryption", "ServerSideEncryption", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), ServerSideEncryption: types.ServerSideEncryptionAes256})
			return err
		}},
		{"download with IfMatch", "IfMatch", func() error {
			_, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String("file.txt"), IfMatch: aws.String(`"etag"`)})
			return err
		}},
		{"download with VersionId", "VersionId", func() error {
			_, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String("file.txt"), VersionId: aws.String("1")})
			return err
		}},
		{"download with ResponseContentType", "ResponseContentType", func() error {
			_, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String("file.txt"), ResponseContentType: aws.String("text/plain")})
			return err
		}},
		{"exists with Range", "Range", func() error {
			_, err := client.FileExists(ctx, &s3.HeadObjectInput{Key: aws.String("file.txt"), Range: aws.String("bytes=0-1")})
			return err
		}},
		{"set another canned ACL", "", func() error {
			_, err := client.SetObjectACL(ctx, &s3.PutObjectAclInput{Key: aws.String("file.txt"), ACL: types.ObjectCannedACLAuthenticatedRead})
			return err
		}},
		{"set an access control policy", "AccessControlPolicy", func() error {
			_, err := client.SetObjectACL(ctx, &s3.PutObjectAclInput{Key: aws.String("file.txt"), AccessControlPolicy: &types.AccessControlPolicy{}})
			return err
		}},
		{"copy from another bucket", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("other-bucket/file.txt"), Key: aws.String("copy.txt")})
			return err
		}},
		{"copy a version", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/file.txt?versionId=1"), Key: aws.String("copy.txt")})
			return err
		}},
		{"list with a negative MaxKeys", "", func() error {
			_, err := client.ListFiles(ctx, &s3.ListObjectsInput{MaxKeys: aws.Int32(-1)})
			return err
		}},
		{"copy with an unknown MetadataDirective", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/file.txt"), Key: aws.String("copy.txt"), MetadataDirective: "MERGE"})
			return err
		}},
		{"copy from a source with no key", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket"), Key: aws.String("copy.txt")})
			return err
		}},
		{"copy from a source with bad URL encoding", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/%zz"), Key: aws.String("copy.txt")})
			return err
		}},
		{"upload of a Body that cannot seek, with no ContentLength", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Body: io.NopCloser(strings.NewReader("data"))})
			return err
		}},
		{"upload to a client with an empty bucket", "", func() error {
			_, err := NewMemoryClient("").UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Body: strings.NewReader("data")})
			return err
		}},
		{"upload with a metadata key that is not a header token", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Metadata: map[string]string{"my key": "value"}})
			return err
		}},
		{"upload with a line break in a metadata value", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Metadata: map[string]string{"key": "a\nb"}})
			return err
		}},
		{"upload with a metadata value outside ASCII", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Metadata: map[string]string{"key": "café"}})
			return err
		}},
		{"upload with more than 2 KB of metadata", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Metadata: map[string]string{"key": strings.Repeat("x", 2048)}})
			return err
		}},
		{"upload with a line break in the content type", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), ContentType: aws.String("text/plain\r\nX: y")})
			return err
		}},
		{"copy with a metadata value outside ASCII", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/file.txt"), Key: aws.String("copy.txt"), Metadata: map[string]string{"key": "café"}})
			return err
		}},
		{"upload with a ContentType that is set but empty", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), ContentType: aws.String("")})
			return err
		}},
		{"REPLACE copy with a ContentType that is set but empty", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/file.txt"), Key: aws.String("copy.txt"), MetadataDirective: types.MetadataDirectiveReplace, ContentType: aws.String("")})
			return err
		}},
		{"upload with an empty metadata value", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Metadata: map[string]string{"key": ""}})
			return err
		}},
		{"upload under a key over 1024 bytes", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String(strings.Repeat("k", 1025))})
			return err
		}},
		{"upload under a key that is not UTF-8", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("bad\xff")})
			return err
		}},
		{"list with a MaxKeys over 1000", "", func() error {
			_, err := client.ListFiles(ctx, &s3.ListObjectsInput{MaxKeys: aws.Int32(1001)})
			return err
		}},
		{"upload under a key with a control character", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("bad\x01key")})
			return err
		}},
		{"upload under a key with a .. segment", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("dir/../file.txt")})
			return err
		}},
		{"upload under a key with a . segment", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("./file.txt")})
			return err
		}},
		{"copy from a source with a space", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/a b.txt"), Key: aws.String("copy.txt")})
			return err
		}},
		{"copy from a source that is not ASCII", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/año.txt"), Key: aws.String("copy.txt")})
			return err
		}},
		{"copy from a source with a +", "", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/a+b.txt"), Key: aws.String("copy.txt")})
			return err
		}},
		{"upload of a Body that fails to read", "", func() error {
			_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Body: iotest.ErrReader(errors.New("read failed")), ContentLength: aws.Int64(4)})
			return err
		}},
		{"upload to another bucket", "", func() error {
			other := *client
			other.Bucket = "other-bucket"
			_, err := other.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Body: strings.NewReader("data")})
			return err
		}},
		{"download from another bucket", "", func() error {
			other := *client
			other.Bucket = "other-bucket"
			_, err := other.DownloadBytes(ctx, &s3.GetObjectInput{Key: aws.String("file.txt")})
			return err
		}},
		{"copy with ContentDisposition", "ContentDisposition", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/file.txt"), Key: aws.String("copy.txt"), ContentDisposition: aws.String("attachment")})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.Error(t, err)
			require.Equal(t, ez.ENOTIMPLEMENTED, ez.ErrorCode(err), "got %v", err)
			if tc.field != "" {
				require.Contains(t, err.Error(), tc.field)
			}
		})
	}

	exists, err := client.FileExists(ctx, &s3.HeadObjectInput{Key: aws.String("other.txt")})
	require.NoError(t, err)
	require.False(t, exists, "a rejected upload stores nothing")
}

func TestMemoryClientCopyMetadataDirective(t *testing.T) {
	ctx := context.Background()
	client := NewMemoryClient("memory-bucket")

	_, err := client.UploadFile(ctx, &s3.PutObjectInput{
		Key:         aws.String("dir/source name.txt"),
		Body:        strings.NewReader("data"),
		ContentType: aws.String("text/plain"),
		Metadata:    map[string]string{"Owner": "a"},
		ACL:         types.ObjectCannedACLPublicRead,
	})
	require.NoError(t, err)

	_, err = client.CopyFile(ctx, &s3.CopyObjectInput{
		CopySource: aws.String("/memory-bucket/" + url.PathEscape("dir/source name.txt")),
		Key:        aws.String("kept.txt"),
	})
	require.NoError(t, err)

	kept, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String("kept.txt")})
	require.NoError(t, err)
	require.Equal(t, "text/plain", aws.ToString(kept.ContentType))
	require.Equal(t, map[string]string{"owner": "a"}, kept.Metadata, "S3 returns metadata keys lowercased")

	acl, err := client.GetObjectACL(ctx, &s3.GetObjectAclInput{Key: aws.String("kept.txt")})
	require.NoError(t, err)
	require.False(t, grantsPublicRead(acl.Grants), "a copy does not inherit the source's ACL")

	_, err = client.CopyFile(ctx, &s3.CopyObjectInput{
		CopySource:        aws.String("memory-bucket/dir/source%20name.txt"),
		Key:               aws.String("replaced.txt"),
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       aws.String("application/json"),
		Metadata:          map[string]string{"kind": "b"},
	})
	require.NoError(t, err)

	replaced, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String("replaced.txt")})
	require.NoError(t, err)
	require.Equal(t, "application/json", aws.ToString(replaced.ContentType))
	require.Equal(t, map[string]string{"kind": "b"}, replaced.Metadata)

	// The SDK's CopyObject deserializer does not type NoSuchKey, so a missing source is generic.
	_, err = client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/missing.txt"), Key: aws.String("copy.txt")})
	var genericErr *smithy.GenericAPIError
	require.True(t, errors.As(err, &genericErr), "got %v", err)
	require.Equal(t, "NoSuchKey", genericErr.ErrorCode())
	var noSuchKey *types.NoSuchKey
	require.False(t, errors.As(err, &noSuchKey), "got %v", err)
}

func TestMemoryClientAnswersInvalidRequestsAsS3(t *testing.T) {
	ctx := context.Background()
	client := NewMemoryClient("memory-bucket")

	_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("file.txt"), Body: strings.NewReader("data")})
	require.NoError(t, err)

	cases := []struct {
		name string
		code string
		call func() error
	}{
		{"set an ACL with no ACL", "InvalidArgument", func() error {
			_, err := client.SetObjectACL(ctx, &s3.PutObjectAclInput{Key: aws.String("file.txt")})
			return err
		}},
		{"copy onto itself without REPLACE", "InvalidRequest", func() error {
			_, err := client.CopyFile(ctx, &s3.CopyObjectInput{CopySource: aws.String("memory-bucket/file.txt"), Key: aws.String("file.txt")})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			require.Error(t, err)
			require.NotEqual(t, ez.EINVALID, ez.ErrorCode(err))
			var apiErr smithy.APIError
			require.True(t, errors.As(err, &apiErr), "got %v", err)
			require.Equal(t, tc.code, apiErr.ErrorCode())
			var responseErr *awshttp.ResponseError
			require.True(t, errors.As(err, &responseErr), "got %v", err)
			require.Equal(t, 400, responseErr.HTTPStatusCode())
		})
	}

	t.Run("upload with no Key", func(t *testing.T) {
		_, err := client.UploadFile(ctx, &s3.PutObjectInput{Body: strings.NewReader("data")})
		require.Error(t, err)
		require.NotEqual(t, ez.EINVALID, ez.ErrorCode(err))
		var invalidParams smithy.InvalidParamsError
		require.True(t, errors.As(err, &invalidParams), "got %v", err)
		var operationErr *smithy.OperationError
		require.True(t, errors.As(err, &operationErr), "got %v", err)
		require.Equal(t, "PutObject", operationErr.Operation())
	})

	t.Run("upload with an empty Key", func(t *testing.T) {
		_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String(""), Body: strings.NewReader("data")})
		require.Error(t, err)
		require.NotEqual(t, ez.EINVALID, ez.ErrorCode(err))
		var serializationErr *smithy.SerializationError
		require.True(t, errors.As(err, &serializationErr), "got %v", err)
		require.Contains(t, err.Error(), "serialization failed: serialization failed: input member Key must not be empty")
	})
}

func TestMemoryClientChecksContentLength(t *testing.T) {
	ctx := context.Background()
	client := NewMemoryClient("memory-bucket")

	_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("wrong.txt"), Body: strings.NewReader("data"), ContentLength: aws.Int64(5)})
	require.Error(t, err)
	require.Equal(t, ez.ENOTIMPLEMENTED, ez.ErrorCode(err), "got %v", err)

	exists, err := client.FileExists(ctx, &s3.HeadObjectInput{Key: aws.String("wrong.txt")})
	require.NoError(t, err)
	require.False(t, exists, "a failed upload stores nothing")

	_, err = client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("right.txt"), Body: strings.NewReader("data"), ContentLength: aws.Int64(4)})
	require.NoError(t, err)

	data, err := client.DownloadBytes(ctx, &s3.GetObjectInput{Key: aws.String("right.txt")})
	require.NoError(t, err)
	require.Equal(t, "data", string(data))
}

func TestMemoryClientUploadsABodyThatCannotSeekWhenItsLengthIsGiven(t *testing.T) {
	ctx := context.Background()
	client := NewMemoryClient("memory-bucket")

	_, err := client.UploadFile(ctx, &s3.PutObjectInput{
		Key:           aws.String("file.txt"),
		Body:          io.NopCloser(strings.NewReader("data")),
		ContentLength: aws.Int64(4),
	})
	require.NoError(t, err)

	data, err := client.DownloadBytes(ctx, &s3.GetObjectInput{Key: aws.String("file.txt")})
	require.NoError(t, err)
	require.Equal(t, "data", string(data))
}

func TestMemoryClientReturnsNoMetadataForAnEmptyMap(t *testing.T) {
	ctx := context.Background()
	client := NewMemoryClient("memory-bucket")

	_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("file.txt"), Body: strings.NewReader("data"), Metadata: map[string]string{}})
	require.NoError(t, err)

	object, err := client.DownloadFile(ctx, &s3.GetObjectInput{Key: aws.String("file.txt")})
	require.NoError(t, err)
	object.Body.Close()
	require.Nil(t, object.Metadata)
}

func TestMemoryClientUploadsABodyThatKnowsItsLength(t *testing.T) {
	ctx := context.Background()
	client := NewMemoryClient("memory-bucket")

	// A *bytes.Buffer cannot seek, and the SDK takes its length from Len
	_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("file.txt"), Body: bytes.NewBufferString("data")})
	require.NoError(t, err)

	data, err := client.DownloadBytes(ctx, &s3.GetObjectInput{Key: aws.String("file.txt")})
	require.NoError(t, err)
	require.Equal(t, "data", string(data))
}

func TestMemoryClientCancelledContext(t *testing.T) {
	client := NewMemoryClient("memory-bucket")

	_, err := client.UploadFile(context.Background(), &s3.PutObjectInput{Key: aws.String("file.txt"), Body: strings.NewReader("data")})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String("other.txt"), Body: strings.NewReader("data")})
	require.True(t, errors.Is(err, context.Canceled), "upload: got %v", err)
	var operationErr *smithy.OperationError
	require.True(t, errors.As(err, &operationErr), "the SDK wraps a context error in the operation's: got %v", err)
	require.Equal(t, "PutObject", operationErr.Operation())

	_, err = client.DownloadBytes(ctx, &s3.GetObjectInput{Key: aws.String("file.txt")})
	require.True(t, errors.Is(err, context.Canceled), "download: got %v", err)

	_, err = client.ListFiles(ctx, &s3.ListObjectsInput{})
	require.True(t, errors.Is(err, context.Canceled), "list: got %v", err)

	_, err = client.DeleteFile(ctx, &s3.DeleteObjectInput{Key: aws.String("file.txt")})
	require.True(t, errors.Is(err, context.Canceled), "delete: got %v", err)

	exists, err := client.FileExists(context.Background(), &s3.HeadObjectInput{Key: aws.String("file.txt")})
	require.NoError(t, err)
	require.True(t, exists, "a cancelled delete deletes nothing")
}

func TestMemoryClientPresignsOffline(t *testing.T) {
	ctx := context.Background()
	client := NewMemoryClient("memory-bucket")

	requireOfflineURL := func(t *testing.T, raw string) {
		t.Helper()

		parsed, err := url.Parse(raw)
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(parsed.Hostname(), ".invalid"), "host %q is on the reserved .invalid domain", parsed.Hostname())
	}

	privateURL, err := client.GetPrivateURL(ctx, &s3.GetObjectInput{Key: aws.String("file.txt")})
	require.NoError(t, err)
	requireOfflineURL(t, privateURL)
	parsed, err := url.Parse(privateURL)
	require.NoError(t, err)
	require.NotEmpty(t, parsed.Query().Get("X-Amz-Signature"))

	put, err := client.PresignPutObject(ctx, &s3.PutObjectInput{Key: aws.String("file.txt")}, time.Minute)
	require.NoError(t, err)
	requireOfflineURL(t, put.URL)
	require.Equal(t, "PUT", put.Method)

	post, err := client.PresignPostObject(ctx, &s3.PutObjectInput{Key: aws.String("file.txt")}, time.Minute)
	require.NoError(t, err)
	requireOfflineURL(t, post.URL)
	require.NotEmpty(t, post.Fields)

	get, err := client.PresignGetObject(ctx, &s3.GetObjectInput{Key: aws.String("file.txt")}, time.Now().UTC().Truncate(time.Hour), 24*time.Hour)
	require.NoError(t, err)
	requireOfflineURL(t, get.URL)
	require.Equal(t, "GET", get.Method)

	// A directory bucket name, for which the SDK would otherwise ask S3 for a session first
	express := NewMemoryClient("memory-bucket--usw2-az1--x-s3")
	expressURL, err := express.GetPrivateURL(ctx, &s3.GetObjectInput{Key: aws.String("file.txt")})
	require.NoError(t, err)
	requireOfflineURL(t, expressURL)
}

func TestMemoryClientConcurrentUse(t *testing.T) {
	ctx := context.Background()
	client := NewMemoryClient("memory-bucket")

	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for i := range 25 {
				key := fmt.Sprintf("worker-%d/file-%d.txt", worker, i)
				body := fmt.Sprintf("worker %d file %d", worker, i)

				_, err := client.UploadFile(ctx, &s3.PutObjectInput{Key: aws.String(key), Body: strings.NewReader(body)})
				if !assert.NoError(t, err) {
					return
				}

				data, err := client.DownloadBytes(ctx, &s3.GetObjectInput{Key: aws.String(key)})
				if !assert.NoError(t, err) {
					return
				}
				assert.Equal(t, body, string(data))

				_, err = client.ListFiles(ctx, &s3.ListObjectsInput{Prefix: aws.String("worker-")})
				assert.NoError(t, err)

				_, err = client.SetObjectACL(ctx, &s3.PutObjectAclInput{Key: aws.String(key), ACL: types.ObjectCannedACLPublicRead})
				assert.NoError(t, err)
			}
		}()
	}
	wg.Wait()

	all, err := client.ListFiles(ctx, &s3.ListObjectsInput{})
	require.NoError(t, err)
	require.Len(t, all.Contents, 8*25)
}

func TestNewMemoryClientOptions(t *testing.T) {
	client := NewMemoryClient("memory-bucket",
		WithDigitalOceanEndpoint("sfo3", "digitaloceanspaces.com"),
		WithPublicBaseURL("https://cdn.example.com"),
	)

	require.Equal(t, "memory-bucket", client.Bucket)
	require.Equal(t, "us-east-1", client.Region)
	require.Equal(t, "https://cdn.example.com", client.PublicURL)

	buckets, err := client.ListBuckets(context.Background())
	require.NoError(t, err)
	require.Len(t, buckets, 1)
	require.Equal(t, "memory-bucket", aws.ToString(buckets[0].Name))
}
