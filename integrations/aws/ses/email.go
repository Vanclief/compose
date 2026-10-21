package ses

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"path/filepath"
	"strings"

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
	err := validateEmail(email)
	if err != nil {
		return "", err
	}

	if len(email.Attachments) > 0 {
		return c.sendRawEmail(ctx, email)
	}

	return c.sendSimpleEmail(ctx, email)
}

func validateEmail(email Email) error {
	if len(email.Recipients) == 0 {
		return ez.New(ez.EINVALID, "recipients cannot be empty", nil)
	}

	if len(email.Recipients) > maxEmailRecipients {
		return ez.New(ez.EINVALID, "recipients cannot exceed 50", nil)
	}

	for i := range email.Recipients {
		if email.Recipients[i] == "" {
			return ez.New(ez.EINVALID, "recipient cannot be empty", nil)
		}
	}

	if email.HTMLBody == "" && email.TextBody == "" {
		return ez.New(ez.EINVALID, "email body cannot be empty", nil)
	}

	for i := range email.Attachments {
		err := validateAttachment(email.Attachments[i])
		if err != nil {
			return err
		}
	}

	return nil
}

// validateAttachment rejects an empty filename and a content type containing
// CR or LF, since the content type is written raw into the MIME part headers.
// An empty content type is allowed: buildRawEmail derives one from the
// filename extension. The filename may contain any character: buildRawEmail
// encodes it into the Content-Disposition header with mime.FormatMediaType.
func validateAttachment(attachment Attachment) error {
	if attachment.Filename == "" {
		return ez.New(ez.EINVALID, "attachment filename cannot be empty", nil)
	}

	if strings.ContainsAny(attachment.ContentType, "\r\n") {
		return ez.New(ez.EINVALID, "attachment content type contains invalid characters", nil)
	}

	return nil
}

func (c *Client) sendSimpleEmail(ctx context.Context, email Email) (string, error) {
	svc, err := c.getSESService(ctx)
	if err != nil {
		return "", ez.Wrap(err)
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
			return "", ez.Wrap(sendErr)
		}

		// Try once more with refreshed session
		svc, err = c.getSESService(ctx)
		if err != nil {
			return "", ez.Wrap(err)
		}

		res, err = svc.SendEmail(ctx, payload)
	}

	if err != nil {
		return "", ez.Wrap(err)
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
	svc, err := c.getSESService(ctx)
	if err != nil {
		return "", ez.Wrap(err)
	}

	rawEmail, err := c.buildRawEmail(email)
	if err != nil {
		return "", ez.Wrap(err)
	}

	input := &ses.SendRawEmailInput{
		RawMessage: &types.RawMessage{Data: rawEmail},
	}

	output, err := svc.SendRawEmail(ctx, input)
	if err != nil && isSessionError(err) {
		sendErr := err

		// Refresh session
		err = c.initSession(ctx)
		if err != nil {
			return "", ez.Wrap(sendErr)
		}

		// Try once more with refreshed session
		svc, err = c.getSESService(ctx)
		if err != nil {
			return "", ez.Wrap(err)
		}

		output, err = svc.SendRawEmail(ctx, input)
	}

	if err != nil {
		return "", ez.Wrap(err)
	}

	return aws.ToString(output.MessageId), nil
}

// buildRawEmail renders the email with its attachments as a MIME message
func (c *Client) buildRawEmail(email Email) ([]byte, error) {
	msg := gomail.NewMessage()
	msg.SetHeader("From", c.getSenderAddress())
	msg.SetHeader("To", email.Recipients...)
	msg.SetHeader("Subject", email.Subject)
	setRawEmailBody(msg, email)

	for i := range email.Attachments {
		attachment := email.Attachments[i]

		// Setting Content-Disposition stops gomail from writing the raw
		// filename into the header. FormatMediaType quotes and escapes it, and
		// percent-encodes CR, LF and non-ASCII characters per RFC 2231.
		disposition := mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Filename})
		if disposition == "" {
			return nil, ez.New(ez.EINVALID, "attachment filename cannot be encoded", nil)
		}

		msg.Attach(attachment.Filename,
			gomail.SetCopyFunc(func(w io.Writer) error {
				_, err := w.Write(attachment.Data)
				return err
			}),
			gomail.SetHeader(map[string][]string{
				"Content-Type":        {attachmentContentType(attachment)},
				"Content-Disposition": {disposition},
			}),
		)
	}

	var rawEmail bytes.Buffer
	_, err := msg.WriteTo(&rawEmail)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return rawEmail.Bytes(), nil
}

// attachmentContentType returns the attachment's content type, falling back to
// the type registered for its filename extension and then to
// application/octet-stream. buildRawEmail always sets the Content-Type header,
// so gomail never applies its own extension default.
func attachmentContentType(attachment Attachment) string {
	if attachment.ContentType != "" {
		return attachment.ContentType
	}

	contentType := mime.TypeByExtension(filepath.Ext(attachment.Filename))
	if contentType != "" {
		return contentType
	}

	return "application/octet-stream"
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
