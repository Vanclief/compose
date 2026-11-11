# Testing an app built on compose

An app's tests should need a PostgreSQL server and nothing else: no AWS credentials, no network,
and no `if environment == "TEST"` in the code under test. Compose provides a stand-in for each
outside service its clients talk to, and helpers for the test database.

This guide covers:

- [Choosing the stand-ins](#choosing-the-stand-ins)
- [Files: the in-memory S3 client](#files-the-in-memory-s3-client)
- [Email and SMS: the recording SES client](#email-and-sms-the-recording-ses-client)
- [The test database](#the-test-database)

## Choosing the stand-ins

The stand-ins have the same types as the real clients (`*s3.Client`, `*ses.Client`), so the code
that uses a client does not change. Only code selects them: there is no config field that turns
one on, so no config value can make a running server store files in memory or stop sending email.

The app's test setup assigns the stand-ins, and the app's `Setup` builds a real client only where
none is set:

```go
// Test setup, before it calls Setup
controller.S3 = s3.NewMemoryClient(controller.Config.S3.Bucket)

sesClient, outbox := ses.NewRecordingClient(
	ses.WithEmailSender(controller.Config.SES.EmailSender),
	ses.WithSenderName(controller.Config.SES.SenderName),
)
controller.SES = sesClient
```

```go
// Setup
if controller.S3 == nil {
	s3Client, err := controller.WithS3(rootCtx, &controller.Config.S3, controller.EnvVars.S3SecretKey)
	if err != nil {
		return ez.Wrap(err)
	}

	controller.S3 = s3Client
}
```

With both assigned, the test setup no longer needs the S3 and AWS secrets.

## Files: the in-memory S3 client

`s3.NewMemoryClient(bucket, opts...)` returns a `*s3.Client` whose bucket lives in memory. It
reads no AWS environment or shared config and makes no network call. Each call returns a client
with its own empty bucket. `WithPublicBaseURL` and `WithDigitalOceanCDN` fill `PublicURL` as they
do for `NewClient`.

Every `Client` method works on it. `GetObjectACL` reads back what an upload's canned ACL or
`SetObjectACL` left on an object, on the real client and on the stand-in alike:

```go
output, err := client.GetObjectACL(ctx, &awss3.GetObjectAclInput{Key: aws.String(key)})
```

A public-read object has a `READ` grant to the group
`http://acs.amazonaws.com/groups/global/AllUsers`; a private one has no such grant.

What the stand-in does not do:

- **Presigned URLs are not fetchable.** They are well formed and signed with dummy credentials,
  but they point at a host on the reserved `.invalid` domain. A test that fetches a file by URL
  needs a real bucket.
- **An input field it does not model fails** with `ez.ENOTIMPLEMENTED` naming the field, instead
  of being ignored. It models the key, body, content type, content length, metadata and canned
  ACL of an upload; the source, metadata directive, content type, metadata and canned ACL of a
  copy; the prefix, marker and `MaxKeys` of a listing; the key and canned ACL of an ACL write;
  and the key alone of a download, an existence check, a delete and an ACL read. Everything else
  fails: `Range`, `IfMatch` and
  `IfNoneMatch`, `VersionId`, `ContentDisposition`, `CacheControl`, server-side encryption,
  `Delimiter`, grant headers, and so on.
- **Values it does not model fail the same way**: a canned ACL other than `private` and
  `public-read`; a bucket other than the client's, on any call or as a copy source; a copy of an
  object version; a copy source that is malformed or not URL-encoded; a metadata directive other than `COPY` and `REPLACE`;
  a `MaxKeys` below 0 or over 1000; an upload whose `Body` has no known length
  and no `ContentLength`, or whose `ContentLength` is not its length; a content type or metadata value that is empty or outside printable ASCII, a
  metadata key that is not an HTTP header token, and more than 2 KB of metadata; a key over 1024
  bytes, not UTF-8, or with a control character or a `.` or `..` segment; and an empty bucket name.
- **Bucket configuration is not modeled.** It accepts a canned ACL as a bucket with ACLs enabled
  does. An AWS bucket with Object Ownership set to bucket owner enforced, the default for new AWS
  buckets, refuses `public-read`.
- **Outputs carry only what it models.** A listing entry has its key, size, ETag and
  last-modified time; its storage class and owner are empty.
- **`NextMarker` is empty** on a truncated listing. AWS fills it only with a delimiter and
  DigitalOcean Spaces fills it always, so page with the last key received as the next `Marker`,
  which works on both.

Errors have the shape the SDK gives them. A missing key is a `*types.NoSuchKey` on a download and
an ACL call, and a missing copy source is an API error with the code `NoSuchKey`. Each carries
its HTTP status, so `errors.As` to the typed error, to `smithy.APIError`, to
`*awshttp.ResponseError` or to `s3.ResponseError` classifies them as it does against S3. `FileExists` answers false for a
missing key. A cancelled context fails the call.

One behavior test (`behavior_test.go` in the package) runs the same cases against the in-memory
client and, when AWS credentials are available, against a real bucket; without them that half
skips, or fails when `COMPOSE_TEST_AWS` is set. A behavior the stand-in gains is added there.

## Email and SMS: the recording SES client

`ses.NewRecordingClient(opts...)` returns a `*ses.Client` and its `*ses.Outbox`. The client sends
nothing: `SendEmail` and `SendSMS` record what they were given and return a message ID. It needs
no credentials and makes no network call.

```go
emails := outbox.Emails()
require.Len(t, emails, 1)
require.Equal(t, []string{"resident@example.com"}, emails[0].Email.Recipients)
require.Equal(t, "Emma <noreply@example.com>", emails[0].From)
```

- `Emails()` and `SMS()` return copies, oldest first. `Reset()` empties both, for use between
  tests.
- `SendEmail` runs the checks the real client runs before it calls SES, and renders an email
  with attachments as a MIME message first. An email those checks refuse is refused here and not
  recorded. What only SES or SNS refuses is recorded: an empty sender, a malformed address, an
  empty phone number. A cancelled context fails the send and records nothing.
- `SentEmail.From` is the sender address the real client would have used, from `WithEmailSender`
  and `WithSenderName`.

## The test database

Tests run against a real PostgreSQL. The package
`drivers/databases/relational/postgres/pgtest` does the housekeeping around it. It needs
PostgreSQL 13 or later.

```go
// Once per test binary. cfg.Database is the base name, for instance "app_test".
sweepCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
err := pgtest.Sweep(sweepCtx, cfg, time.Hour)
cancel()
if err != nil {
	log.Warn().Err(err).Msg("Could not sweep stale test databases")
}

testCfg, err := pgtest.Create(cfg)
if err != nil {
	return err
}

// ... connect with testCfg and load the schema ...

// Between tests
err = pgtest.Truncate(ctx, db)

// When the run ends, after closing the connection
err = pgtest.Drop(testCfg)
```

- **`Create`** makes a database named `<base>_<unix seconds>_<16 hex chars>`, lowercased, and returns the
  config that points at it. The base name is at most 35 bytes, so the whole name fits the 63
  PostgreSQL keeps; `Create` and `Sweep` refuse a longer one. It turns `synchronous_commit` off
  for that database, since nothing in it needs to survive a crash.
- **`Truncate`** empties every table in the `public` schema and restarts their sequences. It keeps
  `bun_migrations` and `bun_migration_locks`, so the schema baseline's floor row survives, and
  the tables that belong to an extension. The truncate cascades: a table outside that set with a
  foreign key to an emptied table is emptied too.
- **`Drop`** drops the database, ending any session still connected to it. A database that does
  not exist is not an error. It takes the config `Create` returned and refuses a name without
  `Create`'s stamp, so passing the base config by mistake drops nothing.
- **`Sweep`** drops the databases earlier runs left behind under the same base name: a run that
  was interrupted or killed by `-timeout` never reaches its `Drop`. It only considers names that
  match the pattern `Create` generates, skips a database with an open session, never forces a
  drop, and stops when its context is done. A sweep failure should not stop the tests.

The package installs no signal handler. An app that wants its databases dropped on Ctrl-C keeps
its own handler and calls `Drop` from it.
