package ses

import (
	"bytes"
	"io"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/vanclief/ez"
	"gopkg.in/gomail.v2"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ses"
)

// SendEmail delivery an email utilizing the AWS SES service
func (c *Client) SendEmail(recipient, subject, htmlBody string) (*ses.SendEmailOutput, error) {
	const op = "Client.SendEmail"

	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(c.Region),
		Credentials: credentials.NewStaticCredentials(c.AccessKeyID, c.SecretAccessKey, ""),
	},
	)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	svc := ses.New(sess)
	payload := &ses.SendEmailInput{
		Destination: &ses.Destination{
			CcAddresses: []*string{},
			ToAddresses: []*string{
				aws.String(recipient),
			},
		},
		Message: &ses.Message{
			Body: &ses.Body{
				Html: &ses.Content{
					Charset: aws.String(charSet),
					Data:    aws.String(htmlBody),
				},
				Text: &ses.Content{
					Charset: aws.String(charSet),
					Data:    aws.String(htmlBody),
				},
			},
			Subject: &ses.Content{
				Charset: aws.String(charSet),
				Data:    aws.String(subject),
			},
		},
		Source: aws.String(c.EmailSender),
	}

	res, err := svc.SendEmail(payload)
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
func (c *Client) SendEmailWithAttachment(recipient, subject, htmlBody string, attachments []Attachment) (*ses.SendRawEmailOutput, error) {
	const op = "Client.SendEmailWithAttachment"

	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(c.Region),
		Credentials: credentials.NewStaticCredentials(c.AccessKeyID, c.SecretAccessKey, ""),
	})
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	msg := gomail.NewMessage()
	msg.SetHeader("From", c.EmailSender)
	msg.SetHeader("To", recipient)
	msg.SetHeader("Subject", subject)
	msg.SetBody("text/html", htmlBody)

	for _, attachment := range attachments {
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

	svc := ses.New(sess)
	input := &ses.SendRawEmailInput{
		RawMessage: &ses.RawMessage{
			Data: rawEmail.Bytes(),
		},
	}

	return svc.SendRawEmail(input)
}
