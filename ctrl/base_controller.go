package ctrl

import (
	"github.com/vanclief/compose/configurator"
	"github.com/vanclief/compose/logger"
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

func (c *BaseController) WithPromtailAndZerolog(params *logger.WithPromtailParams) error {
	const op = "BaseController.WithPromtailAndZerolog"

	err := logger.WithPromtailAndZerolog(params)
	if err != nil {
		return ez.Wrap(op, err)
	}

	return nil
}
