package postgres

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/ez"
)

type mutateCounter struct {
	bun.BaseModel `bun:"table:mutate_counters"`

	ID    int64  `bun:",pk,autoincrement"`
	Name  string `bun:",notnull"`
	Count int64  `bun:",notnull"`
}

func (m *mutateCounter) Validate() error {
	if m.Name == "" {
		return ez.New(ez.EINVALID, "Name is required", nil)
	}

	return nil
}

func (m *mutateCounter) Insert(ctx context.Context, db bun.IDB) error {
	err := m.Validate()
	if err != nil {
		return ez.Wrap(err)
	}

	_, err = db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

func (m *mutateCounter) Update(ctx context.Context, db bun.IDB) error {
	err := m.Validate()
	if err != nil {
		return ez.Wrap(err)
	}

	_, err = db.NewUpdate().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

func (m *mutateCounter) Delete(ctx context.Context, db bun.IDB) error {
	_, err := db.NewDelete().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// mutateSetup - A database with the counters table and one stored counter.
// maxOpenConns raises the two connection cap of baselineConfig.
func mutateSetup(t *testing.T, maxOpenConns int) (*relational.DB, *mutateCounter) {
	cfg := baselineConfig(t)
	cfg.MaxOpenConns = maxOpenConns
	db, _ := connectTestDB(t, cfg)

	err := db.CreateTables([]interface{}{(*mutateCounter)(nil)})
	require.NoError(t, err)

	stored := &mutateCounter{Name: "first"}
	err = stored.Insert(context.Background(), db)
	require.NoError(t, err)

	return db, stored
}

// mutateLoad - The committed row, read outside any transaction
func mutateLoad(t *testing.T, db bun.IDB, id int64) mutateCounter {
	loaded := mutateCounter{ID: id}
	err := db.NewSelect().Model(&loaded).WherePK().Scan(context.Background())
	require.NoError(t, err)

	return loaded
}

// mutateWaitForLockWaiter - Returns once PostgreSQL reports a session of this
// database waiting on a lock, and fails the test if none does within 10 s
func mutateWaitForLockWaiter(t *testing.T, db *relational.DB) {
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		var waiting int
		err := db.NewRaw(
			"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'",
		).Scan(context.Background(), &waiting)
		require.NoError(t, err)

		if waiting > 0 {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("No session started waiting on the row lock")
}

func TestMutateKeepsEveryConcurrentIncrement(t *testing.T) {
	const writers = 20
	db, stored := mutateSetup(t, writers+2)
	ctx := context.Background()

	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			m := &mutateCounter{ID: stored.ID}
			errs <- relational.Mutate(ctx, db, m, func(ctx context.Context, tx bun.Tx) error {
				m.Count++
				// Holds the window between the read and the write open, so
				// without the lock the writers would overlap in it
				time.Sleep(5 * time.Millisecond)
				return nil
			})
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	assert.Equal(t, int64(writers), mutateLoad(t, db, stored.ID).Count)
}

func TestLockRowMakesSecondLockWaitForCommit(t *testing.T) {
	db, stored := mutateSetup(t, 4)
	ctx := context.Background()

	first, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer first.Rollback() // nolint:errcheck // After the Commit below this returns sql.ErrTxDone

	held := &mutateCounter{ID: stored.ID}
	err = relational.LockRow(ctx, first, held)
	require.NoError(t, err)

	held.Count = 7
	err = held.Update(ctx, first)
	require.NoError(t, err)

	type lockResult struct {
		loaded         mutateCounter
		afterCommitted bool
		err            error
	}

	var committed atomic.Bool
	done := make(chan lockResult, 1)
	go func() {
		second, err := db.BeginTx(ctx, nil)
		if err != nil {
			done <- lockResult{err: err}
			return
		}
		defer second.Rollback() // nolint:errcheck // Only reads, nothing to keep

		loaded := mutateCounter{ID: stored.ID}
		err = relational.LockRow(ctx, second, &loaded)
		done <- lockResult{loaded: loaded, afterCommitted: committed.Load(), err: err}
	}()

	mutateWaitForLockWaiter(t, db)

	// A plain read is not blocked by the lock and sees the last committed row
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	plain := mutateCounter{ID: stored.ID}
	err = db.NewSelect().Model(&plain).WherePK().Scan(readCtx)
	require.NoError(t, err)
	assert.Equal(t, int64(0), plain.Count)

	select {
	case <-done:
		t.Fatal("The second LockRow returned while the first transaction held the row")
	default:
	}

	committed.Store(true)
	err = first.Commit()
	require.NoError(t, err)

	result := <-done
	require.NoError(t, result.err)
	assert.True(t, result.afterCommitted)
	assert.Equal(t, int64(7), result.loaded.Count)
}

func TestMutateLoadsStoredRowBeforeFn(t *testing.T) {
	db, stored := mutateSetup(t, 2)
	ctx := context.Background()

	m := &mutateCounter{ID: stored.ID, Name: "stale", Count: 99}
	err := relational.Mutate(ctx, db, m, func(ctx context.Context, tx bun.Tx) error {
		assert.Equal(t, "first", m.Name)
		assert.Equal(t, int64(0), m.Count)
		m.Count++
		return nil
	})
	require.NoError(t, err)

	loaded := mutateLoad(t, db, stored.ID)
	assert.Equal(t, "first", loaded.Name)
	assert.Equal(t, int64(1), loaded.Count)
}

func TestMutateRollsBackWhenFnFails(t *testing.T) {
	db, stored := mutateSetup(t, 2)
	ctx := context.Background()

	m := &mutateCounter{ID: stored.ID}
	err := relational.Mutate(ctx, db, m, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.NewUpdate().Model((*mutateCounter)(nil)).Set("count = ?", 5).Where("id = ?", stored.ID).Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}

		m.Count = 6
		return ez.New(ez.ECONFLICT, "The counter is closed", nil)
	})
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))

	assert.Equal(t, int64(0), mutateLoad(t, db, stored.ID).Count)
}

func TestMutateAndLockRowReportMissingRow(t *testing.T) {
	db, stored := mutateSetup(t, 2)
	ctx := context.Background()
	missing := stored.ID + 1000

	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return relational.LockRow(ctx, tx, &mutateCounter{ID: missing})
	})
	require.Error(t, err)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))

	called := false
	err = relational.Mutate(ctx, db, &mutateCounter{ID: missing}, func(ctx context.Context, tx bun.Tx) error {
		called = true
		return nil
	})
	require.Error(t, err)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	assert.False(t, called)
}

func TestMutateInsideCallerTransaction(t *testing.T) {
	db, stored := mutateSetup(t, 2)
	ctx := context.Background()

	increment := func(m *mutateCounter) func(ctx context.Context, tx bun.Tx) error {
		return func(ctx context.Context, tx bun.Tx) error {
			m.Count++
			return nil
		}
	}

	t.Run("fn gets the ctx given to Mutate", func(t *testing.T) {
		outer, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer outer.Rollback() // nolint:errcheck // Nothing here is kept

		// bun hands a nested closure the outer transaction's ctx, which
		// does not carry this value
		type mutateCtxKey struct{}
		callCtx := context.WithValue(ctx, mutateCtxKey{}, "call")

		var got any
		m := &mutateCounter{ID: stored.ID}
		err = relational.Mutate(callCtx, outer, m, func(ctx context.Context, tx bun.Tx) error {
			got = ctx.Value(mutateCtxKey{})
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, "call", got)
	})

	t.Run("rollback discards", func(t *testing.T) {
		outer, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer outer.Rollback() // nolint:errcheck // After the Rollback below this returns sql.ErrTxDone

		m := &mutateCounter{ID: stored.ID}
		err = relational.Mutate(ctx, outer, m, increment(m))
		require.NoError(t, err)
		assert.Equal(t, int64(1), mutateLoad(t, outer, stored.ID).Count)

		err = outer.Rollback()
		require.NoError(t, err)
		assert.Equal(t, int64(0), mutateLoad(t, db, stored.ID).Count)
	})

	t.Run("commit keeps", func(t *testing.T) {
		outer, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer outer.Rollback() // nolint:errcheck // After the Commit below this returns sql.ErrTxDone

		// A failing Mutate rolls back to its savepoint and leaves outer usable
		failed := &mutateCounter{ID: stored.ID}
		err = relational.Mutate(ctx, outer, failed, func(ctx context.Context, tx bun.Tx) error {
			failed.Count = 50
			return ez.New(ez.ECONFLICT, "The counter is closed", nil)
		})
		require.Error(t, err)
		assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))

		m := &mutateCounter{ID: stored.ID}
		err = relational.Mutate(ctx, outer, m, increment(m))
		require.NoError(t, err)

		err = outer.Commit()
		require.NoError(t, err)
		assert.Equal(t, int64(1), mutateLoad(t, db, stored.ID).Count)
	})
}

func TestMutateRollsBackFnWritesWhenUpdateFails(t *testing.T) {
	db, stored := mutateSetup(t, 2)
	ctx := context.Background()

	m := &mutateCounter{ID: stored.ID}
	err := relational.Mutate(ctx, db, m, func(ctx context.Context, tx bun.Tx) error {
		other := &mutateCounter{Name: "second"}
		err := other.Insert(ctx, tx)
		if err != nil {
			return ez.Wrap(err)
		}

		m.Count = 3
		m.Name = ""
		return nil
	})
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	rows, err := db.NewSelect().Model((*mutateCounter)(nil)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rows)

	loaded := mutateLoad(t, db, stored.ID)
	assert.Equal(t, "first", loaded.Name)
	assert.Equal(t, int64(0), loaded.Count)
}
