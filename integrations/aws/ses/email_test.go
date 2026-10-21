package ses

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/vanclief/ez"
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

	err := validateEmail(Email{
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

func TestValidateAttachmentRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name       string
		attachment Attachment
	}{
		{
			name:       "empty filename",
			attachment: Attachment{ContentType: "application/pdf"},
		},
		{
			name:       "content type with CRLF",
			attachment: Attachment{Filename: "report.pdf", ContentType: "application/pdf\r\nX-Evil: 1"},
		},
		{
			name:       "content type with CR",
			attachment: Attachment{Filename: "report.pdf", ContentType: "application/pdf\rX-Evil: 1"},
		},
		{
			name:       "content type with LF",
			attachment: Attachment{Filename: "report.pdf", ContentType: "application/pdf\nX-Evil: 1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAttachment(tt.attachment)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		})
	}
}

func TestValidateAttachmentAcceptsAnyFilename(t *testing.T) {
	filenames := []string{
		`Reporte financiero Residencial "Los Pinos" 01-2026.pdf`,
		"report.pdf\"\r\nX-Evil: 1",
		"report.pdf\rX-Evil: 1",
		"report.pdf\nX-Evil: 1",
	}

	for _, filename := range filenames {
		err := validateAttachment(Attachment{Filename: filename, ContentType: "application/pdf"})
		assert.NoError(t, err, filename)
	}
}

func TestSendEmailRejectsInjectedContentTypeBeforeNetwork(t *testing.T) {
	// A zero Client has no AWS session, so reaching AWS would fail with a
	// different error. EINVALID proves validation runs before any network call.
	client := &Client{EmailSender: "sender@example.com"}

	_, err := client.SendEmail(context.Background(), Email{
		Recipients: []string{"recipient@example.com"},
		Subject:    "Test",
		TextBody:   "Hello World",
		Attachments: []Attachment{
			{Filename: "report.pdf", ContentType: "application/pdf\r\nX-Evil: 1", Data: []byte("data")},
		},
	})
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

// buildAttachmentEmail validates and renders an email with one PDF attachment
// and returns the MIME message with folded header lines unfolded.
func buildAttachmentEmail(t *testing.T, filename string) string {
	t.Helper()

	return buildEmailWithAttachment(t, Attachment{Filename: filename, ContentType: "application/pdf", Data: []byte("data")})
}

// buildEmailWithAttachment validates and renders an email with the given
// attachment and returns the MIME message with folded header lines unfolded.
func buildEmailWithAttachment(t *testing.T, attachment Attachment) string {
	t.Helper()

	email := Email{
		Recipients:  []string{"recipient@example.com"},
		Subject:     "Test",
		TextBody:    "Hello World",
		Attachments: []Attachment{attachment},
	}

	err := validateEmail(email)
	assert.NoError(t, err)

	client := &Client{EmailSender: "sender@example.com"}
	rawEmail, err := client.buildRawEmail(email)
	assert.NoError(t, err)

	return strings.ReplaceAll(string(rawEmail), "\r\n ", " ")
}

// headerLines returns every line of the MIME message that starts with prefix.
func headerLines(rawEmail string, prefix string) []string {
	var lines []string
	for _, line := range strings.Split(rawEmail, "\r\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}

	return lines
}

func TestBuildRawEmailWritesValidAttachmentHeaders(t *testing.T) {
	rawEmail := buildAttachmentEmail(t, "report.pdf")

	assert.Equal(t, []string{"Content-Type: application/pdf"}, headerLines(rawEmail, "Content-Type: application/pdf"))
	assert.Equal(t, []string{"Content-Disposition: attachment; filename=report.pdf"}, headerLines(rawEmail, "Content-Disposition:"))
}

func TestBuildRawEmailEscapesQuotedFilename(t *testing.T) {
	rawEmail := buildAttachmentEmail(t, `Reporte financiero Residencial "Los Pinos" 01-2026.pdf`)

	assert.Equal(t,
		[]string{`Content-Disposition: attachment; filename="Reporte financiero Residencial \"Los Pinos\" 01-2026.pdf"`},
		headerLines(rawEmail, "Content-Disposition:"),
	)
	assert.NotContains(t, rawEmail, `Residencial "Los Pinos"`)
	assert.Equal(t, []string{"Content-Type: application/pdf"}, headerLines(rawEmail, "Content-Type: application/pdf"))
}

func TestBuildRawEmailEncodesFilenameHeaderInjection(t *testing.T) {
	rawEmail := buildAttachmentEmail(t, "report.pdf\"\r\nX-Evil: 1")

	assert.Empty(t, headerLines(rawEmail, "X-Evil"))
	assert.Equal(t,
		[]string{"Content-Disposition: attachment; filename*=utf-8''report.pdf%22%0D%0AX-Evil%3A%201"},
		headerLines(rawEmail, "Content-Disposition:"),
	)
	assert.Equal(t, []string{"Content-Type: application/pdf"}, headerLines(rawEmail, "Content-Type: application/pdf"))
	assert.NotContains(t, rawEmail, `name="`)
}

func TestBuildRawEmailDerivesEmptyContentTypeFromExtension(t *testing.T) {
	rawEmail := buildEmailWithAttachment(t, Attachment{Filename: "report.pdf", Data: []byte("data")})

	assert.Equal(t, []string{"Content-Type: application/pdf"}, headerLines(rawEmail, "Content-Type: application/pdf"))
	assertNoBareContentType(t, rawEmail)
}

func TestBuildRawEmailFallsBackToOctetStream(t *testing.T) {
	rawEmail := buildEmailWithAttachment(t, Attachment{Filename: "report.zzunknown", Data: []byte("data")})

	assert.Equal(t, []string{"Content-Type: application/octet-stream"}, headerLines(rawEmail, "Content-Type: application/octet-stream"))
	assertNoBareContentType(t, rawEmail)
}

func TestBuildRawEmailUsesExplicitContentTypeVerbatim(t *testing.T) {
	rawEmail := buildEmailWithAttachment(t, Attachment{Filename: "report.pdf", ContentType: "text/csv", Data: []byte("data")})

	assert.Equal(t, []string{"Content-Type: text/csv"}, headerLines(rawEmail, "Content-Type: text/csv"))
	assert.Empty(t, headerLines(rawEmail, "Content-Type: application/pdf"))
	assertNoBareContentType(t, rawEmail)
}

// assertNoBareContentType fails when any MIME part has an empty Content-Type.
func assertNoBareContentType(t *testing.T, rawEmail string) {
	t.Helper()

	assert.NotContains(t, rawEmail, "Content-Type:\r\n")
	assert.NotContains(t, rawEmail, "Content-Type: \r\n")
}
