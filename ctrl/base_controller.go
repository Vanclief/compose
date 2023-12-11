package ctrl

import (
	"os"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun/extra/bundebug"
	"github.com/vanclief/compose/configurator"
	"github.com/vanclief/compose/interfaces/aws/s3"
	"github.com/vanclief/compose/interfaces/aws/ses"
	"github.com/vanclief/compose/interfaces/databases/postgres"
	"github.com/vanclief/compose/interfaces/logging"
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

func (c *BaseController) WithPromtailAndZerolog(params *logging.WithPromtailParams) error {
	const op = "BaseController.WithPromtailAndZerolog"

	err := logging.WithPromtailAndZerolog(params)
	if err != nil {
		return ez.Wrap(op, err)
	}

	return nil
}

func (c *BaseController) WithPostgres(cfg *postgres.ConnectionConfig, models []interface{}) (*postgres.DB, error) {
	const op = "BaseController.WithPostgres"

	db, err := postgres.ConnectToDatabase(cfg)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	if cfg.Verbose {
		queryHook := bundebug.NewQueryHook(bundebug.WithVerbose(cfg.Verbose))
		db.AddQueryHook(queryHook)
	}

	err = db.CreateTables(models)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return db, nil
}

func (c *BaseController) WithSES(cfg *ses.Config, AWSSecretKey string) (*ses.Client, error) {
	log.Info().
		Str("Host", cfg.Region).
		Str("AccessKey", cfg.AccessKeyID).
		Str("Email Sender", cfg.EmailSender).
		Str("PushNotificationARN", cfg.PushNotificationARN).
		Msg("Creating SES Client")

	sesClient, err := ses.NewClient(cfg.Region, cfg.AccessKeyID, AWSSecretKey)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to setup AWS Client")
		os.Exit(1)
	}

	sesClient.SetEmailSender(cfg.EmailSender)
	sesClient.SetPushNotificationARN(cfg.PushNotificationARN)

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
