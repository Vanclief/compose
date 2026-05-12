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

// SES limits each message to 50 recipients across To, Cc, and Bcc.
const maxEmailRecipients = 50

type Email struct {
	Recipients  []string
	Subject     string
	HTMLBody    string
	TextBody    string
	Attachments []Attachment
}

type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// SendEmail delivers an email utilizing the AWS SES service
func (c *Client) SendEmail(ctx context.Context, email Email) (string, error) {
	const op = "Client.SendEmail"

	err := validateEmail(op, email)
	if err != nil {
		return "", err
	}

	if len(email.Attachments) > 0 {
		return c.sendRawEmail(ctx, email)
	}

	return c.sendSimpleEmail(ctx, email)
}

func validateEmail(op string, email Email) error {
	if len(email.Recipients) == 0 {
		return ez.New(op, ez.EINVALID, "recipients cannot be empty", nil)
	}

	if len(email.Recipients) > maxEmailRecipients {
		return ez.New(op, ez.EINVALID, "recipients cannot exceed 50", nil)
	}

	for i := range email.Recipients {
		if email.Recipients[i] == "" {
			return ez.New(op, ez.EINVALID, "recipient cannot be empty", nil)
		}
	}

	if email.HTMLBody == "" && email.TextBody == "" {
		return ez.New(op, ez.EINVALID, "email body cannot be empty", nil)
	}

	return nil
}

func (c *Client) sendSimpleEmail(ctx context.Context, email Email) (string, error) {
	const op = "Client.sendSimpleEmail"

	svc, err := c.getSESService(ctx)
	if err != nil {
		return "", ez.Wrap(op, err)
	}

	payload := &ses.SendEmailInput{
		Destination: &types.Destination{
			ToAddresses: email.Recipients,
		},
		Message: &types.Message{
			Body: buildEmailBody(email),
			Subject: &types.Content{
				Charset: aws.String(charSet),
				Data:    aws.String(email.Subject),
			},
		},
		Source: aws.String(c.getSenderAddress()),
	}

	res, err := svc.SendEmail(ctx, payload)
	if err != nil && isSessionError(err) {
		sendErr := err

		// Refresh session
		err = c.initSession(ctx)
		if err != nil {
			return "", ez.Wrap(op, sendErr)
		}

		// Try once more with refreshed session
		svc, err = c.getSESService(ctx)
		if err != nil {
			return "", ez.Wrap(op, err)
		}

		res, err = svc.SendEmail(ctx, payload)
	}

	if err != nil {
		return "", ez.Wrap(op, err)
	}

	return aws.ToString(res.MessageId), nil
}

func buildEmailBody(email Email) *types.Body {
	body := &types.Body{}

	if email.HTMLBody != "" {
		body.Html = &types.Content{
			Charset: aws.String(charSet),
			Data:    aws.String(email.HTMLBody),
		}
	}

	if email.TextBody != "" {
		body.Text = &types.Content{
			Charset: aws.String(charSet),
			Data:    aws.String(email.TextBody),
		}
	}

	return body
}

func (c *Client) sendRawEmail(ctx context.Context, email Email) (string, error) {
	const op = "Client.sendRawEmail"

	svc, err := c.getSESService(ctx)
	if err != nil {
		return "", ez.Wrap(op, err)
	}

	msg := gomail.NewMessage()
	msg.SetHeader("From", c.getSenderAddress())
	msg.SetHeader("To", email.Recipients...)
	msg.SetHeader("Subject", email.Subject)
	setRawEmailBody(msg, email)

	for i := range email.Attachments {
		attachment := email.Attachments[i]
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
		return "", ez.Wrap(op, err)
	}

	input := &ses.SendRawEmailInput{
		RawMessage: &types.RawMessage{Data: rawEmail.Bytes()},
	}

	output, err := svc.SendRawEmail(ctx, input)
	if err != nil && isSessionError(err) {
		sendErr := err

		// Refresh session
		err = c.initSession(ctx)
		if err != nil {
			return "", ez.Wrap(op, sendErr)
		}

		// Try once more with refreshed session
		svc, err = c.getSESService(ctx)
		if err != nil {
			return "", ez.Wrap(op, err)
		}

		output, err = svc.SendRawEmail(ctx, input)
	}

	if err != nil {
		return "", ez.Wrap(op, err)
	}

	return aws.ToString(output.MessageId), nil
}

func setRawEmailBody(msg *gomail.Message, email Email) {
	switch {
	case email.TextBody != "" && email.HTMLBody != "":
		msg.SetBody("text/plain", email.TextBody)
		msg.AddAlternative("text/html", email.HTMLBody)
	case email.TextBody != "":
		msg.SetBody("text/plain", email.TextBody)
	case email.HTMLBody != "":
		msg.SetBody("text/html", email.HTMLBody)
	}
}

func (c *Client) getSenderAddress() string {
	if c.SenderName == "" {
		return c.EmailSender
	}
	return fmt.Sprintf("%s <%s>", c.SenderName, c.EmailSender)
}
