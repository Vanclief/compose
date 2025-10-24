package ctrl

import (
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/components/configurator"
	"github.com/vanclief/compose/integrations/aws/s3"
	"github.com/vanclief/compose/integrations/aws/ses"
	"github.com/vanclief/compose/integrations/promtail"
	"github.com/vanclief/ez"
)

type BaseController struct {
	Environment string
	logWriter   io.Writer
}

func (c *BaseController) LoadEnvVarsAndConfig(envVarsOutput, configOutput any, configOpts ...configurator.Option) error {
	const op = "BaseController.LoadEnvVarsAndConfig"

	cfg, err := configurator.New(configOpts...)
	if err != nil {
		return ez.Wrap(op, err)
	}

	c.Environment = cfg.Environment

	err = cfg.LoadEnvVarsAndConfig(envVarsOutput, configOutput)
	if err != nil {
		return ez.Wrap(op, err)
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
	const op = "BaseController.WithPromtail"

	writer, err := promtail.NewWriter(params)
	if err != nil {
		return ez.Wrap(op, err)
	}

	c.logWriter = writer
	log.Logger = log.Output(writer)
	log.Info().
		Str("App", params.App).
		Str("Environment", params.Environment).
		Str("Host", params.PromtailHost).
		Str("Username", params.PromtailUsername).
		Int("Timeout MS", params.PromtailTimeoutMS).
		Bool("Enabled", params.PromtailEnabled).
		Msg("Promtail Config")

	return nil
}

func (c *BaseController) WithSES(cfg *ses.Config, AWSSecretKey string) (*ses.Client, error) {
	log.Info().
		Str("Host", cfg.Region).
		Str("AccessKey", cfg.AccessKeyID).
		Str("Email Sender", cfg.EmailSender).
		Str("PushNotificationARN", cfg.PushNotificationARN).
		Msg("Creating SES Client")

	sesClient, err := ses.NewClient(
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

func (c *BaseController) WithS3(cfg *s3.Config, S3SecretKey string) (*s3.Client, error) {
	log.Info().
		Str("Host", cfg.Region).
		Str("Bucket", cfg.Bucket).
		Str("AccessKey", cfg.AccessKeyID).
		Msg("Creating S3 Client")

	s3Client, err := s3.NewClient(cfg.URL, cfg.Region, cfg.AccessKeyID, S3SecretKey, cfg.Bucket)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to setup S3 Client")
		os.Exit(1)
	}

	return s3Client, nil
}
