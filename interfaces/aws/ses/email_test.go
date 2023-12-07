package ses

import (
	"github.com/stretchr/testify/assert"
)

type email struct {
	sender    string
	recipient string
	subject   string
	htmlBody  string
	charSet   string
	region    string
}

func (suite *TestSuite) TestSendEmail() {
	e := email{
		recipient: suite.testEmail,
		subject:   "Compose SES Test Email",
		htmlBody:  `<h1>Hello World</h1>`,
	}

	msg, err := suite.client.SendEmail(e.recipient, e.subject, e.htmlBody)
	suite.NotNil(msg)
	suite.Nil(err)
	assert.NoError(suite.T(), err, "error sending email")
}
