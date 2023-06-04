package config

type optionApplyFunc func(cfg *config) error

type Option interface {
	applyOption(cfg *config) error
}

func (f optionApplyFunc) applyOption(c *config) error {
	return f(c)
}

func WithRequiredEnv(env string) Option {
	return optionApplyFunc(func(cfg *config) error {
		cfg.envars[env] = true
		return nil
	})
}

func WithOptionalEnv(env string) Option {
	return optionApplyFunc(func(cfg *config) error {
		cfg.envars[env] = false
		return nil
	})
}

func WithConfigPath(path string) Option {
	return optionApplyFunc(func(cfg *config) error {
		cfg.configPath = path
		return nil
	})
}

func WithEnvPath(path string) Option {
	return optionApplyFunc(func(cfg *config) error {
		cfg.envPath = path
		return nil
	})
}
