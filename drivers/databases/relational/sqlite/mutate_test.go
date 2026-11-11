package sqlite

import (
	"context"
	"testing"

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

// mutateSetup - An in-memory database with the counters table and one stored counter
func mutateSetup(t *testing.T) (*relational.DB, *mutateCounter) {
	db, err := ConnectToMemoryDatabase()
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Close() // nolint:errcheck // Closing an in-memory database only frees it
	})

	err = db.CreateTables([]interface{}{(*mutateCounter)(nil)})
	require.NoError(t, err)

	stored := &mutateCounter{Name: "first"}
	err = stored.Insert(context.Background(), db)
	require.NoError(t, err)

	return db, stored
}

// mutateLoad - The committed row, read outside any transaction
func mutateLoad(t *testing.T, db *relational.DB, id int64) mutateCounter {
	loaded := mutateCounter{ID: id}
	err := db.NewSelect().Model(&loaded).WherePK().Scan(context.Background())
	require.NoError(t, err)

	return loaded
}

func TestMutateLoadsRunsFnAndUpdates(t *testing.T) {
	db, stored := mutateSetup(t)
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

func TestMutateAndLockRowReportMissingRow(t *testing.T) {
	db, stored := mutateSetup(t)
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

func TestMutateRollsBackWhenFnFails(t *testing.T) {
	db, stored := mutateSetup(t)
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
