package ctrl

import (
	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun/extra/bundebug"
	"github.com/vanclief/compose/interfaces/databases/relational"
	"github.com/vanclief/compose/interfaces/databases/relational/drivers/postgres"
	"github.com/vanclief/ez"
)

func (c *BaseController) WithPostgres(cfg *postgres.ConnectionConfig, models []interface{}, options ...relational.Option) (*relational.DB, error) {
	const op = "BaseController.WithPostgres"

	db, err := postgres.ConnectToDatabase(cfg)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	if cfg.Verbose {
		queryHook := bundebug.NewQueryHook(bundebug.WithVerbose(cfg.Verbose))
		db.AddQueryHook(queryHook)
		log.Info().Bool("Verbose", cfg.Verbose).Msg("Displaying database query logs")
	}

	for _, option := range options {
		if err := option(db); err != nil {
			return nil, ez.Wrap(op, err)
		}
	}

	err = db.CreateTables(models)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	return db, nil
}
