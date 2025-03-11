package ctrl

import (
	"os"

	"github.com/rs/zerolog/log"
	"github.com/vanclief/compose/configurator"
	"github.com/vanclief/compose/interfaces/aws/s3"
	"github.com/vanclief/compose/interfaces/aws/ses"
	"github.com/vanclief/compose/interfaces/promtail"
	"github.com/vanclief/ez"
)

type BaseController struct {
	Environment string
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

func (c *BaseController) WithPromtailAndZerolog(params *promtail.WithPromtailParams) error {
	const op = "BaseController.WithPromtailAndZerolog"

	err := promtail.WithZerolog(params)
	if err != nil {
		return ez.Wrap(op, err)
	}

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
