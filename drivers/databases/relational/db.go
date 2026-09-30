package relational

import (
	"context"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/vanclief/ez"
)

type DB struct {
	*bun.DB
}

// CreateTables - Creates the database schema if it doesn't already exist
func (db *DB) CreateTables(models []interface{}) error {
	ctx := context.Background()

	for _, model := range models {
		_, err := db.NewCreateTable().
			Model(model).
			IfNotExists().
			Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}

// RegisterModels - Registers many-to-many relationship
func (db *DB) RegisterModels(models []interface{}) error {
	for _, model := range models {
		db.RegisterModel(model)
	}

	return nil
}

// ResetTables - Drops and recreates the database schema
func (db *DB) ResetTables(models []interface{}) error {
	ctx := context.Background()

	for _, model := range models {
		err := db.ResetModel(ctx, model)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}

// ResetSchema - Drops the declared tables and the ledger, then creates the
// declared schema as Migrate does on an empty database. Meant for tests
func (db *DB) ResetSchema(ctx context.Context, schema Schema) error {
	tables, err := validateSchema(db.DB, schema)
	if err != nil {
		return ez.Wrap(err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return ez.Wrap(err)
	}
	defer conn.Close() // nolint:errcheck // Closing only returns the connection to the pool

	err = lockSchema(ctx, conn)
	if err != nil {
		return ez.Wrap(err)
	}
	defer unlockSchema(ctx, conn)

	// Reverse model order drops referencing tables before the ones they
	// reference, which SQLite needs since it has no CASCADE
	for i := len(tables) - 1; i >= 0; i-- {
		_, err = conn.NewDropTable().Model(tables[i].Model).IfExists().Cascade().Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	_, err = conn.NewDropTable().Model((*schemaStep)(nil)).IfExists().Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	err = createExtensions(ctx, conn, schema.Extensions)
	if err != nil {
		return ez.Wrap(err)
	}

	err = createFresh(ctx, conn, schema, tables)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// CreateExtensions - Creates a database extension if it doesn't already
// exist. Extensions are a Postgres concept, so requesting any on another
// dialect is an error rather than a silent no-op.
func (db *DB) CreateExtensions(extensions []string) error {
	return createExtensions(context.Background(), db.DB, extensions)
}

// createExtensions - CreateExtensions on a given connection, so Migrate can
// run it on the connection it holds its lock on
func createExtensions(ctx context.Context, db bun.IDB, extensions []string) error {
	if len(extensions) == 0 {
		return nil
	}

	if db.Dialect().Name() != dialect.PG {
		return ez.New(ez.ENOTIMPLEMENTED, "Database extensions are only supported by PostgreSQL", nil)
	}

	for _, extension := range extensions {
		_, err := db.NewRaw("CREATE EXTENSION IF NOT EXISTS ?", bun.Ident(extension)).Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	return nil
}
