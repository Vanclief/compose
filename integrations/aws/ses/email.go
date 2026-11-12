package ses

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/mail"
	"path/filepath"
	"strings"

	"github.com/vanclief/ez"
	"gopkg.in/gomail.v2"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"
)

// SES limits each message to 50 recipients across To, Cc, and Bcc.
const maxEmailRecipients = 50

type Email struct {
	Recipients  []string
	Subject     string
	HTMLBody    string
	TextBody    string
	Attachments []Attachment
}

type Attachment struct {
	Filename    string
	ContentType string
	Data        []byte
}

// SendEmail delivers an email utilizing the AWS SES service
func (c *Client) SendEmail(ctx context.Context, email Email) (string, error) {
	err := validateEmail(email)
	if err != nil {
		return "", err
	}

	if c.outbox != nil {
		return c.recordEmail(ctx, email)
	}

	if len(email.Attachments) > 0 {
		return c.sendRawEmail(ctx, email)
	}

	return c.sendSimpleEmail(ctx, email)
}

func validateEmail(email Email) error {
	if len(email.Recipients) == 0 {
		return ez.New(ez.EINVALID, "recipients cannot be empty", nil)
	}

	if len(email.Recipients) > maxEmailRecipients {
		return ez.New(ez.EINVALID, "recipients cannot exceed 50", nil)
	}

	for i := range email.Recipients {
		if email.Recipients[i] == "" {
			return ez.New(ez.EINVALID, "recipient cannot be empty", nil)
		}
	}

	if email.HTMLBody == "" && email.TextBody == "" {
		return ez.New(ez.EINVALID, "email body cannot be empty", nil)
	}

	for i := range email.Attachments {
		err := validateAttachment(email.Attachments[i])
		if err != nil {
			return err
		}
	}

	return nil
}

// validateAttachment rejects an empty filename and a content type containing
// CR or LF, since the content type is written raw into the MIME part headers.
// An empty content type is allowed: buildRawEmail derives one from the
// filename extension. The filename may contain any character: buildRawEmail
// encodes it into the Content-Disposition header with mime.FormatMediaType.
func validateAttachment(attachment Attachment) error {
	if attachment.Filename == "" {
		return ez.New(ez.EINVALID, "attachment filename cannot be empty", nil)
	}

	if strings.ContainsAny(attachment.ContentType, "\r\n") {
		return ez.New(ez.EINVALID, "attachment content type contains invalid characters", nil)
	}

	return nil
}

func (c *Client) sendSimpleEmail(ctx context.Context, email Email) (string, error) {
	svc, err := c.getSESService(ctx)
	if err != nil {
		return "", ez.Wrap(err)
	}

	payload := &ses.SendEmailInput{
		Destination: &types.Destination{
			ToAddresses: email.Recipients,
		},
		Message: &types.Message{
			Body: buildEmailBody(email),
			Subject: &types.Content{
				Charset: aws.String(charSet),
				Data:    aws.String(email.Subject),
			},
		},
		Source: aws.String(c.getSenderAddress()),
	}

	res, err := svc.SendEmail(ctx, payload)
	if err != nil && isSessionError(err) {
		sendErr := err

		// Refresh session
		err = c.initSession(ctx)
		if err != nil {
			return "", ez.Wrap(sendErr)
		}

		// Try once more with refreshed session
		svc, err = c.getSESService(ctx)
		if err != nil {
			return "", ez.Wrap(err)
		}

		res, err = svc.SendEmail(ctx, payload)
	}

	if err != nil {
		return "", ez.Wrap(err)
	}

	return aws.ToString(res.MessageId), nil
}

func buildEmailBody(email Email) *types.Body {
	body := &types.Body{}

	if email.HTMLBody != "" {
		body.Html = &types.Content{
			Charset: aws.String(charSet),
			Data:    aws.String(email.HTMLBody),
		}
	}

	if email.TextBody != "" {
		body.Text = &types.Content{
			Charset: aws.String(charSet),
			Data:    aws.String(email.TextBody),
		}
	}

	return body
}

func (c *Client) sendRawEmail(ctx context.Context, email Email) (string, error) {
	svc, err := c.getSESService(ctx)
	if err != nil {
		return "", ez.Wrap(err)
	}

	rawEmail, err := c.buildRawEmail(email)
	if err != nil {
		return "", ez.Wrap(err)
	}

	input := &ses.SendRawEmailInput{
		RawMessage: &types.RawMessage{Data: rawEmail},
	}

	output, err := svc.SendRawEmail(ctx, input)
	if err != nil && isSessionError(err) {
		sendErr := err

		// Refresh session
		err = c.initSession(ctx)
		if err != nil {
			return "", ez.Wrap(sendErr)
		}

		// Try once more with refreshed session
		svc, err = c.getSESService(ctx)
		if err != nil {
			return "", ez.Wrap(err)
		}

		output, err = svc.SendRawEmail(ctx, input)
	}

	if err != nil {
		return "", ez.Wrap(err)
	}

	return aws.ToString(output.MessageId), nil
}

// buildRawEmail renders the email with its attachments as a MIME message
func (c *Client) buildRawEmail(email Email) ([]byte, error) {
	msg := gomail.NewMessage()

	// gomail writes printable ASCII and tabs unchanged, which is all
	// getSenderAddress returns whenever one of its forms parses back
	msg.SetHeader("From", c.getSenderAddress())
	msg.SetHeader("To", formatRecipients(email.Recipients)...)
	msg.SetHeader("Subject", email.Subject)
	setRawEmailBody(msg, email)

	for i := range email.Attachments {
		attachment := email.Attachments[i]

		// Setting Content-Disposition stops gomail from writing the raw
		// filename into the header. FormatMediaType quotes and escapes it, and
		// percent-encodes CR, LF and non-ASCII characters per RFC 2231.
		disposition := mime.FormatMediaType("attachment", map[string]string{"filename": attachment.Filename})
		if disposition == "" {
			return nil, ez.New(ez.EINVALID, "attachment filename cannot be encoded", nil)
		}

		msg.Attach(attachment.Filename,
			gomail.SetCopyFunc(func(w io.Writer) error {
				_, err := w.Write(attachment.Data)
				return err
			}),
			gomail.SetHeader(map[string][]string{
				"Content-Type":        {attachmentContentType(attachment)},
				"Content-Disposition": {disposition},
			}),
		)
	}

	var rawEmail bytes.Buffer
	_, err := msg.WriteTo(&rawEmail)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return rawEmail.Bytes(), nil
}

// attachmentContentType returns the attachment's content type, falling back to
// the type registered for its filename extension and then to
// application/octet-stream. buildRawEmail always sets the Content-Type header,
// so gomail never applies its own extension default.
func attachmentContentType(attachment Attachment) string {
	if attachment.ContentType != "" {
		return attachment.ContentType
	}

	contentType := mime.TypeByExtension(filepath.Ext(attachment.Filename))
	if contentType != "" {
		return contentType
	}

	return "application/octet-stream"
}

func setRawEmailBody(msg *gomail.Message, email Email) {
	switch {
	case email.TextBody != "" && email.HTMLBody != "":
		msg.SetBody("text/plain", email.TextBody)
		msg.AddAlternative("text/html", email.HTMLBody)
	case email.TextBody != "":
		msg.SetBody("text/plain", email.TextBody)
	case email.HTMLBody != "":
		msg.SetBody("text/html", email.HTMLBody)
	}
}

// formatRecipients returns the recipients as To header values in a new slice,
// since gomail encodes the values it is given in place. gomail's header
// encoder writes a value made only of printable ASCII and tabs unchanged and
// RFC 2047 encodes any other value whole. A recipient made only of those bytes
// is passed exactly as the caller wrote it. Any other recipient that
// mail.ParseAddress reads as one address with a display name and an address
// made only of those bytes is written as that name and address by
// mail.Address.String, which quotes or RFC 2047 encodes the name as needed and
// so gives a value gomail writes unchanged. mail.ParseAddress also reads the
// name from a comment, as in "bob@example.com (José)", and from a group of one,
// as in "Friends: José <jose@example.com>;", so such a recipient is rewritten
// too, and a comment or a group label is not kept. Every other recipient is
// passed as the caller wrote it, and gomail encodes it whole.
func formatRecipients(recipients []string) []string {
	formatted := make([]string, len(recipients))
	for i := range recipients {
		formatted[i] = recipients[i]
		if isUnencodedHeaderText(recipients[i]) {
			continue
		}

		address, err := mail.ParseAddress(recipients[i])
		if err != nil || address.Name == "" || !isUnencodedHeaderText(address.Address) {
			continue
		}

		formatted[i] = address.String()
	}

	return formatted
}

// isUnencodedHeaderText reports whether s is made only of printable ASCII and
// tabs, the bytes gomail's header encoder leaves unchanged.
func isUnencodedHeaderText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '\t' && (s[i] < ' ' || s[i] > '~') {
			return false
		}
	}

	return true
}

// getSenderAddress returns the From address: the sender address alone when
// there is no sender name, and otherwise the first of three forms that is made
// only of bytes gomail's header encoder leaves unchanged and that
// mail.ParseAddress parses to the sender's address. The first form is the name
// and the address as the caller wrote them, which is taken whatever name it
// parses to, so a name already encoded as an RFC 2047 word is decoded by mail
// clients. The second is the name quoted or RFC 2047 encoded by
// mail.Address.String and the third the name B-encoded by mime.BEncoding, and
// each is taken only when it parses back to exactly the sender name. When no
// form qualifies, the second is returned.
func (c *Client) getSenderAddress() string {
	if c.SenderName == "" {
		return c.EmailSender
	}

	// mail.Address.String quotes a local part that needs quoting, so the
	// written forms are given the parsed address, whose quotes are removed
	address := c.EmailSender
	parsed, err := mail.ParseAddress(c.EmailSender)
	if err == nil {
		address = parsed.Address
	}

	asWritten := c.SenderName + " <" + c.EmailSender + ">"
	parsed, ok := parseHeaderAddress(asWritten)
	if ok && parsed.Address == address {
		return asWritten
	}

	quoted := (&mail.Address{Name: c.SenderName, Address: address}).String()
	encoded := mime.BEncoding.Encode("utf-8", c.SenderName) + " " + (&mail.Address{Address: address}).String()
	for _, candidate := range []string{quoted, encoded} {
		parsed, ok = parseHeaderAddress(candidate)
		if ok && parsed.Name == c.SenderName && parsed.Address == address {
			return candidate
		}
	}

	return quoted
}

// parseHeaderAddress parses s with mail.ParseAddress when s is made only of
// bytes gomail's header encoder leaves unchanged.
func parseHeaderAddress(s string) (*mail.Address, bool) {
	if !isUnencodedHeaderText(s) {
		return nil, false
	}

	parsed, err := mail.ParseAddress(s)
	if err != nil {
		return nil, false
	}

	return parsed, true
}
