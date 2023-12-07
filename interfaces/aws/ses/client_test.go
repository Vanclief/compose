package ses

import (
	"testing"

	"github.com/stretchr/testify/suite"
	"github.com/vanclief/compose/configurator"
)

type TestSuite struct {
	suite.Suite
	client          *Client
	testEmail       string
	testPhoneNumber string
}

type EnvVars struct {
	Environment     string
	TestEmail       string
	TestPhoneNumber string
	AWSSecretKey    string ` mapstucture:"AWS_SECRET_KEY"`
}

type TestConfig struct {
	SES Config ` mapstucture:"ses"`
}

func newTestClient() (*Client, *EnvVars) {
	opts := []configurator.Option{}
	opts = append(opts, configurator.WithRequiredEnv("ENVIRONMENT"))
	opts = append(opts, configurator.WithRequiredEnv("AWS_SECRET_KEY"))
	opts = append(opts, configurator.WithRequiredEnv("TEST_EMAIL"))
	opts = append(opts, configurator.WithRequiredEnv("TEST_PHONE_NUMBER"))
	opts = append(opts, configurator.WithConfigPath("../../../config/application/"))
	opts = append(opts, configurator.WithEnvPath("../../../.env"))

	cfg, err := configurator.New(opts...)
	if err != nil {
		panic(err)
	}

	env := &EnvVars{}
	err = cfg.LoadEnvVars(env)
	if err != nil {
		panic(err)
	}

	testConfig := &TestConfig{}
	err = cfg.LoadConfiguration(testConfig)
	if err != nil {
		panic(err)
	}

	sesClient, err := NewClient(testConfig.SES.Region, testConfig.SES.AccessKeyID, env.AWSSecretKey)
	if err != nil {
		panic(err)
	}

	return sesClient, env
}

func (suite *TestSuite) SetupTest() {
	var env *EnvVars

	suite.client, env = newTestClient()
	suite.testEmail = env.TestEmail
	suite.testPhoneNumber = env.TestPhoneNumber

	suite.client.SetEmailSender(env.TestEmail)
}

func TestSuiteRun(t *testing.T) {
	suite.Run(t, new(TestSuite))
}
