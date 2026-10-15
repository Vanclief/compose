package s3

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// Presigning is local: static credentials and no network call, so this test needs no bucket.
func newOfflineClient(t *testing.T) *Client {
	client, err := NewClient(context.Background(), "sfo3", "AKIAOFFLINEEXAMPLE", "offline-secret", "offline-bucket",
		WithDigitalOceanEndpoint("sfo3", "digitaloceanspaces.com"))
	require.NoError(t, err)
	return client
}

func TestPresignGetObjectSignsAsOfTheGivenInstant(t *testing.T) {
	client := newOfflineClient(t)
	// Relative to now so the URL is never expired, truncated so the signed date is predictable.
	signingTime := time.Now().UTC().Truncate(time.Hour)

	first, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{Key: aws.String("app/files/a.jpeg")}, signingTime, 24*time.Hour)
	require.NoError(t, err)
	second, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{Key: aws.String("app/files/a.jpeg")}, signingTime, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, first.URL, second.URL, "the same key and instant sign to the same URL")
	require.Equal(t, "GET", first.Method)

	parsed, err := url.Parse(first.URL)
	require.NoError(t, err)
	require.Equal(t, "offline-bucket.sfo3.digitaloceanspaces.com", parsed.Host)
	require.Equal(t, "/app/files/a.jpeg", parsed.Path)
	query := parsed.Query()
	require.Equal(t, signingTime.Format("20060102T150405Z"), query.Get("X-Amz-Date"))
	require.Equal(t, "86400", query.Get("X-Amz-Expires"))
	require.NotEmpty(t, query.Get("X-Amz-Signature"))

	later, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{Key: aws.String("app/files/a.jpeg")}, signingTime.Add(time.Hour), 24*time.Hour)
	require.NoError(t, err)
	require.NotEqual(t, first.URL, later.URL, "another instant signs to another URL")
}

func TestPresignGetObjectZeroTimeSignsNow(t *testing.T) {
	client := newOfflineClient(t)

	signed, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{Key: aws.String("k")}, time.Time{}, time.Minute)
	require.NoError(t, err)

	parsed, err := url.Parse(signed.URL)
	require.NoError(t, err)
	date, err := time.Parse("20060102T150405Z", parsed.Query().Get("X-Amz-Date"))
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().UTC(), date, 2*time.Minute)
	require.Equal(t, "60", parsed.Query().Get("X-Amz-Expires"))
}

func TestPresignGetObjectRequiresInput(t *testing.T) {
	client := newOfflineClient(t)

	_, err := client.PresignGetObject(context.Background(), nil, time.Time{}, time.Minute)
	require.Error(t, err)
}

func TestPresignGetObjectRequiresExpiresWithSigningTime(t *testing.T) {
	client := newOfflineClient(t)

	_, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{Key: aws.String("k")}, time.Now(), 0)
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

func TestPresignGetObjectRejectsAlreadyExpiredWindow(t *testing.T) {
	client := newOfflineClient(t)
	signingTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	_, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{Key: aws.String("k")}, signingTime, time.Hour)
	require.Error(t, err)
	require.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

func TestPresignGetObjectAcceptsPastSigningTimeThatStillCoversNow(t *testing.T) {
	client := newOfflineClient(t)
	signingTime := time.Now().Add(-10 * time.Minute)

	signed, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{Key: aws.String("k")}, signingTime, time.Hour)
	require.NoError(t, err)
	require.NotEmpty(t, signed.URL)
}

func TestPresignGetObjectReturnsSignedHeaders(t *testing.T) {
	client := newOfflineClient(t)

	signed, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{Key: aws.String("k"), Range: aws.String("bytes=0-9")}, time.Now(), time.Hour)
	require.NoError(t, err)
	require.Equal(t, "bytes=0-9", signed.Headers.Get("Range"), "a Range input is a signed header the caller must send")

	parsed, err := url.Parse(signed.URL)
	require.NoError(t, err)
	require.Contains(t, parsed.Query().Get("X-Amz-SignedHeaders"), "range")
}

// The fixed-time presigner wraps the SDK's default signer, so S3's single path escaping
// (DisableURIPathEscaping) still applies to keys with reserved characters.
func TestPresignGetObjectKeepsDefaultPathEscaping(t *testing.T) {
	client := newOfflineClient(t)
	signingTime := time.Now().UTC().Truncate(time.Second)
	key := "app/files/a b+c.jpeg"

	signed, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{Key: aws.String(key)}, signingTime, time.Hour)
	require.NoError(t, err)

	parsed, err := url.Parse(signed.URL)
	require.NoError(t, err)
	require.Equal(t, "/app/files/a%20b%2Bc.jpeg", parsed.EscapedPath())

	require.Equal(t, presignWithSigner(t, client, key, signingTime, true), signed.URL, "signs like the SDK default, with path escaping disabled")
	require.NotEqual(t, presignWithSigner(t, client, key, signingTime, false), signed.URL, "double escaping the path would change the signature")
}

// presignWithSigner presigns key at signingTime with an explicitly built v4 signer.
func presignWithSigner(t *testing.T, client *Client, key string, signingTime time.Time, disableURIPathEscaping bool) string {
	signer := v4.NewSigner(func(so *v4.SignerOptions) {
		so.DisableURIPathEscaping = disableURIPathEscaping
	})

	req, err := client.presign.PresignGetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(client.Bucket), Key: aws.String(key)}, func(opts *s3.PresignOptions) {
		opts.Expires = time.Hour
		opts.Presigner = &fixedTimePresigner{next: signer, signingTime: signingTime}
	})
	require.NoError(t, err)

	return req.URL
}
