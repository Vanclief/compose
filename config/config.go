package config

import (
	"fmt"
	"strings"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"github.com/vanclief/ez"
)

type config struct {
	moduleName string
	envs       map[string]bool
	path       string
}

func NewConfig(opts ...Option) (*config, error) {
	const op = "config.NewConfig"

	c := &config{
		envs: make(map[string]bool),
	}

	for _, opt := range opts {
		if err := opt.applyOption(c); err != nil {
			return nil, ez.Wrap(op, err)
		}
	}

	return c, nil
}

// LoadEnvVariables loads the environment variables into the struct
func (cfg *config) LoadEnvVariables(env any) error {
	const op = "config.LoadEnvVariables"

	vars := make(map[string]interface{})
	viper.AutomaticEnv()

	for envVar, required := range cfg.envs {
		value := viper.GetString(envVar)

		if value == "" && required {
			errMsg := fmt.Sprintf("Required env var %s is not set", envVar)
			return ez.New(op, ez.EINVALID, errMsg, nil)
		} else if value != "" {
			key := strings.ReplaceAll(envVar, "_", "")
			vars[key] = value
		}
	}

	mapstructure.Decode(vars, env)

	return nil
}

func (cfg *config) LoadSettings(environment string, settings any) error {
	const op = "config.LoadSettings"

	if environment == "" {
		return ez.New(op, ez.EINVALID, "Environment is not set", nil)
	}

	environment = strings.ToLower(environment)

	viper.SetConfigType("json")
	viper.AddConfigPath(".")
	// viper.AddConfigPath(env.ProjectRootPath)

	configPath := fmt.Sprintf("%s%s.config", cfg.path, environment)
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

// GetTestingEnvVariables returns env variables for testing
// func GetTestingEnvVariables() *Env {
// 	wd, err := os.Getwd()
// 	if err != nil {
// 		panic(err)
// 	}

// 	// Since we are loading configuration files from the root dir, when running from main package
// 	// this is fine but for testing we need to find the root dir
// 	dir := filepath.Dir(wd)

// 	for dir[len(dir)-len(moduleName):] != moduleName {
// 		dir = filepath.Dir(dir)
// 	}

// 	return &Env{ProjectRootPath: dir}
// }
