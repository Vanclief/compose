package ctrl

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/components/configurator"
	"github.com/vanclief/compose/integrations/aws/s3"
	"github.com/vanclief/compose/integrations/aws/ses"
	"github.com/vanclief/compose/integrations/promtail"
	"github.com/vanclief/ez"
)

// fatalDrainTimeout bounds how long a Fatal exit waits for Loki to receive the
// buffered log lines before the process exits.
const fatalDrainTimeout = 5 * time.Second

type BaseController struct {
	Environment string
	logWriter   io.Writer
	promtail    *promtail.Client
}

func (c *BaseController) LoadEnvVarsAndConfig(envVarsOutput, configOutput any, configOpts ...configurator.Option) error {
	cfg, err := configurator.New(configOpts...)
	if err != nil {
		return ez.Wrap(err)
	}

	c.Environment = cfg.Environment

	err = cfg.LoadEnvVarsAndConfig(envVarsOutput, configOutput)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

func (c *BaseController) WithZerolog() {
	writer := c.logWriter
	if writer == nil {
		writer = os.Stdout
	}

	output := zerolog.ConsoleWriter{Out: writer}
	output.FormatMessage = func(i interface{}) string {
		if msg, ok := i.(string); ok {
			return fmt.Sprintf("%-50s", msg)
		}
		return ""
	}

	log.Logger = log.Output(output)
}

func (c *BaseController) WithPromtail(params *promtail.WithPromtailParams) error {
	err := params.Validate()
	if err != nil {
		return ez.Wrap(err)
	}

	c.logWriter = os.Stdout
	if params.PromtailEnabled {
		client, err := promtail.NewClient(params)
		if err != nil {
			return ez.Wrap(err)
		}

		c.promtail = client
		c.logWriter = &promtailWriter{stdout: os.Stdout, client: client}
	}

	log.Logger = log.Output(c.logWriter)
	log.Info().
		Str("App", params.App).
		Str("Environment", params.Environment).
		Str("Host", params.PromtailHost).
		Str("Username", params.PromtailUsername).
		Int("Timeout MS", params.EffectiveTimeoutMS()).
		Bool("Enabled", params.PromtailEnabled).
		Msg("Promtail Config")

	return nil
}

// CloseLogs pushes the logs still buffered for Promtail. Call it before the
// process exits, after the last log line worth keeping.
func (c *BaseController) CloseLogs(ctx context.Context) error {
	if c.promtail == nil {
		return nil
	}

	err := c.promtail.Close(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// promtailWriter writes every line to stdout and to Promtail. Unlike
// io.MultiWriter it is an io.Closer, which zerolog's Fatal relies on: it closes
// the logger's writer before os.Exit, and the zerolog.ConsoleWriter set by
// WithZerolog forwards that Close to its Out. Without it the lines still
// buffered in the client would be lost on log.Fatal or rest.Start's
// e.Logger.Fatal.
type promtailWriter struct {
	stdout io.Writer
	client *promtail.Client
}

var _ io.Closer = (*promtailWriter)(nil)

// Write writes p to stdout and then to the client, returning the first error
// like io.MultiWriter. The client queues p and never fails.
func (w *promtailWriter) Write(p []byte) (int, error) {
	n, err := w.stdout.Write(p)
	if err != nil {
		return n, ez.Wrap(err)
	}

	return w.client.Write(p)
}

// Close pushes the lines still buffered in the client, waiting at most
// fatalDrainTimeout. Client.Close is idempotent, so it is safe for both
// zerolog's Fatal hook and CloseLogs to close the same client.
func (w *promtailWriter) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), fatalDrainTimeout)
	defer cancel()

	err := w.client.Close(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

func (c *BaseController) WithSES(ctx context.Context, cfg *ses.Config, AWSSecretKey string) (*ses.Client, error) {
	log.Info().
		Str("Host", cfg.Region).
		Str("AccessKey", cfg.AccessKeyID).
		Str("Email Sender", cfg.EmailSender).
		Str("PushNotificationARN", cfg.PushNotificationARN).
		Msg("Creating SES Client")

	sesClient, err := ses.NewClient(
		ctx,
		cfg.Region,
		cfg.AccessKeyID,
		AWSSecretKey,
		ses.WithEmailSender(cfg.EmailSender),
		ses.WithSenderName(cfg.SenderName),
		ses.WithPushNotificationARN(cfg.PushNotificationARN),
	)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to setup AWS Client")
		os.Exit(1)
	}

	return sesClient, nil
}

func (c *BaseController) WithS3(ctx context.Context, cfg *s3.Config, S3SecretKey string, opts ...s3.ClientOption) (*s3.Client, error) {
	log.Info().
		Str("Host", cfg.Region).
		Str("Bucket", cfg.Bucket).
		Str("AccessKey", cfg.AccessKeyID).
		Msg("Creating S3 Client")

	s3Client, err := s3.NewClient(ctx, cfg.Region, cfg.AccessKeyID, S3SecretKey, cfg.Bucket, opts...)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to setup S3 Client")
		os.Exit(1)
	}

	return s3Client, nil
}
