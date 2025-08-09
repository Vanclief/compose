package ses

// ClientOption defines function type for client options
type ClientOption func(*Client)

// WithSessionTTL sets a custom session TTL in seconds
func WithSessionTTL(ttl int64) ClientOption {
	return func(c *Client) {
		c.sessionTTL = ttl
	}
}

// WithRefreshTTL sets a custom refresh TTL in seconds
func WithRefreshTTL(ttl int64) ClientOption {
	return func(c *Client) {
		c.refreshTTL = ttl
	}
}

// WithEmailSender sets the email sender
func WithEmailSender(emailSender string) ClientOption {
	return func(c *Client) {
		c.EmailSender = emailSender
	}
}

// WithSenderName sets the sender name
func WithSenderName(senderName string) ClientOption {
	return func(c *Client) {
		c.SenderName = senderName
	}
}

// WithPushNotificationARN sets the push notification ARN
func WithPushNotificationARN(arn string) ClientOption {
	return func(c *Client) {
		c.PushNotificationARN = arn
	}
}
