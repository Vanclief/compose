package s3

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/suite"
	"github.com/vanclief/compose/components/configurator"
)

type S3Suite struct {
	suite.Suite
	client *Client
}

type EnvVars struct {
	Environment string
	S3SecretKey string ` mapstucture:"S3_SECRET_KEY"`
}

type TestConfig struct {
	S3 Config ` mapstucture:"s3"`
}

func newTestClient() (*Client, error) {
	opts := []configurator.Option{}
	opts = append(opts, configurator.WithRequiredEnv("ENVIRONMENT"))
	opts = append(opts, configurator.WithRequiredEnv("S3_SECRET_KEY"))
	opts = append(opts, configurator.WithConfigPath("../../../config/application/"))
	opts = append(opts, configurator.WithEnvPath("../../../.env"))

	cfg, err := configurator.New(opts...)
	if err != nil {
		return nil, err
	}

	env := &EnvVars{}
	err = cfg.LoadEnvVars(env)
	if err != nil {
		return nil, err
	}

	testConfig := &TestConfig{}
	err = cfg.LoadConfiguration(testConfig)
	if err != nil {
		return nil, err
	}

	return NewClient(
		context.Background(),
		testConfig.S3.Region,
		testConfig.S3.AccessKeyID,
		env.S3SecretKey,
		testConfig.S3.Bucket,
		WithDigitalOceanEndpoint(testConfig.S3.Region, testConfig.S3.URL),
		WithDigitalOceanCDN(testConfig.S3.Bucket, testConfig.S3.Region, testConfig.S3.URL),
	)
}

func (suite *S3Suite) SetupTest() {
	// Without AWS credentials the suite skips so `go test ./...` stays
	// runnable on any machine. Setting COMPOSE_TEST_AWS turns missing
	// credentials into a hard failure so regressions cannot hide behind skips.
	client, err := newTestClient()
	if err != nil {
		if os.Getenv("COMPOSE_TEST_AWS") != "" {
			suite.T().Fatalf("AWS testing is enabled but the S3 client could not be created: %v", err)
		}

		suite.T().Skipf("AWS credentials are not available: %v", err)
	}

	suite.client = client
}

func TestSuiteRun(t *testing.T) {
	suite.Run(t, new(S3Suite))
}

func (suite *S3Suite) TestNewClient() {
	suite.NotNil(suite.client)
}
