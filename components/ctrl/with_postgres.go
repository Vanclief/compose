package ctrl

import (
	"context"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun/extra/bundebug"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/compose/drivers/databases/relational/postgres"
	"github.com/vanclief/ez"
)

// WithPostgres - Connects to a Postgres database, runs the options, then
// migrates it to the schema (see relational.Schema). Options must not
// change the schema
func (c *BaseController) WithPostgres(cfg *postgres.ConnectionConfig, schema relational.Schema, options ...relational.Option) (*relational.DB, error) {
	db, err := postgres.ConnectToDatabase(cfg)
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

	plan, err := db.Migrate(context.Background(), schema)
	if err != nil {
		db.Close() // nolint:errcheck // The initialization error is the one that matters
		return nil, ez.Wrap(err)
	}

	log.Info().
		Int("Missing", len(plan.Missing)).
		Int("Different", len(plan.Different)).
		Int("Extra", len(plan.Extra)).
		Msg("Checked the database schema")

	return db, nil
}
