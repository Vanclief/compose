package ses

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/vanclief/ez"
)

// SendSMS Send SMS using AWS SNS
func (c *Client) SendSMS(ctx context.Context, phoneNumber string, message string) (string, error) {
	const op = "Client.SendSMS"

	svc, err := c.getSNSService(ctx)
	if err != nil {
		return "", ez.Wrap(op, err)
	}

	params := &sns.PublishInput{
		PhoneNumber: aws.String(phoneNumber),
		Message:     aws.String(message),
	}

	resp, err := svc.Publish(ctx, params)
	if err != nil && isSessionError(err) {
		// Refresh session
		if refreshErr := c.initSession(ctx); refreshErr != nil {
			return "", ez.Wrap(op, err) // Return original error if refresh fails
		}

		// Try once more with refreshed session
		svc, svcErr := c.getSNSService(ctx)
		if svcErr != nil {
			return "", ez.Wrap(op, svcErr)
		}

		resp, err = svc.Publish(ctx, params)
	}

	if err != nil {
		return "", ez.Wrap(op, err)
	}

	return aws.ToString(resp.MessageId), nil
}
