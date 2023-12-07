package ses

import (
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/vanclief/ez"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sns"
)

// SendSMS Send SMS using AWS SNS
func (c *Client) SendSMS(phoneNumber string, message string) (string, error) {
	const op = "Client.SendSMS"

	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(c.Region),
		Credentials: credentials.NewStaticCredentials(c.AccessKeyID, c.SecretAccessKey, ""),
	},
	)
	if err != nil {
		return "", ez.Wrap(op, err)
	}

	svc := sns.New(sess)

	params := &sns.PublishInput{
		PhoneNumber: aws.String(phoneNumber),
		Message:     aws.String(message),
	}

	resp, err := svc.Publish(params)
	if err != nil {
		return "", ez.Wrap(op, err)
	}

	return *resp.MessageId, nil
}
