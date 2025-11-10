package configurator

import (
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"github.com/vanclief/ez"
)

type Configurator struct {
	envVars     map[string]bool
	envPath     string
	Environment string
	configPath  string
}

// New returns a new Configurator instance
func New(opts ...Option) (*Configurator, error) {
	const op = "Configurator.New"

	// Check that the environment variable is set
	viper.BindEnv("ENVIRONMENT")
	environment := viper.GetString("ENVIRONMENT")
	if environment == "" {
		return nil, ez.New(op, ez.EINVALID, "Required ENVIRONMENT variable not set", nil)
	}

	c := &Configurator{
		envVars:     make(map[string]bool),
		Environment: environment,
	}

	for _, opt := range opts {
		if err := opt.applyOption(c); err != nil {
			return nil, ez.Wrap(op, err)
		}
	}

	return c, nil
}

func (cfg *Configurator) LoadEnvVarsAndConfig(envVarsOutput, configOutput any) error {
	const op = "Configurator.LoadEnvVarsAndConfig"

	err := cfg.LoadEnvVars(envVarsOutput)
	if err != nil {
		return ez.Wrap(op, err)
	}

	err = cfg.LoadConfiguration(configOutput)
	if err != nil {
		return ez.Wrap(op, err)
	}

	return nil
}

// LoadEnvVars loads the environment variables into an interface
func (cfg *Configurator) LoadEnvVars(output any) error {
	const op = "Configurator.LoadEnvVars"

	envMap := make(map[string]interface{})
	viper.AutomaticEnv()

	for envar, required := range cfg.envVars {
		value := viper.GetString(envar)

		if value == "" && required {
			errMsg := fmt.Sprintf("Required env var %s is not set", envar)
			return ez.New(op, ez.EINVALID, errMsg, nil)
		} else if value != "" {
			key := strings.ReplaceAll(envar, "_", "")
			envMap[key] = value
		}
	}

	envMap["ENVIRONMENT"] = cfg.Environment

	err := mapstructure.Decode(envMap, output)
	if err != nil {
		return ez.Wrap(op, err)
	}

	return nil
}

// LoadEnvVarsFromFile loads the environment variables from a file into an interface
func (cfg *Configurator) LoadEnvVarsFromFile(output any) error {
	const op = "Configurator.LoadEnvFromFile"

	err := godotenv.Load(cfg.envPath)
	if err != nil {
		return ez.Wrap(op, err)
	}

	envMap := make(map[string]interface{})

	for envar, required := range cfg.envVars {
		value := os.Getenv(envar)

		if value == "" && required {
			errMsg := fmt.Sprintf("Required env var %s is not set", envar)
			return ez.New(op, ez.EINVALID, errMsg, nil)
		} else if value != "" {
			key := strings.ReplaceAll(envar, "_", "")
			envMap[key] = value
		}
	}

	err = mapstructure.Decode(envMap, output)
	if err != nil {
		return ez.Wrap(op, err)
	}

	return nil
}

// LoadConfiguration loads configuration files into an interface based on the environment
func (cfg *Configurator) LoadConfiguration(output any) error {
	const op = "Configurator.LoadConfiguration"

	environment := strings.ToLower(cfg.Environment)

	viper.SetConfigType("json")
	viper.AddConfigPath(".")

	configPath := fmt.Sprintf("%s%s.config", cfg.configPath, environment)
	viper.SetConfigName(configPath)

	err := viper.ReadInConfig()
	if err != nil {
		errMsg := fmt.Sprintf("Config file with path %s.json not found", configPath)
		return ez.New(op, ez.ENOTFOUND, errMsg, err)
	}

	err = viper.Unmarshal(&output)
	if err != nil {
		return ez.New(op, ez.EINTERNAL, "Unable to unmarshal settings", err)
	}

	return nil
}
