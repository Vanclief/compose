package postgres

type Option func(db *DB) error

func WithExtensions(extensions []string) Option {
	return func(db *DB) error {
		return db.CreateExtensions(extensions)
	}
}
