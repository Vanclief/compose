package configurator

type optionApplyFunc func(cfg *Configurator) error

type Option interface {
	applyOption(cfg *Configurator) error
}

func (f optionApplyFunc) applyOption(c *Configurator) error {
	return f(c)
}

func WithRequiredEnv(env string) Option {
	return optionApplyFunc(func(cfg *Configurator) error {
		cfg.envars[env] = true
		return nil
	})
}

func WithOptionalEnv(env string) Option {
	return optionApplyFunc(func(cfg *Configurator) error {
		cfg.envars[env] = false
		return nil
	})
}

func WithConfigPath(path string) Option {
	return optionApplyFunc(func(cfg *Configurator) error {
		cfg.configPath = path
		return nil
	})
}

func WithEnvPath(path string) Option {
	return optionApplyFunc(func(cfg *Configurator) error {
		cfg.envPath = path
		return nil
	})
}
