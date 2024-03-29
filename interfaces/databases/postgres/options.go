package postgres

type Option func(db *DB) error

func WithExtensions(extensions []string) Option {
	return func(db *DB) error {
		return db.CreateExtensions(extensions)
	}
}

func WithRegistrableModels(models []interface{}) Option {
	return func(db *DB) error {
		return db.RegisterModels(models)
	}
}
