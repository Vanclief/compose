package ses

import (
	"bytes"
	"context"
	"math/rand"
	"mime"
	"net/mail"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// senderAddresses returns, for a client with the given sender address and
// name, the Source the simple path sends, the From the recording client stores
// and the From header value the raw path writes, along with the raw message
// with folded header lines unfolded. gomail folds a header line by replacing a
// space with CRLF and a space, so unfolding gives back the value it was given.
func senderAddresses(t *testing.T, sender string, name string) ([]string, string) {
	t.Helper()

	email := Email{
		Recipients:  []string{"recipient@example.com"},
		Subject:     "Test",
		TextBody:    "Hello World",
		Attachments: []Attachment{{Filename: "report.pdf", ContentType: "application/pdf", Data: []byte("data")}},
	}

	client, outbox := NewRecordingClient(WithEmailSender(sender), WithSenderName(name))
	_, err := client.SendEmail(context.Background(), email)
	require.NoError(t, err)

	emails := outbox.Emails()
	require.Len(t, emails, 1)

	rawEmail, err := client.buildRawEmail(email)
	require.NoError(t, err)

	_, err = mail.ReadMessage(bytes.NewReader(rawEmail))
	require.NoError(t, err)

	unfolded := strings.ReplaceAll(string(rawEmail), "\r\n ", " ")
	from := headerLines(unfolded, "From: ")
	require.Len(t, from, 1)

	return []string{client.getSenderAddress(), emails[0].From, strings.TrimPrefix(from[0], "From: ")}, unfolded
}

// isMasterSenderGood reports whether the name and address written as they are
// make a From value of printable ASCII and tabs that parses to the address.
func isMasterSenderGood(master string, address string) bool {
	if !isUnencodedHeaderText(master) {
		return false
	}

	parsed, err := mail.ParseAddress(master)

	return err == nil && parsed.Address == address
}

func TestSenderAddressWritesAValidFromAsWritten(t *testing.T) {
	tests := []struct {
		name     string
		wantName string
	}{
		{name: "Emma", wantName: "Emma"},
		{name: "Acme Inc.", wantName: "Acme Inc."},
		{name: "John Q. Public", wantName: "John Q. Public"},
		{name: "Emma ", wantName: "Emma"},
		{name: `Acme "Best" Co`, wantName: "Acme Best Co"},
		{name: "=?utf-8?q?PayPal?=", wantName: "PayPal"},
		{name: mime.QEncoding.Encode("utf-8", "José Inc."), wantName: "José Inc."},
		{name: mime.QEncoding.Encode("utf-8", "Condominios Pérez S.A. de C.V."), wantName: "Condominios Pérez S.A. de C.V."},
		{name: mime.BEncoding.Encode("utf-8", "José Pérez"), wantName: "José Pérez"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.name + " <no-reply@example.com>"

			addresses, _ := senderAddresses(t, "no-reply@example.com", tt.name)
			for _, address := range addresses {
				require.Equal(t, want, address)

				parsed, err := mail.ParseAddress(address)
				require.NoError(t, err)
				require.Equal(t, &mail.Address{Name: tt.wantName, Address: "no-reply@example.com"}, parsed)
			}
		})
	}
}

func TestSenderAddressParsesBackToTheNameAndAddress(t *testing.T) {
	longName := strings.Repeat("Pérez ", 21) + "Ruiz"
	require.Equal(t, 130, utf8.RuneCountInString(longName))

	tests := []struct {
		name   string
		sender string
		want   *mail.Address
	}{
		{name: "Acme, Inc.", sender: "no-reply@example.com", want: &mail.Address{Name: "Acme, Inc.", Address: "no-reply@example.com"}},
		{name: "José Pérez", sender: "no-reply@example.com", want: &mail.Address{Name: "José Pérez", Address: "no-reply@example.com"}},
		{name: `A\B`, sender: "no-reply@example.com", want: &mail.Address{Name: `A\B`, Address: "no-reply@example.com"}},
		{name: `Pérez\Co`, sender: "no-reply@example.com", want: &mail.Address{Name: `Pérez\Co`, Address: "no-reply@example.com"}},
		{name: "user@host", sender: "no-reply@example.com", want: &mail.Address{Name: "user@host", Address: "no-reply@example.com"}},
		{name: "<x>", sender: "no-reply@example.com", want: &mail.Address{Name: "<x>", Address: "no-reply@example.com"}},
		{name: "Evil\r\nBcc: x@y.com", sender: "no-reply@example.com", want: &mail.Address{Name: "Evil\r\nBcc: x@y.com", Address: "no-reply@example.com"}},
		{name: longName, sender: "no-reply@example.com", want: &mail.Address{Name: longName, Address: "no-reply@example.com"}},
		{name: "Acme, Inc.", sender: `"no reply"@example.com`, want: &mail.Address{Name: "Acme, Inc.", Address: "no reply@example.com"}},
	}

	for _, tt := range tests {
		t.Run(tt.name+" "+tt.sender, func(t *testing.T) {
			require.False(t, isMasterSenderGood(tt.name+" <"+tt.sender+">", tt.want.Address))

			addresses, rawEmail := senderAddresses(t, tt.sender, tt.name)
			for _, address := range addresses {
				parsed, err := mail.ParseAddress(address)
				require.NoError(t, err)
				require.Equal(t, tt.want, parsed)
			}

			require.Empty(t, headerLines(rawEmail, "Bcc:"))
		})
	}
}

func TestSenderAddressOfGeneratedNames(t *testing.T) {
	alphabet := []rune{'a', 'b', '.', ',', '(', ')', '<', '>', '@', '\\', '"', '=', '?', 'é', ' ', '\t'}
	random := rand.New(rand.NewSource(1))

	for i := 0; i < 3000; i++ {
		runes := make([]rune, 1+random.Intn(12))
		for j := range runes {
			runes[j] = alphabet[random.Intn(len(alphabet))]
		}
		name := string(runes)

		master := name + " <no-reply@example.com>"
		addresses, _ := senderAddresses(t, "no-reply@example.com", name)
		for _, address := range addresses {
			if isMasterSenderGood(master, "no-reply@example.com") {
				require.Equal(t, master, address, "name %q", name)
				continue
			}

			parsed, err := mail.ParseAddress(address)
			require.NoError(t, err, "name %q", name)
			require.Equal(t, &mail.Address{Name: name, Address: "no-reply@example.com"}, parsed, "name %q", name)
		}
	}
}

func TestSenderAddressWithoutNameIsTheBareAddress(t *testing.T) {
	client := &Client{EmailSender: "no-reply@example.com"}
	require.Equal(t, "no-reply@example.com", client.getSenderAddress())
}

// sendRecipients sends an email with an attachment to recipients through a
// recording client and returns it rendered by buildRawEmail. It checks that
// the caller's slice and the recorded email keep the recipients as written.
func sendRecipients(t *testing.T, recipients []string) []byte {
	t.Helper()

	client, outbox := NewRecordingClient(WithEmailSender("sender@example.com"))

	written := append([]string{}, recipients...)
	email := Email{
		Recipients:  written,
		Subject:     "Report",
		TextBody:    "Attached",
		Attachments: []Attachment{{Filename: "report.pdf", ContentType: "application/pdf", Data: []byte("data")}},
	}

	_, err := client.SendEmail(context.Background(), email)
	require.NoError(t, err)

	rawEmail, err := client.buildRawEmail(email)
	require.NoError(t, err)

	require.Equal(t, recipients, written)

	emails := outbox.Emails()
	require.Len(t, emails, 1)
	require.Equal(t, recipients, emails[0].Email.Recipients)

	return rawEmail
}

// gomailToHeader returns the To header lines gomail writes when it is given
// the recipients with no formatting.
func gomailToHeader(t *testing.T, recipients []string) []string {
	t.Helper()

	msg := gomail.NewMessage()
	msg.SetHeader("From", "sender@example.com")
	msg.SetHeader("To", append([]string{}, recipients...)...)
	msg.SetBody("text/plain", "Attached")

	var rawEmail bytes.Buffer
	_, err := msg.WriteTo(&rawEmail)
	require.NoError(t, err)

	return headerLines(rawEmail.String(), "To:")
}

func TestBuildRawEmailWritesASCIIRecipientsAsWritten(t *testing.T) {
	recipients := []string{
		"plain@example.com",
		"Bob <bob@example.com>",
		"Friends: Bob <bob@example.com>;",
		"bob@example.com (Bob)",
		`"john doe"@example.com`,
		`Bob <"john doe"@example.com>`,
		`"a\"b"@example.com`,
	}

	for _, recipient := range recipients {
		t.Run(recipient, func(t *testing.T) {
			rawEmail := sendRecipients(t, []string{recipient})

			want := []string{"To: " + recipient}
			require.Equal(t, want, headerLines(string(rawEmail), "To:"))
			require.Equal(t, want, gomailToHeader(t, []string{recipient}))
		})
	}
}

func TestBuildRawEmailWritesANonASCIIAddressAsGomailEncodesIt(t *testing.T) {
	recipients := []string{"José <jösé@example.com>"}

	rawEmail := sendRecipients(t, recipients)

	want := []string{"To: =?UTF-8?q?Jos=C3=A9_<j=C3=B6s=C3=A9@example.com>?="}
	require.Equal(t, want, headerLines(string(rawEmail), "To:"))
	require.Equal(t, want, gomailToHeader(t, recipients))
}

func TestBuildRawEmailEncodesANonASCIIDisplayName(t *testing.T) {
	rawEmail := sendRecipients(t, []string{"José Pérez <jose@example.com>"})
	require.Equal(t, []string{"To: =?utf-8?q?Jos=C3=A9_P=C3=A9rez?= <jose@example.com>"}, headerLines(string(rawEmail), "To:"))
}

func TestBuildRawEmailWritesRecipientsThatParseBack(t *testing.T) {
	longName := "José María Pérez Hernández de la Concepción Martínez Gutiérrez"

	tests := []struct {
		name       string
		recipients []string
		want       []*mail.Address
	}{
		{
			name:       "non-ASCII display name",
			recipients: []string{"José Pérez <jose@example.com>"},
			want:       []*mail.Address{{Name: "José Pérez", Address: "jose@example.com"}},
		},
		{
			name:       "long non-ASCII display name",
			recipients: []string{longName + " <jose@example.com>"},
			want:       []*mail.Address{{Name: longName, Address: "jose@example.com"}},
		},
		{
			name:       "non-ASCII comment after an address",
			recipients: []string{"bob@example.com (José)"},
			want:       []*mail.Address{{Name: "José", Address: "bob@example.com"}},
		},
		{
			name:       "non-ASCII display name followed by a comment",
			recipients: []string{"José <bob@example.com> (c)"},
			want:       []*mail.Address{{Name: "José", Address: "bob@example.com"}},
		},
		{
			name:       "group of one with a non-ASCII display name",
			recipients: []string{"Friends: José <jose@example.com>;"},
			want:       []*mail.Address{{Name: "José", Address: "jose@example.com"}},
		},
		{
			name: "mixed recipients",
			recipients: []string{
				"plain@example.com",
				`"john doe"@example.com`,
				"José Pérez <jose@example.com>",
				`Bob <"john doe"@example.com>`,
				`"a\"b"@example.com`,
				longName + " <long@example.com>",
			},
			want: []*mail.Address{
				{Address: "plain@example.com"},
				{Address: "john doe@example.com"},
				{Name: "José Pérez", Address: "jose@example.com"},
				{Name: "Bob", Address: "john doe@example.com"},
				{Address: `a"b@example.com`},
				{Name: longName, Address: "long@example.com"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rawEmail := sendRecipients(t, tt.recipients)

			msg, err := mail.ReadMessage(bytes.NewReader(rawEmail))
			require.NoError(t, err)

			to, err := msg.Header.AddressList("To")
			require.NoError(t, err)
			require.Equal(t, tt.want, to)
		})
	}
}
