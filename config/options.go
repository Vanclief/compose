package config

type optionApplyFunc func(cfg *config) error

type Option interface {
	applyOption(cfg *config) error
}

func (f optionApplyFunc) applyOption(c *config) error {
	return f(c)
}

func WithModuleName(name string) Option {
	return optionApplyFunc(func(cfg *config) error {
		cfg.moduleName = name
		return nil
	})
}

func WithRequiredEnv(env string) Option {
	return optionApplyFunc(func(cfg *config) error {
		cfg.envs[env] = true
		return nil
	})
}

func WithOptionalEnv(env string) Option {
	return optionApplyFunc(func(cfg *config) error {
		cfg.envs[env] = false
		return nil
	})
}
