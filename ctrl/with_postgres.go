package ctrl

import (
	"github.com/uptrace/bun/extra/bundebug"
	"github.com/vanclief/compose/interfaces/databases/postgres"
	"github.com/vanclief/ez"
)

func (c *BaseController) WithPostgres(cfg *postgres.ConnectionConfig, models []interface{}, options ...postgres.Option) (*postgres.DB, error) {
	const op = "BaseController.WithPostgres"

	db, err := postgres.ConnectToDatabase(cfg)
	if err != nil {
		return nil, ez.Wrap(op, err)
	}

	if cfg.Verbose {
		queryHook := bundebug.NewQueryHook(bundebug.WithVerbose(cfg.Verbose))
		db.AddQueryHook(queryHook)
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
