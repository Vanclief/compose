package s3

import (
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

func newTestClient() *Client {
	opts := []configurator.Option{}
	opts = append(opts, configurator.WithRequiredEnv("ENVIRONMENT"))
	opts = append(opts, configurator.WithRequiredEnv("S3_SECRET_KEY"))
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

	s3Client, err := NewClient(testConfig.S3.URL, testConfig.S3.Region, testConfig.S3.AccessKeyID, env.S3SecretKey, testConfig.S3.Bucket)
	if err != nil {
		panic(err)
	}

	return s3Client
}

func (suite *S3Suite) SetupTest() {
	client := newTestClient()

	suite.client = client
}

func TestSuiteRun(t *testing.T) {
	suite.Run(t, new(S3Suite))
}

func (suite *S3Suite) TestNewClient() {
	suite.NotNil(suite.client)
}
