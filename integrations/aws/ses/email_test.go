package ses

import (
	"bytes"
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"gopkg.in/gomail.v2"
)

func (suite *TestSuite) TestSendEmail() {
	msgID, err := suite.client.SendEmail(context.Background(), Email{
		Recipients: []string{suite.testEmail},
		Subject:    "Compose SES Test Email",
		HTMLBody:   `<h1>Hello World</h1>`,
		TextBody:   "Hello World",
	})
	suite.NotEmpty(msgID)
	suite.Nil(err)
}

func TestBuildEmailBodyOmitsTextWhenFallbackIsEmpty(t *testing.T) {
	body := buildEmailBody(Email{
		HTMLBody: `<h1>Hello World</h1>`,
	})

	assert.NotNil(t, body.Html)
	assert.Nil(t, body.Text)
	assert.Equal(t, `<h1>Hello World</h1>`, aws.ToString(body.Html.Data))
}

func TestValidateEmailRejectsTooManyRecipients(t *testing.T) {
	recipients := make([]string, maxEmailRecipients+1)
	for i := range recipients {
		recipients[i] = "recipient@example.com"
	}

	err := validateEmail("test", Email{
		Recipients: recipients,
		HTMLBody:   `<h1>Hello World</h1>`,
	})

	assert.Error(t, err)
}

func TestBuildEmailBodyUsesProvidedTextFallback(t *testing.T) {
	body := buildEmailBody(Email{
		HTMLBody: `<h1>Hello World</h1>`,
		TextBody: "Hello World",
	})

	assert.NotNil(t, body.Html)
	assert.NotNil(t, body.Text)
	assert.Equal(t, `<h1>Hello World</h1>`, aws.ToString(body.Html.Data))
	assert.Equal(t, "Hello World", aws.ToString(body.Text.Data))
}

func TestSetRawEmailBodyOmitsTextWhenFallbackIsEmpty(t *testing.T) {
	msg := gomail.NewMessage()
	msg.SetHeader("From", "sender@example.com")
	msg.SetHeader("To", "recipient@example.com")
	msg.SetHeader("Subject", "Test")
	setRawEmailBody(msg, Email{
		HTMLBody: `<h1>Hello World</h1>`,
	})

	var rawEmail bytes.Buffer
	_, err := msg.WriteTo(&rawEmail)

	assert.NoError(t, err)
	assert.Contains(t, rawEmail.String(), "Content-Type: text/html")
	assert.NotContains(t, rawEmail.String(), "Content-Type: text/plain")
}

func TestSetRawEmailBodyUsesProvidedTextFallback(t *testing.T) {
	msg := gomail.NewMessage()
	msg.SetHeader("From", "sender@example.com")
	msg.SetHeader("To", "recipient@example.com")
	msg.SetHeader("Subject", "Test")
	setRawEmailBody(msg, Email{
		HTMLBody: `<h1>Hello World</h1>`,
		TextBody: "Hello World",
	})

	var rawEmail bytes.Buffer
	_, err := msg.WriteTo(&rawEmail)

	assert.NoError(t, err)
	assert.Contains(t, rawEmail.String(), "Content-Type: text/plain")
	assert.Contains(t, rawEmail.String(), "Hello World")
	assert.Contains(t, rawEmail.String(), "Content-Type: text/html")
	assert.Contains(t, rawEmail.String(), "<h1>Hello World</h1>")
}
