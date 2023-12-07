package ses

import (
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/vanclief/ez"

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
