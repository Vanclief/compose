package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/ez"
)

// sqlMigration - A migration whose Up runs one query
func sqlMigration(name, query string) migrate.Migration {
	return migrate.Migration{
		Name: name,
		Up: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			_, err := migrator.DB().ExecContext(ctx, query)
			if err != nil {
				return ez.Wrap(err)
			}

			return nil
		},
	}
}

// sqlState - The SQLSTATE of the PostgreSQL error inside err
func sqlState(t *testing.T, err error) string {
	var pgErr pgdriver.Error
	require.True(t, errors.As(err, &pgErr), "not a PostgreSQL error: %v", err)

	return pgErr.Field('C')
}

func TestRunMigrationsIgnoresStatementTimeout(t *testing.T) {
	// The read timeout proves the migration pool does not inherit the main pool's socket deadline
	cfg := baselineConfig(t)
	cfg.StatementTimeout = 1000
	cfg.ReadTimeout = 2500
	db, _ := connectTestDB(t, cfg)
	ctx := context.Background()

	// A database default reaches every session that does not set its own
	// value; the migration pool has not opened one yet, so only its explicit
	// statement_timeout 0 keeps the sleep below from being cancelled
	_, err := db.NewRaw("ALTER DATABASE ? SET statement_timeout = '1s'", bun.Ident(cfg.Database)).Exec(ctx)
	require.NoError(t, err)

	// The main pool cancels the same query before its read deadline
	_, err = db.NewRaw("SELECT pg_sleep(3)").Exec(ctx)
	require.Error(t, err)
	assert.Equal(t, "57014", sqlState(t, err), "query_canceled")

	migrations := migrate.NewMigrations()
	migrations.Add(sqlMigration("20240101_sleep", "SELECT pg_sleep(3)"))

	err = db.RunMigrations(migrations)
	require.NoError(t, err)
	assert.Equal(t, []ledgerRow{{Name: "20240101_sleep", GroupID: 1}}, readLedger(t, db))
}

func TestRunMigrationsLockTimeout(t *testing.T) {
	// baselineConfig gives the main pool two connections: one holds the lock,
	// one reads the ledger
	cfg := baselineConfig(t)
	cfg.MigrationLockTimeout = 500
	db, _ := connectTestDB(t, cfg)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, "CREATE TABLE things (id bigserial PRIMARY KEY)")
	require.NoError(t, err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback() // nolint:errcheck // No-op after the explicit Rollback

	_, err = tx.ExecContext(ctx, "LOCK TABLE things IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)

	migrations := migrate.NewMigrations()
	migrations.Add(sqlMigration("20240101_x", "ALTER TABLE things ADD COLUMN x int"))

	// A regression would wait on the lock forever, so give up after 5 seconds
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- db.RunMigrations(migrations)
	}()

	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		tx.Rollback() // nolint:errcheck // Frees the waiting migration so the database can be dropped
		t.Fatal("RunMigrations still waits on the lock after 5 seconds")
	}

	require.Error(t, err)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Equal(t, "55P03", sqlState(t, err), "lock_not_available")
	assert.Empty(t, readLedger(t, db))

	err = tx.Rollback()
	require.NoError(t, err)

	err = db.RunMigrations(migrations)
	require.NoError(t, err)
	assert.True(t, columnExists(t, db, "things", "x"))
	assert.Equal(t, []ledgerRow{{Name: "20240101_x", GroupID: 1}}, readLedger(t, db))
}

func TestRunMigrationsDoesNotRollBackOnFailure(t *testing.T) {
	db, _ := newBaselineDB(t)

	// Without IF NOT EXISTS, so running it a second time would fail
	createThings := sqlMigration("20240101_one", "CREATE TABLE things (id bigserial PRIMARY KEY)")

	broken := migrate.NewMigrations()
	broken.Add(createThings)
	broken.Add(sqlMigration("20240102_two", "SELECT 1/0"))

	err := db.RunMigrations(broken)
	require.Error(t, err)
	assert.Equal(t, "22012", sqlState(t, err), "division_by_zero")
	assert.True(t, tableExists(t, db, "things"))
	assert.Equal(t, []ledgerRow{{Name: "20240101_one", GroupID: 1}}, readLedger(t, db))

	fixed := migrate.NewMigrations()
	fixed.Add(createThings)
	fixed.Add(sqlMigration("20240102_two", "CREATE TABLE shapes (id bigserial PRIMARY KEY)"))

	err = db.RunMigrations(fixed)
	require.NoError(t, err)
	assert.True(t, tableExists(t, db, "shapes"))
	expected := []ledgerRow{{Name: "20240101_one", GroupID: 1}, {Name: "20240102_two", GroupID: 2}}
	assert.Equal(t, expected, readLedger(t, db))
}

func TestRollbackLastMigrationKeepsRowWhenDownFails(t *testing.T) {
	db, _ := newBaselineDB(t)

	migration := sqlMigration("20240101_one", "SELECT 1")
	migration.Down = func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
		// Down runs on the migration pool like Up
		assert.Same(t, db.MigrationDB, migrator.DB())

		_, err := migrator.DB().ExecContext(ctx, "SELECT 1/0")
		if err != nil {
			return ez.Wrap(err)
		}

		return nil
	}

	migrations := migrate.NewMigrations()
	migrations.Add(migration)

	err := db.RunMigrations(migrations)
	require.NoError(t, err)

	err = db.RollbackLastMigration(migrations)
	require.Error(t, err)
	assert.Equal(t, "22012", sqlState(t, err), "division_by_zero")
	assert.Equal(t, []ledgerRow{{Name: "20240101_one", GroupID: 1}}, readLedger(t, db))
}

func TestRunMigrationsAllowsTwoConnectionsInOneMigration(t *testing.T) {
	// The main pool is capped at one connection; the migration pool must not
	// inherit the cap, or the query inside the transaction would wait for the
	// transaction's connection forever
	cfg := baselineConfig(t)
	cfg.MaxOpenConns = 1
	db, _ := connectTestDB(t, cfg)

	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{
		Name: "20240101_two_connections",
		Up: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			err := migrator.DB().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
				_, err := migrator.DB().ExecContext(ctx, "SELECT 1")
				if err != nil {
					return ez.Wrap(err)
				}

				return nil
			})
			if err != nil {
				return ez.Wrap(err)
			}

			return nil
		},
	})

	err := db.RunMigrations(migrations)
	require.NoError(t, err)
	assert.Equal(t, []ledgerRow{{Name: "20240101_two_connections", GroupID: 1}}, readLedger(t, db))
}

type order struct {
	ID    int64  `bun:",pk"`
	Items []item `bun:"m2m:order_to_items,join:Order=Item"`
}

type item struct {
	ID int64 `bun:",pk"`
}

type orderToItem struct {
	OrderID int64  `bun:",pk"`
	Order   *order `bun:"rel:belongs-to,join:order_id=id"`
	ItemID  int64  `bun:",pk"`
	Item    *item  `bun:"rel:belongs-to,join:item_id=id"`
}

func TestRunMigrationsSeesRegisteredModels(t *testing.T) {
	db, _ := newBaselineDB(t)
	ctx := context.Background()

	err := db.RegisterModels([]interface{}{(*orderToItem)(nil)})
	require.NoError(t, err)

	for _, model := range []interface{}{(*order)(nil), (*item)(nil), (*orderToItem)(nil)} {
		_, err = db.NewCreateTable().Model(model).Exec(ctx)
		require.NoError(t, err)
	}

	_, err = db.ExecContext(ctx, "INSERT INTO orders (id) VALUES (1)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO items (id) VALUES (7)")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "INSERT INTO order_to_items (order_id, item_id) VALUES (1, 7)")
	require.NoError(t, err)

	// bun panics on an m2m relation whose join model the dialect never registered
	var orders []order
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{
		Name: "20240101_read_orders",
		Up: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			err := migrator.DB().NewSelect().Model(&orders).Relation("Items").Scan(ctx)
			if err != nil {
				return ez.Wrap(err)
			}

			return nil
		},
	})

	err = db.RunMigrations(migrations)
	require.NoError(t, err)
	require.Len(t, orders, 1)
	assert.Equal(t, []item{{ID: 7}}, orders[0].Items)
}

func TestCloseClosesMigrationPool(t *testing.T) {
	db, _ := newBaselineDB(t)

	err := db.Close()
	require.NoError(t, err)

	err = db.MigrationDB.PingContext(context.Background())
	assert.ErrorContains(t, err, "sql: database is closed")
}
