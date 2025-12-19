package ses

import (
	"context"

	"github.com/stretchr/testify/assert"
)

type sms struct {
	message     string
	phoneNumber string
}

func (suite *TestSuite) TestSendSMS() {
	m := sms{
		message:     "Automated test message from Compose",
		phoneNumber: suite.testPhoneNumber,
	}

	msg, err := suite.client.SendSMS(context.Background(), m.phoneNumber, m.message)
	suite.NotNil(msg)
	suite.Nil(err)
	assert.NoError(suite.T(), err, "error sending SMS")
}
