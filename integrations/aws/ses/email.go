package ses

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/vanclief/ez"
	"gopkg.in/gomail.v2"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"
)

// SendEmail delivery an email utilizing the AWS SES service
func (c *Client) SendEmail(ctx context.Context, recipient, subject, htmlBody string) (*ses.SendEmailOutput, error) {
	const op = "Client.SendEmail"

	svc, err := c.getSESService(ctx)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	payload := &ses.SendEmailInput{
		Destination: &types.Destination{
			CcAddresses: []string{},
			ToAddresses: []string{recipient},
		},
		Message: &types.Message{
			Body: &types.Body{
				Html: &types.Content{
					Charset: aws.String(charSet),
					Data:    aws.String(htmlBody),
				},
				Text: &types.Content{
					Charset: aws.String(charSet),
					Data:    aws.String(htmlBody),
				},
			},
			Subject: &types.Content{
				Charset: aws.String(charSet),
				Data:    aws.String(subject),
			},
		},
		Source: aws.String(c.getSenderAddress()),
	}

	res, err := svc.SendEmail(ctx, payload)
	if err != nil && isSessionError(err) {
		// Refresh session
		if refreshErr := c.initSession(ctx); refreshErr != nil {
			return nil, ez.Wrap(op, err) // Return original error if refresh fails
		}

		// Try once more with refreshed session
		svc, svcErr := c.getSESService(ctx)
		if svcErr != nil {
			return nil, ez.Wrap(op, svcErr)
		}
		return svc.SendEmail(ctx, payload)
	}

	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return res, nil
}

type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// SendEmailWithAttachment delivers an email with attachments using AWS SES
func (c *Client) SendEmailWithAttachment(ctx context.Context, recipient, subject, htmlBody string, attachments []Attachment) (*ses.SendRawEmailOutput, error) {
	const op = "Client.SendEmailWithAttachment"

	svc, err := c.getSESService(ctx)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	msg := gomail.NewMessage()
	msg.SetHeader("From", c.getSenderAddress())
	msg.SetHeader("To", recipient)
	msg.SetHeader("Subject", subject)
	msg.SetBody("text/html", htmlBody)

	for i := range attachments {
		attachment := attachments[i]
		msg.Attach(attachment.Filename,
			gomail.SetCopyFunc(func(w io.Writer) error {
				_, err := w.Write(attachment.Data)
				return err
			}),
			gomail.SetHeader(map[string][]string{
				"Content-Type": {attachment.ContentType},
			}),
		)
	}

	var rawEmail bytes.Buffer
	_, err = msg.WriteTo(&rawEmail)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	input := &ses.SendRawEmailInput{
		RawMessage: &types.RawMessage{Data: rawEmail.Bytes()},
	}

	output, err := svc.SendRawEmail(ctx, input)
	if err != nil && isSessionError(err) {
		// Refresh session
		if refreshErr := c.initSession(ctx); refreshErr != nil {
			return nil, ez.Wrap(op, err) // Return original error if refresh fails
		}

		// Try once more with refreshed session
		svc, svcErr := c.getSESService(ctx)
		if svcErr != nil {
			return nil, ez.Wrap(op, svcErr)
		}
		return svc.SendRawEmail(ctx, input)
	}

	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return output, nil
}

func (c *Client) getSenderAddress() string {
	if c.SenderName == "" {
		return c.EmailSender
	}
	return fmt.Sprintf("%s <%s>", c.SenderName, c.EmailSender)
}
