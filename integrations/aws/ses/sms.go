package ses

import (
	"github.com/vanclief/ez"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sns"
)

// SendSMS Send SMS using AWS SNS
func (c *Client) SendSMS(phoneNumber string, message string) (string, error) {
	const op = "Client.SendSMS"

	svc, err := c.getSNSService()
	if err != nil {
		return "", ez.Wrap(op, err)
	}

	params := &sns.PublishInput{
		PhoneNumber: aws.String(phoneNumber),
		Message:     aws.String(message),
	}

	resp, err := svc.Publish(params)
	if err != nil && isSessionError(err) {
		// Refresh session
		if refreshErr := c.initSession(); refreshErr != nil {
			return "", ez.Wrap(op, err) // Return original error if refresh fails
		}

		// Try once more with refreshed session
		resp, err = svc.Publish(params)
	}

	if err != nil {
		return "", ez.Wrap(op, err)
	}

	return *resp.MessageId, nil
}
