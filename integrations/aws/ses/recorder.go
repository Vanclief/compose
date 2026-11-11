package ses

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/smithy-go"
	"github.com/vanclief/ez"
)

// SentEmail is one email a recording client accepted.
type SentEmail struct {
	ID    string // the message ID SendEmail returned
	From  string // the sender address the real client would have used
	Email Email
}

// SentSMS is one text message a recording client accepted.
type SentSMS struct {
	ID          string
	PhoneNumber string
	Message     string
}

// Outbox holds what a recording client was asked to send, in order.
// It is safe for concurrent use.
type Outbox struct {
	mutex  sync.Mutex
	lastID int
	emails []SentEmail
	sms    []SentSMS
}

// NewRecordingClient returns a Client that sends nothing and records every
// email and text message in the returned Outbox. It makes no network call and
// needs no credentials. WithEmailSender and WithSenderName set the From of the
// recorded emails.
func NewRecordingClient(opts ...ClientOption) (*Client, *Outbox) {
	outbox := &Outbox{}
	client := &Client{outbox: outbox}

	for _, opt := range opts {
		opt(client)
	}

	return client, outbox
}

// Emails returns a copy of the recorded emails, oldest first.
func (o *Outbox) Emails() []SentEmail {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	emails := make([]SentEmail, len(o.emails))
	for i := range o.emails {
		emails[i] = o.emails[i]
		emails[i].Email = cloneEmail(o.emails[i].Email)
	}

	return emails
}

// SMS returns a copy of the recorded text messages, oldest first.
func (o *Outbox) SMS() []SentSMS {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	return slices.Clone(o.sms)
}

// Reset forgets every recorded email and text message, for use between tests.
// Message IDs keep counting so they stay unique for the outbox's lifetime.
func (o *Outbox) Reset() {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	o.emails = nil
	o.sms = nil
}

// recordEmail records an email that passed validation. An email with
// attachments is first rendered as the real client would, so an email the real
// client refuses while building the MIME message is refused here too. A done
// ctx fails with its error, as the real client's call to AWS does.
func (c *Client) recordEmail(ctx context.Context, email Email) (string, error) {
	if len(email.Attachments) > 0 {
		_, err := c.buildRawEmail(email)
		if err != nil {
			return "", ez.Wrap(err)
		}
	}

	// The operation the real client would have called, so the error has the SDK's shape
	operation := "SendEmail"
	if len(email.Attachments) > 0 {
		operation = "SendRawEmail"
	}

	err := ctx.Err()
	if err != nil {
		return "", ez.Wrap(&smithy.OperationError{ServiceID: ses.ServiceID, OperationName: operation, Err: err})
	}

	return c.outbox.addEmail(c.getSenderAddress(), email), nil
}

// recordSMS records a text message. A done ctx fails with its error, as the
// real client's call to AWS does.
func (c *Client) recordSMS(ctx context.Context, phoneNumber string, message string) (string, error) {
	err := ctx.Err()
	if err != nil {
		return "", ez.Wrap(&smithy.OperationError{ServiceID: sns.ServiceID, OperationName: "Publish", Err: err})
	}

	return c.outbox.addSMS(phoneNumber, message), nil
}

func (o *Outbox) addEmail(from string, email Email) string {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	id := o.nextID()
	o.emails = append(o.emails, SentEmail{ID: id, From: from, Email: cloneEmail(email)})

	return id
}

func (o *Outbox) addSMS(phoneNumber string, message string) string {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	id := o.nextID()
	o.sms = append(o.sms, SentSMS{ID: id, PhoneNumber: phoneNumber, Message: message})

	return id
}

// nextID returns a new message ID. The caller holds the mutex.
func (o *Outbox) nextID() string {
	o.lastID++

	return fmt.Sprintf("recorded-%d", o.lastID)
}

// cloneEmail copies the email's slices and attachment data, so changes to the
// original do not reach the copy.
func cloneEmail(email Email) Email {
	email.Recipients = slices.Clone(email.Recipients)
	email.Attachments = slices.Clone(email.Attachments)

	for i := range email.Attachments {
		email.Attachments[i].Data = slices.Clone(email.Attachments[i].Data)
	}

	return email
}
