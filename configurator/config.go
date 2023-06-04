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
	envars     map[string]bool
	configPath string
	envPath    string
}

func New(opts ...Option) (*Configurator, error) {
	const op = "Configurator.New"

	c := &Configurator{
		envars: make(map[string]bool),
	}

	for _, opt := range opts {
		if err := opt.applyOption(c); err != nil {
			return nil, ez.Wrap(op, err)
		}
	}

	return c, nil
}

// LoadEnv loads the environment variables into the struct
func (cfg *Configurator) LoadEnv(output any) error {
	const op = "Configurator.LoadEnv"

	envMap := make(map[string]interface{})
	viper.AutomaticEnv()

	for envar, required := range cfg.envars {
		value := viper.GetString(envar)

		if value == "" && required {
			errMsg := fmt.Sprintf("Required env var %s is not set", envar)
			return ez.New(op, ez.EINVALID, errMsg, nil)
		} else if value != "" {
			key := strings.ReplaceAll(envar, "_", "")
			envMap[key] = value
		}
	}

	err := mapstructure.Decode(envMap, output)
	if err != nil {
		return ez.Wrap(op, err)
	}

	return nil
}

// LoadEnvFromFile loads the environment variables into the struct
func (cfg *Configurator) LoadEnvFromFile(output any) error {
	const op = "Configurator.LoadEnvFromFile"

	err := godotenv.Load(cfg.envPath)
	if err != nil {
		return ez.Wrap(op, err)
	}

	envMap := make(map[string]interface{})

	for envar, required := range cfg.envars {
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

func (cfg *Configurator) LoadSettings(environment string, settings any) error {
	const op = "Configurator.LoadSettings"

	if environment == "" {
		return ez.New(op, ez.EINVALID, "Environment is not set", nil)
	}

	environment = strings.ToLower(environment)

	viper.SetConfigType("json")
	viper.AddConfigPath(".")
	// viper.AddConfigPath(env.ProjectRootPath)

	configPath := fmt.Sprintf("%s%s.config", cfg.configPath, environment)
	viper.SetConfigName(configPath)

	err := viper.ReadInConfig()
	if err != nil {
		errMsg := fmt.Sprintf("Config file %s.json not found", configPath)
		return ez.New(op, ez.EINVALID, errMsg, err)
	}

	err = viper.Unmarshal(&settings)
	if err != nil {
		return ez.New(op, ez.EINTERNAL, "Unable to unmarshal settings", err)
	}

	return nil
}
