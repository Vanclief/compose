package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/migrate"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/ez"
)

// concurrentHandles - Two handles to one fresh database, each with its own
// pools, standing in for two instances of an app booting together. It also
// creates concurrent_runs, where concurrentRecordingMigration records its runs.
func concurrentHandles(t *testing.T) (*relational.DB, *relational.DB) {
	first, cfg := newBaselineDB(t)

	second, err := ConnectToDatabase(&cfg)
	require.NoError(t, err)

	// Registered after connectTestDB's cleanup, so it runs before the database is dropped
	t.Cleanup(func() {
		second.Close() // nolint:errcheck // The test is over
	})

	_, err = first.ExecContext(context.Background(), "CREATE TABLE concurrent_runs (step text NOT NULL)")
	require.NoError(t, err)

	return first, second
}

// concurrentRun - Starts run on both handles at the same moment and returns
// both errors
func concurrentRun(first, second *relational.DB, run func(db *relational.DB) error) (error, error) {
	start := make(chan struct{})
	errs := make([]error, 2)

	var wg sync.WaitGroup
	for i, db := range []*relational.DB{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = run(db)
		}()
	}

	close(start)
	wg.Wait()

	return errs[0], errs[1]
}

// concurrentRecordStep - Inserts step into concurrent_runs and sleeps half a
// second, so a second run that does not wait overlaps the first
func concurrentRecordStep(ctx context.Context, migrator *migrate.Migrator, step string) error {
	_, err := migrator.DB().ExecContext(ctx, "INSERT INTO concurrent_runs (step) VALUES (?)", step)
	if err != nil {
		return ez.Wrap(err)
	}

	_, err = migrator.DB().ExecContext(ctx, "SELECT pg_sleep(0.5)")
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// concurrentRecordingMigration - A migration whose Up and Down each record
// one run in concurrent_runs
func concurrentRecordingMigration(name string) *migrate.Migrations {
	migrations := migrate.NewMigrations()
	migrations.Add(migrate.Migration{
		Name: name,
		Up: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			return concurrentRecordStep(ctx, migrator, "up")
		},
		Down: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			return concurrentRecordStep(ctx, migrator, "down")
		},
	})

	return migrations
}

// concurrentSteps - How many times step was recorded in concurrent_runs
func concurrentSteps(t *testing.T, db *relational.DB, step string) int {
	var count int
	err := db.NewRaw("SELECT count(*) FROM concurrent_runs WHERE step = ?", step).Scan(context.Background(), &count)
	require.NoError(t, err)

	return count
}

// migrationLockHeld - Whether any session holds an advisory lock on db's database
func migrationLockHeld(t *testing.T, db *relational.DB) bool {
	var held bool
	err := db.NewRaw(`SELECT EXISTS (
		SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND granted
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
	)`).Scan(context.Background(), &held)
	require.NoError(t, err)

	return held
}

// migrationLockRunWithin - Runs RunMigrations and fails the test when it has
// not returned after limit, as it would when it waits on a lock nobody releases
func migrationLockRunWithin(t *testing.T, db *relational.DB, migrations *migrate.Migrations, limit time.Duration) error {
	done := make(chan error, 1)
	go func() {
		done <- db.RunMigrations(migrations)
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(limit):
		t.Fatalf("RunMigrations still waits after %s", limit)
		return nil
	}
}

func TestConcurrentRunMigrationsRunsUpOnce(t *testing.T) {
	// Without the lock a fresh database fails on bun creating its tables twice,
	// and one with a ledger runs Up twice and writes two rows
	for _, withLedger := range []bool{false, true} {
		name := "fresh"
		if withLedger {
			name = "existing ledger"
		}

		t.Run(name, func(t *testing.T) {
			first, second := concurrentHandles(t)
			migrations := concurrentRecordingMigration("20240101_record")

			if withLedger {
				err := migrate.NewMigrator(first.DB, migrate.NewMigrations()).Init(context.Background())
				require.NoError(t, err)
			}

			firstErr, secondErr := concurrentRun(first, second, func(db *relational.DB) error {
				return db.RunMigrations(migrations)
			})
			require.NoError(t, firstErr)
			require.NoError(t, secondErr)

			assert.Equal(t, 1, concurrentSteps(t, first, "up"))
			assert.Equal(t, []ledgerRow{{Name: "20240101_record", GroupID: 1}}, readLedger(t, first))
			assert.False(t, migrationLockHeld(t, first))
		})
	}
}

func TestRollbackLastMigrationFailsWhileAnotherRollsBack(t *testing.T) {
	first, second := concurrentHandles(t)

	// Two groups: each RunMigrations applies what is pending as one group
	err := first.RunMigrations(concurrentRecordingMigration("20240101_record"))
	require.NoError(t, err)

	// The Down of the last group reports that it runs and then waits, so the
	// second rollback is certain to start while the first holds the lock
	running := make(chan struct{})
	finish := make(chan struct{})

	migrations := concurrentRecordingMigration("20240101_record")
	migrations.Add(migrate.Migration{
		Name: "20240102_blocking",
		Up: func(context.Context, *migrate.Migrator, *migrate.Migration) error {
			return nil
		},
		Down: func(ctx context.Context, migrator *migrate.Migrator, _ *migrate.Migration) error {
			close(running)
			<-finish

			return concurrentRecordStep(ctx, migrator, "down")
		},
	})

	err = first.RunMigrations(migrations)
	require.NoError(t, err)

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.RollbackLastMigration(migrations)
	}()

	select {
	case <-running:
	case err = <-firstDone:
		t.Fatalf("The first rollback returned before its Down ran: %v", err)
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- second.RollbackLastMigration(migrations)
	}()

	// Had the second rollback waited for the lock, it would have undone the
	// group before the last one once the first was done
	select {
	case err = <-secondDone:
	case <-time.After(10 * time.Second):
		close(finish)
		t.Fatal("The second rollback waited for the first instead of failing")
	}
	close(finish)

	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))

	err = <-firstDone
	require.NoError(t, err)

	assert.Equal(t, 1, concurrentSteps(t, first, "down"))
	assert.Equal(t, []ledgerRow{{Name: "20240101_record", GroupID: 1}}, readLedger(t, first))
	assert.False(t, migrationLockHeld(t, first))
}

func TestConcurrentRunMigrationsFromBaselineLoadsOnce(t *testing.T) {
	sql := dumpBaseline(t)
	first, second := concurrentHandles(t)
	baseline := relational.Baseline{SQL: sql, Floor: "20240101_one"}

	firstErr, secondErr := concurrentRun(first, second, func(db *relational.DB) error {
		return db.RunMigrationsFromBaseline(baseline, colorMigrations())
	})
	require.NoError(t, firstErr)
	require.NoError(t, secondErr)

	assert.True(t, columnExists(t, first, "things", "color"))
	expected := []ledgerRow{{Name: "20240101_one", GroupID: 1}, {Name: "20240102_two", GroupID: 2}}
	assert.Equal(t, expected, readLedger(t, first))
	assert.False(t, migrationLockHeld(t, first))
}

func TestMigrationLockReleasedAfterFailedMigration(t *testing.T) {
	first, second := concurrentHandles(t)

	broken := migrate.NewMigrations()
	broken.Add(sqlMigration("20240101_one", "SELECT 1/0"))

	err := first.RunMigrations(broken)
	require.Error(t, err)
	assert.Equal(t, "22012", sqlState(t, err), "division_by_zero")
	assert.False(t, migrationLockHeld(t, first))

	fixed := migrate.NewMigrations()
	fixed.Add(sqlMigration("20240101_one", "SELECT 1"))

	// Another instance would wait forever on a lock left behind
	err = migrationLockRunWithin(t, second, fixed, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, []ledgerRow{{Name: "20240101_one", GroupID: 1}}, readLedger(t, second))
}

func TestMigrationLockReleasedAfterPanic(t *testing.T) {
	first, second := concurrentHandles(t)

	panicking := migrate.NewMigrations()
	panicking.Add(migrate.Migration{
		Name: "20240101_one",
		Up: func(context.Context, *migrate.Migrator, *migrate.Migration) error {
			panic("migration lock test")
		},
	})

	assert.PanicsWithValue(t, "migration lock test", func() {
		first.RunMigrations(panicking) // nolint:errcheck // It panics before returning
	})
	assert.False(t, migrationLockHeld(t, first))

	err := migrationLockRunWithin(t, second, concurrentRecordingMigration("20240101_one"), 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 1, concurrentSteps(t, second, "up"))
}

func TestMigrationLockPoolSetsTCPKeepalives(t *testing.T) {
	// SHOW reads the values back from the socket, in seconds without a unit.
	// The tests connect over TCP; on a Unix socket the server reports 0 for all three
	db, _ := newBaselineDB(t)
	ctx := context.Background()

	settings := map[string]string{
		"tcp_keepalives_idle":     "30",
		"tcp_keepalives_interval": "10",
		"tcp_keepalives_count":    "3",
	}

	for name, expected := range settings {
		var value string
		err := db.MigrationDB.NewRaw("SHOW "+name).Scan(ctx, &value)
		require.NoError(t, err)
		assert.Equal(t, expected, value, name)
	}
}

func TestMigrationLockPoolTurnsOffIdleSessionTimeout(t *testing.T) {
	// A database default reaches every session that does not set its own
	// value. With it the server would end the session that holds the migration
	// lock, which runs no statement, in the middle of a migration
	db, cfg := newBaselineDB(t)
	ctx := context.Background()

	_, err := db.NewRaw("ALTER DATABASE ? SET idle_session_timeout = '1s'", bun.Ident(cfg.Database)).Exec(ctx)
	require.NoError(t, err)

	var value string
	err = db.MigrationDB.NewRaw("SHOW idle_session_timeout").Scan(ctx, &value)
	require.NoError(t, err)
	assert.Equal(t, "0", value)
}
