package relational

type Option func(db *DB) error

func WithRegistrableModels(models []interface{}) Option {
	return func(db *DB) error {
		return db.RegisterModels(models)
	}
}
