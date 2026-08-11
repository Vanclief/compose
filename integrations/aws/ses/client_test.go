package ses

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/suite"
	"github.com/vanclief/compose/components/configurator"
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

func newTestClient(sesOpts ...ClientOption) (*Client, *EnvVars, error) {
	opts := []configurator.Option{}
	opts = append(opts, configurator.WithRequiredEnv("ENVIRONMENT"))
	opts = append(opts, configurator.WithRequiredEnv("AWS_SECRET_KEY"))
	opts = append(opts, configurator.WithRequiredEnv("TEST_EMAIL"))
	opts = append(opts, configurator.WithRequiredEnv("TEST_PHONE_NUMBER"))
	opts = append(opts, configurator.WithConfigPath("../../../config/application/"))
	opts = append(opts, configurator.WithEnvPath("../../../.env"))

	cfg, err := configurator.New(opts...)
	if err != nil {
		return nil, nil, err
	}

	env := &EnvVars{}
	err = cfg.LoadEnvVars(env)
	if err != nil {
		return nil, nil, err
	}

	testConfig := &TestConfig{}
	err = cfg.LoadConfiguration(testConfig)
	if err != nil {
		return nil, nil, err
	}

	sesClient, err := NewClient(context.Background(), testConfig.SES.Region, testConfig.SES.AccessKeyID, env.AWSSecretKey, sesOpts...)
	if err != nil {
		return nil, nil, err
	}

	return sesClient, env, nil
}

func (suite *TestSuite) SetupTest() {
	// Without AWS credentials the suite skips so `go test ./...` stays
	// runnable on any machine. Setting COMPOSE_TEST_AWS turns missing
	// credentials into a hard failure so regressions cannot hide behind skips.
	client, env, err := newTestClient()
	if err != nil {
		if os.Getenv("COMPOSE_TEST_AWS") != "" {
			suite.T().Fatalf("AWS testing is enabled but the SES client could not be created: %v", err)
		}

		suite.T().Skipf("AWS credentials are not available: %v", err)
	}

	suite.client = client
	suite.client.EmailSender = env.TestEmail
	suite.testEmail = env.TestEmail
	suite.testPhoneNumber = env.TestPhoneNumber
}

func TestSuiteRun(t *testing.T) {
	suite.Run(t, new(TestSuite))
}
