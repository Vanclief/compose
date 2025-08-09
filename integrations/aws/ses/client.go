package ses

import (
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/aws/aws-sdk-go/service/sns"
	"github.com/vanclief/ez"
)

const (
	charSet                  string = "UTF-8"
	defaultSessionTTL        int64  = 3600 // Session TTL in seconds (1 hour)
	defaultSessionRefreshTTL int64  = 300  // Time to refresh session before it expires (5 minutes)
)

// Client contains the AWS configuration and methods
type Client struct {
	Region              string
	AccessKeyID         string
	SecretAccessKey     string
	EmailSender         string
	SenderName          string
	PushNotificationARN string

	// Session management
	awsSession    *session.Session
	sesSvc        *ses.SES
	snsSvc        *sns.SNS
	sessionExpiry time.Time
	sessionTTL    int64
	refreshTTL    int64
	sessionMutex  sync.RWMutex
}

// NewClient creates and returns a new AWS Client from configuration
func NewClient(region, accessKey, secretKey string, opts ...ClientOption) (*Client, error) {
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
		sessionTTL:      defaultSessionTTL,
		refreshTTL:      defaultSessionRefreshTTL,
	}

	// Apply options
	for _, opt := range opts {
		opt(client)
	}

	// Initialize sessions
	if err := client.initSession(); err != nil {
		return nil, ez.Wrap(op, err)
	}

	return client, nil
}

// initSession initializes the AWS session and services
func (c *Client) initSession() error {
	const op = "Client.initSession"

	c.sessionMutex.Lock()
	defer c.sessionMutex.Unlock()

	// Create new AWS session
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(c.Region),
		Credentials: credentials.NewStaticCredentials(c.AccessKeyID, c.SecretAccessKey, ""),
	})
	if err != nil {
		return ez.Wrap(op, err)
	}

	// Store the AWS session
	c.awsSession = sess

	// Initialize SES and SNS services from the same session
	c.sesSvc = ses.New(sess)
	c.snsSvc = sns.New(sess)

	// Set expiry time
	c.sessionExpiry = time.Now().Add(time.Duration(c.sessionTTL) * time.Second)

	return nil
}

// ensureValidSession checks if the session is valid and refreshes it if needed
func (c *Client) ensureValidSession() error {
	const op = "Client.ensureValidSession"

	c.sessionMutex.RLock()
	sessionValid := c.awsSession != nil && time.Until(c.sessionExpiry) > time.Duration(c.refreshTTL)*time.Second
	c.sessionMutex.RUnlock()

	// Refresh session if it's nil or about to expire
	if !sessionValid {
		if err := c.initSession(); err != nil {
			return ez.Wrap(op, err)
		}
	}

	return nil
}

// isSessionError determines if an AWS error indicates that the session is invalid
func isSessionError(err error) bool {
	if err == nil {
		return false
	}

	// Check if it's an AWS error
	awsErr, ok := err.(awserr.Error)
	if !ok {
		return false
	}

	// Common session-related error codes
	switch awsErr.Code() {
	case
		"ExpiredToken",                // Token has expired
		"UnrecognizedClientException", // Session issue
		"TokenRefreshRequired",        // Token refresh needed
		"RequestExpired":              // Request expired during processing
		return true
	}

	return false
}

// getSESService returns the SES service, ensuring a valid session
func (c *Client) getSESService() (*ses.SES, error) {
	if err := c.ensureValidSession(); err != nil {
		return nil, err
	}
	return c.sesSvc, nil
}

// getSNSService returns the SNS service, ensuring a valid session
func (c *Client) getSNSService() (*sns.SNS, error) {
	if err := c.ensureValidSession(); err != nil {
		return nil, err
	}
	return c.snsSvc, nil
}
