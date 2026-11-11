package ses

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestRecordingClientRecordsEmail(t *testing.T) {
	tests := []struct {
		name     string
		opts     []ClientOption
		wantFrom string
	}{
		{
			name:     "sender address only",
			opts:     []ClientOption{WithEmailSender("sender@example.com")},
			wantFrom: "sender@example.com",
		},
		{
			name:     "sender name",
			opts:     []ClientOption{WithEmailSender("sender@example.com"), WithSenderName("Compose")},
			wantFrom: "Compose <sender@example.com>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, outbox := NewRecordingClient(tt.opts...)
			email := Email{
				Recipients: []string{"a@example.com", "b@example.com"},
				Subject:    "Welcome",
				HTMLBody:   "<h1>Hello</h1>",
				TextBody:   "Hello",
			}

			id, err := client.SendEmail(context.Background(), email)
			require.NoError(t, err)
			assert.NotEmpty(t, id)

			emails := outbox.Emails()
			require.Len(t, emails, 1)
			assert.Equal(t, SentEmail{ID: id, From: tt.wantFrom, Email: email}, emails[0])
			assert.Empty(t, outbox.SMS())
		})
	}
}

func TestRecordingClientKeepsOrderAndResets(t *testing.T) {
	client, outbox := NewRecordingClient(WithEmailSender("sender@example.com"))
	ctx := context.Background()

	var emailIDs []string
	for i := 0; i < 3; i++ {
		id, err := client.SendEmail(ctx, Email{
			Recipients: []string{"recipient@example.com"},
			Subject:    fmt.Sprintf("email %d", i),
			TextBody:   "Hello",
		})
		require.NoError(t, err)
		emailIDs = append(emailIDs, id)
	}

	var smsIDs []string
	for i := 0; i < 2; i++ {
		id, err := client.SendSMS(ctx, "+15555550100", fmt.Sprintf("sms %d", i))
		require.NoError(t, err)
		smsIDs = append(smsIDs, id)
	}

	emails := outbox.Emails()
	require.Len(t, emails, 3)
	for i := range emails {
		assert.Equal(t, emailIDs[i], emails[i].ID)
		assert.Equal(t, fmt.Sprintf("email %d", i), emails[i].Email.Subject)
	}

	sms := outbox.SMS()
	require.Len(t, sms, 2)
	for i := range sms {
		assert.Equal(t, smsIDs[i], sms[i].ID)
		assert.Equal(t, fmt.Sprintf("sms %d", i), sms[i].Message)
	}

	allIDs := append(append([]string{}, emailIDs...), smsIDs...)
	assert.Len(t, uniqueStrings(allIDs), len(allIDs))

	outbox.Reset()
	assert.Empty(t, outbox.Emails())
	assert.Empty(t, outbox.SMS())

	id, err := client.SendSMS(ctx, "+15555550100", "after reset")
	require.NoError(t, err)
	assert.NotContains(t, allIDs, id)
}

func TestRecordingAndRealClientsRejectTheSameInvalidEmails(t *testing.T) {
	tooManyRecipients := make([]string, maxEmailRecipients+1)
	for i := range tooManyRecipients {
		tooManyRecipients[i] = "recipient@example.com"
	}

	tests := []struct {
		name  string
		email Email
	}{
		{
			name:  "no recipients",
			email: Email{Subject: "Test", TextBody: "Hello"},
		},
		{
			name:  "empty recipient",
			email: Email{Recipients: []string{"recipient@example.com", ""}, Subject: "Test", TextBody: "Hello"},
		},
		{
			name:  "more than 50 recipients",
			email: Email{Recipients: tooManyRecipients, Subject: "Test", TextBody: "Hello"},
		},
		{
			name:  "no body",
			email: Email{Recipients: []string{"recipient@example.com"}, Subject: "Test"},
		},
		{
			name: "attachment with empty filename",
			email: Email{
				Recipients:  []string{"recipient@example.com"},
				TextBody:    "Hello",
				Attachments: []Attachment{{ContentType: "application/pdf", Data: []byte("data")}},
			},
		},
		{
			name: "attachment content type with CRLF",
			email: Email{
				Recipients:  []string{"recipient@example.com"},
				TextBody:    "Hello",
				Attachments: []Attachment{{Filename: "report.pdf", ContentType: "application/pdf\r\nX-Evil: 1", Data: []byte("data")}},
			},
		},
		{
			name: "attachment content type with CR",
			email: Email{
				Recipients:  []string{"recipient@example.com"},
				TextBody:    "Hello",
				Attachments: []Attachment{{Filename: "report.pdf", ContentType: "application/pdf\rX-Evil: 1", Data: []byte("data")}},
			},
		},
		{
			name: "attachment content type with LF",
			email: Email{
				Recipients:  []string{"recipient@example.com"},
				TextBody:    "Hello",
				Attachments: []Attachment{{Filename: "report.pdf", ContentType: "application/pdf\nX-Evil: 1", Data: []byte("data")}},
			},
		},
	}

	// Static credentials make NewClient build its AWS config without any
	// network call, and validation errors return before AWS is reached.
	realClient, err := NewClient(context.Background(), "us-east-1", "test-access-key", "test-secret-key", WithEmailSender("sender@example.com"))
	require.NoError(t, err)

	recordingClient, outbox := NewRecordingClient(WithEmailSender("sender@example.com"))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := realClient.SendEmail(context.Background(), tt.email)
			require.Error(t, err)
			realCode := ez.ErrorCode(err)

			id, err := recordingClient.SendEmail(context.Background(), tt.email)
			require.Error(t, err)
			assert.Empty(t, id)
			assert.Equal(t, realCode, ez.ErrorCode(err))
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			assert.Empty(t, outbox.Emails())
		})
	}
}

func TestRecordingClientRecordsACopyOfTheEmail(t *testing.T) {
	client, outbox := NewRecordingClient(WithEmailSender("sender@example.com"))

	recipients := []string{"recipient@example.com"}
	data := []byte("%PDF-1.7")
	attachments := []Attachment{{Filename: "report.pdf", ContentType: "application/pdf", Data: data}}

	id, err := client.SendEmail(context.Background(), Email{
		Recipients:  recipients,
		Subject:     "Report",
		TextBody:    "Attached",
		Attachments: attachments,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	recipients[0] = "changed@example.com"
	data[0] = 'X'
	attachments[0].Filename = "changed.pdf"

	emails := outbox.Emails()
	require.Len(t, emails, 1)
	assert.Equal(t, id, emails[0].ID)
	assert.Equal(t, []string{"recipient@example.com"}, emails[0].Email.Recipients)
	require.Len(t, emails[0].Email.Attachments, 1)
	assert.Equal(t, "report.pdf", emails[0].Email.Attachments[0].Filename)
	assert.Equal(t, []byte("%PDF-1.7"), emails[0].Email.Attachments[0].Data)

	// Changing what Emails returned does not change the outbox either.
	emails[0].Email.Recipients[0] = "changed@example.com"
	emails[0].Email.Attachments[0].Data[0] = 'X'

	again := outbox.Emails()
	assert.Equal(t, []string{"recipient@example.com"}, again[0].Email.Recipients)
	assert.Equal(t, []byte("%PDF-1.7"), again[0].Email.Attachments[0].Data)
}

func TestRecordingClientRecordsSMS(t *testing.T) {
	client, outbox := NewRecordingClient()

	id, err := client.SendSMS(context.Background(), "+15555550100", "Your code is 123456")
	require.NoError(t, err)
	assert.NotEmpty(t, id)

	assert.Equal(t, []SentSMS{{ID: id, PhoneNumber: "+15555550100", Message: "Your code is 123456"}}, outbox.SMS())
	assert.Empty(t, outbox.Emails())
}

func TestRecordingClientConcurrentSends(t *testing.T) {
	client, outbox := NewRecordingClient(WithEmailSender("sender@example.com"))
	ctx := context.Background()

	const goroutines = 8
	const sendsPerGoroutine = 25

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < sendsPerGoroutine; i++ {
				_, err := client.SendEmail(ctx, Email{
					Recipients: []string{"recipient@example.com"},
					Subject:    "Concurrent",
					TextBody:   "Hello",
				})
				assert.NoError(t, err)

				_, err = client.SendSMS(ctx, "+15555550100", "Concurrent")
				assert.NoError(t, err)

				outbox.Emails()
				outbox.SMS()
			}
		}()
	}
	wg.Wait()

	emails := outbox.Emails()
	sms := outbox.SMS()
	assert.Len(t, emails, goroutines*sendsPerGoroutine)
	assert.Len(t, sms, goroutines*sendsPerGoroutine)

	var ids []string
	for i := range emails {
		ids = append(ids, emails[i].ID)
	}
	for i := range sms {
		ids = append(ids, sms[i].ID)
	}
	assert.Len(t, uniqueStrings(ids), len(ids))
}

// uniqueStrings returns the distinct values of values.
func uniqueStrings(values []string) map[string]struct{} {
	unique := make(map[string]struct{}, len(values))
	for _, value := range values {
		unique[value] = struct{}{}
	}

	return unique
}

func TestRecordingClientFailsOnACancelledContext(t *testing.T) {
	client, outbox := NewRecordingClient(WithEmailSender("noreply@example.com"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.SendEmail(ctx, Email{Recipients: []string{"resident@example.com"}, Subject: "Hello", TextBody: "Hi"})
	require.ErrorIs(t, err, context.Canceled)
	var operationErr *smithy.OperationError
	require.ErrorAs(t, err, &operationErr)
	assert.Equal(t, "SendEmail", operationErr.Operation())

	_, err = client.SendSMS(ctx, "+15555550100", "Hi")
	require.ErrorIs(t, err, context.Canceled)
	operationErr = nil
	require.ErrorAs(t, err, &operationErr)
	assert.Equal(t, "Publish", operationErr.Operation())

	assert.Empty(t, outbox.Emails())
	assert.Empty(t, outbox.SMS())
}
