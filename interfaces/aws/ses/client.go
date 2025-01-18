package ses

import "github.com/vanclief/ez"

const (
	charSet string = "UTF-8"
)

// Client contains the AWS configuration and methods
type Client struct {
	Region              string
	AccessKeyID         string
	SecretAccessKey     string
	EmailSender         string
	SenderName          string
	PushNotificationARN string
}

// NewClient creates and returns a new AWS Client from configuration
func NewClient(region, accessKey, secretKey string) (*Client, error) {
	const op = "aws.NewClient"

	if region == "" {
		return nil, ez.New(op, ez.EINVALID, "Region cannot be empty", nil)
	} else if accessKey == "" {
		return nil, ez.New(op, ez.EINVALID, "AccessKey cannot be empty", nil)
	} else if secretKey == "" {
		return nil, ez.New(op, ez.EINVALID, "SecretKey cannot be empty", nil)
	}

	client := &Client{
		Region:          region,
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
	}

	return client, nil
}

func (c *Client) SetEmailSender(emailSender string) {
	c.EmailSender = emailSender
}

func (c *Client) SetSenderName(senderName string) {
	c.SenderName = senderName
}

func (c *Client) SetPushNotificationARN(arn string) {
	c.PushNotificationARN = arn
}
