package ctrl

import (
	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun/extra/bundebug"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/compose/drivers/databases/relational/sqlite"
	"github.com/vanclief/ez"
)

// WithSQLite - Opens a SQLite database and creates the schema from the
// model structs if it doesn't already exist.
func (c *BaseController) WithSQLite(cfg *sqlite.ConnectionConfig, models []interface{}, options ...relational.Option) (*relational.DB, error) {
	db, err := sqlite.ConnectToDatabase(cfg)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	if cfg.Verbose {
		queryHook := bundebug.NewQueryHook(bundebug.WithVerbose(cfg.Verbose))
		db.AddQueryHook(queryHook)
		log.Info().Bool("Verbose", cfg.Verbose).Msg("Displaying database query logs")
	}

	for _, option := range options {
		err = option(db)
		if err != nil {
			db.Close() // nolint:errcheck // The option error is the one that matters
			return nil, ez.Wrap(err)
		}
	}

	err = db.CreateTables(models)
	if err != nil {
		db.Close() // nolint:errcheck // The initialization error is the one that matters
		return nil, ez.Wrap(err)
	}

	return db, nil
}
