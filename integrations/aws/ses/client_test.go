package ses

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

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

func TestGetServicesConcurrentWithSessionRefresh(t *testing.T) {
	// Static credentials make NewClient build its AWS config without any
	// network call, so this runs offline.
	client, err := NewClient(context.Background(), "us-east-1", "test-access-key", "test-secret-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}

				sesSvc, err := client.getSESService(ctx)
				if err != nil || sesSvc == nil {
					t.Errorf("getSESService: svc=%v err=%v", sesSvc, err)
					return
				}

				snsSvc, err := client.getSNSService(ctx)
				if err != nil || snsSvc == nil {
					t.Errorf("getSNSService: svc=%v err=%v", snsSvc, err)
					return
				}
			}
		}()
	}

	for i := 0; i < 100; i++ {
		client.sessionMutex.Lock()
		client.sessionExpiry = time.Now().Add(-time.Minute)
		client.sessionMutex.Unlock()

		err := client.ensureValidSession(ctx)
		if err != nil {
			t.Errorf("ensureValidSession: %v", err)
		}
	}

	close(stop)
	wg.Wait()
}
