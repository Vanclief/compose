package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/vanclief/ez"
)

const (
	memoryRegion  = "us-east-1"
	memoryOwnerID = "memory-owner"
	// memoryMaxKeys is the most keys one S3 listing returns, and its default.
	memoryMaxKeys = 1000
	// defaultContentType is what the SDK sends when an upload names no content type.
	defaultContentType = "application/octet-stream"
	// copyDefaultContentType is what S3 stores for a REPLACE copy that names no content type.
	copyDefaultContentType = "binary/octet-stream"
	allUsersGroupURI       = "http://acs.amazonaws.com/groups/global/AllUsers"
	noSuchKeyMessage       = "The specified key does not exist."
)

// NewMemoryClient returns a Client whose bucket lives in memory, for tests that must run with no
// AWS credentials and no network. It has the same type and methods as a Client from NewClient,
// and it reads no AWS environment or shared config. opts fill PublicURL as in NewClient; an
// endpoint option is ignored. Presigned URLs are well formed and signed with dummy credentials,
// but they point at a host on the reserved .invalid domain, so nothing answers them.
//
// No input field is silently ignored: a field the store does not model on an operation fails
// with ez.ENOTIMPLEMENTED naming the field, and so does a value it does not model, such as a
// canned ACL other than private and public-read or a bucket other than its own. A cancelled
// context fails with the context's error. The rejections it does model, such as a missing key,
// fail with the error types and wrapping the SDK returns.
func NewMemoryClient(bucket string, opts ...ClientOption) *Client {
	co := &clientOptions{}
	for _, opt := range opts {
		opt(co)
	}

	// Presigning is a local computation, so a real SDK client signs the URLs. It never sends a request.
	presignClient := s3.New(s3.Options{
		Region:       memoryRegion,
		Credentials:  credentials.NewStaticCredentialsProvider("MEMORYACCESSKEY", "memory-secret", ""),
		BaseEndpoint: aws.String("https://s3.memory.invalid"),
		// A directory bucket name would otherwise make the presigner ask S3 for a session
		DisableS3ExpressSessionAuth: aws.Bool(true),
	})

	return &Client{
		Region:    memoryRegion,
		Bucket:    bucket,
		s3:        newMemoryStore(bucket),
		presign:   s3.NewPresignClient(presignClient),
		PublicURL: co.publicBaseURL,
	}
}

// memoryObject is one stored object. It is never changed once stored: a write stores a new one.
type memoryObject struct {
	data         []byte
	contentType  string
	metadata     map[string]string
	acl          types.ObjectCannedACL
	etag         string
	lastModified time.Time
}

// memoryStore implements objectAPI for one bucket held in memory. One mutex guards the map.
type memoryStore struct {
	mu      sync.Mutex
	bucket  string
	created time.Time
	objects map[string]*memoryObject
}

var _ objectAPI = (*memoryStore)(nil)

func newMemoryStore(bucket string) *memoryStore {
	return &memoryStore{
		bucket:  bucket,
		created: time.Now().UTC(),
		objects: map[string]*memoryObject{},
	}
}

func (s *memoryStore) get(key string) *memoryObject {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.objects[key]
}

func (s *memoryStore) put(key string, object *memoryObject) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.objects[key] = object
}

func (s *memoryStore) PutObject(ctx context.Context, input *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	err := s.checkCall(ctx, "PutObject", input, "Bucket", "Key", "Body", "ContentType", "ContentLength", "Metadata", "ACL")
	if err != nil {
		return nil, err
	}

	key, err := requireKey("PutObject", input.Key)
	if err != nil {
		return nil, err
	}

	acl, err := cannedACL(input.ACL)
	if err != nil {
		return nil, err
	}

	err = checkHeaders(input.ContentType, input.Metadata)
	if err != nil {
		return nil, err
	}

	// grug: an upload of unknown length is not modeled. The SDK takes the length of a Body that
	// can seek or has a Len method; what it and S3 do with any other Body that has no
	// ContentLength was not observed.
	_, seekable := input.Body.(io.Seeker)
	_, sized := input.Body.(interface{ Len() int })
	if input.Body != nil && !seekable && !sized && input.ContentLength == nil {
		return nil, ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support a Body of unknown length, set ContentLength", nil)
	}

	// grug: the transport errors the SDK returns for a Body that fails to read, or whose length is
	// not ContentLength, are not modeled.
	var data []byte
	if input.Body != nil {
		data, err = io.ReadAll(input.Body)
		if err != nil {
			return nil, ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support a Body that fails to read", err)
		}
	}

	if input.ContentLength != nil && *input.ContentLength != int64(len(data)) {
		return nil, ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support a ContentLength of %d for a Body of %d bytes", *input.ContentLength, len(data)), nil)
	}

	sum := md5.Sum(data)
	object := &memoryObject{
		data:         data,
		contentType:  contentTypeOrDefault(input.ContentType),
		metadata:     storedMetadata(input.Metadata),
		acl:          acl,
		etag:         `"` + hex.EncodeToString(sum[:]) + `"`,
		lastModified: now(),
	}

	s.put(key, object)

	return &s3.PutObjectOutput{
		ETag: aws.String(object.etag),
	}, nil
}

func (s *memoryStore) GetObject(ctx context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	err := s.checkCall(ctx, "GetObject", input, "Bucket", "Key")
	if err != nil {
		return nil, err
	}

	key, err := requireKey("GetObject", input.Key)
	if err != nil {
		return nil, err
	}

	object := s.get(key)
	if object == nil {
		return nil, noSuchKey("GetObject")
	}

	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(object.data)),
		ContentLength: aws.Int64(int64(len(object.data))),
		ContentType:   aws.String(object.contentType),
		Metadata:      maps.Clone(object.metadata),
		ETag:          aws.String(object.etag),
		LastModified:  aws.Time(object.lastModified),
	}, nil
}

func (s *memoryStore) HeadObject(ctx context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	err := s.checkCall(ctx, "HeadObject", input, "Bucket", "Key")
	if err != nil {
		return nil, err
	}

	key, err := requireKey("HeadObject", input.Key)
	if err != nil {
		return nil, err
	}

	object := s.get(key)
	if object == nil {
		// A HEAD response has no body, so S3 answers a missing key with NotFound, not NoSuchKey.
		return nil, apiError("HeadObject", http.StatusNotFound, &types.NotFound{})
	}

	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(object.data))),
		ContentType:   aws.String(object.contentType),
		Metadata:      maps.Clone(object.metadata),
		ETag:          aws.String(object.etag),
		LastModified:  aws.Time(object.lastModified),
	}, nil
}

func (s *memoryStore) DeleteObject(ctx context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	err := s.checkCall(ctx, "DeleteObject", input, "Bucket", "Key")
	if err != nil {
		return nil, err
	}

	key, err := requireKey("DeleteObject", input.Key)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Deleting a missing key succeeds, as in S3.
	delete(s.objects, key)

	return &s3.DeleteObjectOutput{}, nil
}

func (s *memoryStore) CopyObject(ctx context.Context, input *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	err := s.checkCall(ctx, "CopyObject", input, "Bucket", "Key", "CopySource", "MetadataDirective", "ContentType", "Metadata", "ACL")
	if err != nil {
		return nil, err
	}

	key, err := requireKey("CopyObject", input.Key)
	if err != nil {
		return nil, err
	}

	sourceBucket, sourceKey, err := parseCopySource(aws.ToString(input.CopySource))
	if err != nil {
		return nil, err
	}

	// grug: the store holds one bucket, so copies come from that bucket only.
	if sourceBucket != s.bucket {
		return nil, ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store holds only bucket %q and cannot copy from %q", s.bucket, sourceBucket), nil)
	}

	// S3 does not copy ACLs: the copy gets the input's canned ACL or private.
	acl, err := cannedACL(input.ACL)
	if err != nil {
		return nil, err
	}

	// The SDK sends these headers whatever the MetadataDirective is
	err = checkHeaders(input.ContentType, input.Metadata)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	source := s.objects[sourceKey]
	if source == nil {
		// The SDK's CopyObject deserializer does not type NoSuchKey, so the error is generic.
		return nil, apiError("CopyObject", http.StatusNotFound, &smithy.GenericAPIError{Code: "NoSuchKey", Message: noSuchKeyMessage})
	}

	contentType := source.contentType
	metadata := source.metadata
	switch input.MetadataDirective {
	case "", types.MetadataDirectiveCopy:
		if sourceKey == key {
			// AWS's code. DigitalOcean Spaces answered InvalidArgument.
			return nil, apiError("CopyObject", http.StatusBadRequest, &smithy.GenericAPIError{
				Code:    "InvalidRequest",
				Message: "copying an object onto itself requires MetadataDirective REPLACE",
			})
		}
	case types.MetadataDirectiveReplace:
		// A copy has no body, so the SDK sends no default content type and S3 stores its own.
		contentType = copyDefaultContentType
		if input.ContentType != nil {
			contentType = strings.TrimSpace(*input.ContentType)
		}
		metadata = storedMetadata(input.Metadata)
	default:
		return nil, ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support the MetadataDirective %q", input.MetadataDirective), nil)
	}

	object := &memoryObject{
		data:         source.data,
		contentType:  contentType,
		metadata:     metadata,
		acl:          acl,
		etag:         source.etag,
		lastModified: now(),
	}
	s.objects[key] = object

	return &s3.CopyObjectOutput{
		CopyObjectResult: &types.CopyObjectResult{
			ETag:         aws.String(object.etag),
			LastModified: aws.Time(object.lastModified),
		},
	}, nil
}

func (s *memoryStore) ListObjects(ctx context.Context, input *s3.ListObjectsInput, _ ...func(*s3.Options)) (*s3.ListObjectsOutput, error) {
	err := s.checkCall(ctx, "ListObjects", input, "Bucket", "Prefix", "Marker", "MaxKeys")
	if err != nil {
		return nil, err
	}

	maxKeys := int32(memoryMaxKeys)
	if input.MaxKeys != nil {
		// grug: what S3 answers a MaxKeys below zero or over its limit with was not observed.
		if *input.MaxKeys < 0 || *input.MaxKeys > memoryMaxKeys {
			return nil, ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support a MaxKeys below 0 or over %d", memoryMaxKeys), nil)
		}

		maxKeys = *input.MaxKeys
	}

	prefix := aws.ToString(input.Prefix)
	marker := aws.ToString(input.Marker)

	s.mu.Lock()
	defer s.mu.Unlock()

	keys := []string{}
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) && key > marker {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)

	// A listing of zero keys is empty and not truncated, as S3 answers it.
	truncated := maxKeys > 0 && len(keys) > int(maxKeys)
	if len(keys) > int(maxKeys) {
		keys = keys[:maxKeys]
	}

	var contents []types.Object
	for _, key := range keys {
		object := s.objects[key]
		contents = append(contents, types.Object{
			Key:          aws.String(key),
			Size:         aws.Int64(int64(len(object.data))),
			ETag:         aws.String(object.etag),
			LastModified: aws.Time(object.lastModified),
		})
	}

	// NextMarker stays empty. AWS fills it only for a listing with a Delimiter and DigitalOcean
	// Spaces fills it always, so code that pages with the last key it received as the next Marker
	// works on both, and code that relies on NextMarker fails here instead of on AWS.
	return &s3.ListObjectsOutput{
		Name:        aws.String(s.bucket),
		Prefix:      aws.String(prefix),
		Marker:      aws.String(marker),
		MaxKeys:     aws.Int32(maxKeys),
		Contents:    contents,
		IsTruncated: aws.Bool(truncated),
	}, nil
}

func (s *memoryStore) PutObjectAcl(ctx context.Context, input *s3.PutObjectAclInput, _ ...func(*s3.Options)) (*s3.PutObjectAclOutput, error) {
	err := s.checkCall(ctx, "PutObjectAcl", input, "Bucket", "Key", "ACL")
	if err != nil {
		return nil, err
	}

	key, err := requireKey("PutObjectAcl", input.Key)
	if err != nil {
		return nil, err
	}

	if input.ACL == "" {
		// The code DigitalOcean Spaces answered. What AWS answers was not observed.
		return nil, apiError("PutObjectAcl", http.StatusBadRequest, &smithy.GenericAPIError{
			Code:    "InvalidArgument",
			Message: "ACL is required",
		})
	}

	acl, err := cannedACL(input.ACL)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	object := s.objects[key]
	if object == nil {
		return nil, noSuchKey("PutObjectAcl")
	}

	updated := *object
	updated.acl = acl
	s.objects[key] = &updated

	return &s3.PutObjectAclOutput{}, nil
}

func (s *memoryStore) GetObjectAcl(ctx context.Context, input *s3.GetObjectAclInput, _ ...func(*s3.Options)) (*s3.GetObjectAclOutput, error) {
	err := s.checkCall(ctx, "GetObjectAcl", input, "Bucket", "Key")
	if err != nil {
		return nil, err
	}

	key, err := requireKey("GetObjectAcl", input.Key)
	if err != nil {
		return nil, err
	}

	object := s.get(key)
	if object == nil {
		return nil, noSuchKey("GetObjectAcl")
	}

	// The public grant comes before the owner's, the order DigitalOcean Spaces returned them in.
	// The order on AWS was not observed.
	var grants []types.Grant
	if object.acl == types.ObjectCannedACLPublicRead {
		grants = append(grants, types.Grant{
			Grantee:    &types.Grantee{Type: types.TypeGroup, URI: aws.String(allUsersGroupURI)},
			Permission: types.PermissionRead,
		})
	}
	grants = append(grants, types.Grant{
		Grantee:    &types.Grantee{Type: types.TypeCanonicalUser, ID: aws.String(memoryOwnerID)},
		Permission: types.PermissionFullControl,
	})

	return &s3.GetObjectAclOutput{
		Owner:  &types.Owner{ID: aws.String(memoryOwnerID)},
		Grants: grants,
	}, nil
}

func (s *memoryStore) ListBuckets(ctx context.Context, input *s3.ListBucketsInput, _ ...func(*s3.Options)) (*s3.ListBucketsOutput, error) {
	err := s.checkCall(ctx, "ListBuckets", input)
	if err != nil {
		return nil, err
	}

	return &s3.ListBucketsOutput{
		Buckets: []types.Bucket{
			{Name: aws.String(s.bucket), CreationDate: aws.Time(s.created)},
		},
		Owner: &types.Owner{ID: aws.String(memoryOwnerID)},
	}, nil
}

// checkCall returns the error a call fails with before it reaches the bucket: ctx's error when ctx
// is done, and ez.ENOTIMPLEMENTED for an empty Bucket, for a Bucket other than the store's, and
// for the first exported field of input, a pointer to an SDK input struct, that is set and not
// named in modeled.
// grug: one bucket, and only the listed fields are modeled on each operation. Any other field
// fails, including fields a newer SDK adds. List a field here once the store models what S3 does
// with it.
func (s *memoryStore) checkCall(ctx context.Context, operation string, input any, modeled ...string) error {
	err := ctx.Err()
	if err != nil {
		return operationError(operation, err)
	}

	value := reflect.ValueOf(input).Elem()

	bucketField := value.FieldByName("Bucket")
	if bucketField.IsValid() {
		bucket := aws.ToString(bucketField.Interface().(*string))
		if bucket == "" {
			return ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support an empty Bucket", nil)
		}
		if bucket != s.bucket {
			return ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store holds only bucket %q and cannot reach %q", s.bucket, bucket), nil)
		}
	}

	for i := range value.NumField() {
		field := value.Type().Field(i)
		if !field.IsExported() || slices.Contains(modeled, field.Name) || value.Field(i).IsZero() {
			continue
		}

		return ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support %s on %s", field.Name, operation), nil)
	}

	return nil
}

// operationError wraps err the way the SDK wraps every error an operation returns.
func operationError(operation string, err error) error {
	return &smithy.OperationError{ServiceID: s3.ServiceID, OperationName: operation, Err: err}
}

// responseError is the wrapper the SDK puts around every S3 error, which is internal to the SDK:
// an *awshttp.ResponseError that also satisfies s3.ResponseError.
type responseError struct {
	*awshttp.ResponseError
}

var _ s3.ResponseError = (*responseError)(nil)

// ServiceHostID is empty, since no host answered.
func (e *responseError) ServiceHostID() string {
	return ""
}

func (e *responseError) As(target any) bool {
	return errors.As(e.ResponseError, target)
}

// apiError wraps err, an error S3 answers operation with, the way the SDK returns it: inside a
// response error carrying the HTTP status, inside a *smithy.OperationError.
func apiError(operation string, status int, err error) error {
	return operationError(operation, &responseError{
		ResponseError: &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
				Err:      err,
			},
		},
	})
}

// now is the time an object is stamped with. S3 returns Last-Modified as an HTTP date, which
// holds whole seconds.
func now() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

// requireKey returns key, or the error the SDK returns without sending a request: a validation
// error when Key is missing and a serialization error when it is empty.
func requireKey(operation string, key *string) (string, error) {
	if key == nil {
		invalidParams := smithy.InvalidParamsError{Context: operation + "Input"}
		invalidParams.Add(smithy.NewErrParamRequired("Key"))

		return "", operationError(operation, invalidParams)
	}

	if *key == "" {
		// The SDK's serializer wraps its own serialization error once more
		return "", operationError(operation, &smithy.SerializationError{
			Err: &smithy.SerializationError{Err: errors.New("input member Key must not be empty")},
		})
	}

	// grug: S3 refuses a key over 1024 bytes and one that is not UTF-8. Its answers were not
	// observed, so they fail as not modeled.
	if len(*key) > maxKeyBytes || !utf8.ValidString(*key) {
		return "", ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support a Key over %d bytes or one that is not UTF-8", maxKeyBytes), nil)
	}

	// grug: what S3 does with a control character in a key, or with a . or .. path segment, was
	// not observed.
	if strings.ContainsFunc(*key, unicode.IsControl) || hasDotSegment(*key) {
		return "", ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support a Key with a control character or a . or .. segment", nil)
	}

	return *key, nil
}

func hasDotSegment(key string) bool {
	for segment := range strings.SplitSeq(key, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}

	return false
}

// noSuchKey is the error S3 answers a missing key with on GetObject, GetObjectAcl and
// PutObjectAcl, typed as the SDK's deserializers type it for those operations so callers can
// classify it with errors.As. CopyObject's deserializer does not type it.
func noSuchKey(operation string) error {
	return apiError(operation, http.StatusNotFound, &types.NoSuchKey{Message: aws.String(noSuchKeyMessage)})
}

// cannedACL returns the canned ACL an object gets from acl, private when acl is empty.
// grug: only the canned ACLs private and public-read are modeled. Other canned ACLs fail.
func cannedACL(acl types.ObjectCannedACL) (types.ObjectCannedACL, error) {
	switch acl {
	case "":
		return types.ObjectCannedACLPrivate, nil
	case types.ObjectCannedACLPrivate, types.ObjectCannedACLPublicRead:
		return acl, nil
	}

	return "", ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support the canned ACL %q", acl), nil)
}

// parseCopySource splits a CopySource of the form bucket/key, with an optional leading slash and
// URL encoding, into its bucket and key.
// grug: what S3 answers a malformed CopySource with was not observed, so one fails as not modeled.
func parseCopySource(source string) (string, string, error) {
	// grug: object versions are not modeled, so a ?versionId= suffix fails.
	if strings.Contains(source, "?") {
		return "", "", ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support CopySource versions", nil)
	}

	// grug: whether S3 reads a + in a CopySource as a space was not observed.
	if strings.Contains(source, "+") {
		return "", "", ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support a + in CopySource, encode it as %2B", nil)
	}

	// grug: S3 asks for a URL-encoded CopySource, and the SDK sends the value as it is given. What
	// S3 answers one with a space or a byte outside printable ASCII was not observed.
	if strings.ContainsFunc(source, isNotURLEncoded) {
		return "", "", ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support a CopySource with a space or a character outside printable ASCII, URL-encode it", nil)
	}

	decoded, err := url.PathUnescape(strings.TrimPrefix(source, "/"))
	if err != nil {
		return "", "", ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support a CopySource that is not valid URL encoding", nil)
	}

	bucket, key, found := strings.Cut(decoded, "/")
	if !found || bucket == "" || key == "" {
		return "", "", ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support a CopySource that is not bucket/key", nil)
	}

	return bucket, key, nil
}

func isNotURLEncoded(r rune) bool {
	return r <= ' ' || r >= 0x7f
}

func contentTypeOrDefault(contentType *string) string {
	if contentType == nil {
		return defaultContentType
	}

	return strings.TrimSpace(*contentType)
}

// maxKeyBytes is S3's limit on the length of a key.
const maxKeyBytes = 1024

// maxMetadataBytes is S3's limit on the keys and values of one object's metadata.
const maxMetadataBytes = 2048

// checkHeaders fails for a content type or metadata the store does not model. The SDK sends both
// as HTTP headers and refuses a metadata key that is not a header token and a value with a line
// break. What S3 returns for a value outside ASCII, and what it answers metadata over its limit
// with, was not observed, nor what it does with an empty content type or an empty value.
// grug: printable ASCII, not empty, and S3's 2 KB only.
func checkHeaders(contentType *string, metadata map[string]string) error {
	if !isPrintableASCII(aws.ToString(contentType)) {
		return ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support a ContentType outside printable ASCII", nil)
	}

	// The SDK sends a ContentType that is set but empty as an empty header, with no default
	if contentType != nil && strings.TrimSpace(*contentType) == "" {
		return ez.New(ez.ENOTIMPLEMENTED, "the memory store does not support an empty ContentType, leave it nil for the default", nil)
	}

	size := 0
	for key, value := range metadata {
		if !isHeaderToken(key) {
			return ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support the metadata key %q, which is not an HTTP header token", key), nil)
		}
		if !isPrintableASCII(value) {
			return ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support a value outside printable ASCII for the metadata key %q", key), nil)
		}
		if strings.TrimSpace(value) == "" {
			return ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support an empty value for the metadata key %q", key), nil)
		}

		size += len(key) + len(value)
	}

	if size > maxMetadataBytes {
		return ez.New(ez.ENOTIMPLEMENTED, fmt.Sprintf("the memory store does not support more than %d bytes of metadata", maxMetadataBytes), nil)
	}

	return nil
}

func isPrintableASCII(value string) bool {
	for i := range len(value) {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}

	return true
}

// isHeaderToken reports whether name can be an HTTP header name: letters, digits and the
// punctuation RFC 9110 allows in a token.
func isHeaderToken(name string) bool {
	if name == "" {
		return false
	}

	for i := range len(name) {
		c := name[i]
		isLetter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit && !strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			return false
		}
	}

	return true
}

// storedMetadata copies metadata as the SDK returns it from S3: keys lowercased, the space around
// each value trimmed, and nil when there is none.
func storedMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}

	stored := make(map[string]string, len(metadata))
	for key, value := range metadata {
		stored[strings.ToLower(key)] = strings.TrimSpace(value)
	}

	return stored
}
