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

type mutateItem struct {
	bun.BaseModel `bun:"table:mutate_items"`

	OrgID int64  `bun:",pk"`
	Code  string `bun:",pk"`
	Name  string `bun:",notnull"`
}

func (m *mutateItem) Validate() error {
	return nil
}

func (m *mutateItem) Insert(ctx context.Context, db bun.IDB) error {
	_, err := db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

func (m *mutateItem) Update(ctx context.Context, db bun.IDB) error {
	_, err := db.NewUpdate().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

func (m *mutateItem) Delete(ctx context.Context, db bun.IDB) error {
	_, err := db.NewDelete().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

func TestMutateRefusesChangedPrimaryKey(t *testing.T) {
	ctx := context.Background()

	t.Run("single key", func(t *testing.T) {
		db, stored := mutateSetup(t)
		other := &mutateCounter{Name: "other"}
		err := other.Insert(ctx, db)
		require.NoError(t, err)

		m := &mutateCounter{ID: stored.ID}
		err = relational.Mutate(ctx, db, m, func(ctx context.Context, tx bun.Tx) error {
			_, err := tx.NewUpdate().Model((*mutateCounter)(nil)).Set("count = ?", 5).Where("id = ?", stored.ID).Exec(ctx)
			if err != nil {
				return ez.Wrap(err)
			}

			m.ID = other.ID
			m.Name = "moved"
			m.Count = 9
			return nil
		})
		require.Error(t, err)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		assert.Equal(t, "id", ez.ErrorData(err)["column"])

		assert.Equal(t, mutateCounter{ID: stored.ID, Name: "first"}, mutateLoad(t, db, stored.ID))
		assert.Equal(t, mutateCounter{ID: other.ID, Name: "other"}, mutateLoad(t, db, other.ID))
	})

	t.Run("composite key", func(t *testing.T) {
		db, _ := mutateSetup(t)
		err := db.CreateTables([]interface{}{(*mutateItem)(nil)})
		require.NoError(t, err)

		_, err = db.NewInsert().Model(&[]mutateItem{
			{OrgID: 1, Code: "A", Name: "row A"},
			{OrgID: 1, Code: "B", Name: "row B"},
		}).Exec(ctx)
		require.NoError(t, err)

		m := &mutateItem{OrgID: 1, Code: "A"}
		err = relational.Mutate(ctx, db, m, func(ctx context.Context, tx bun.Tx) error {
			m.Code = "B"
			m.Name = "moved"
			return nil
		})
		require.Error(t, err)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		assert.Equal(t, "code", ez.ErrorData(err)["column"])

		var stored []mutateItem
		err = db.NewSelect().Model(&stored).Order("code").Scan(ctx)
		require.NoError(t, err)
		assert.Equal(t, []mutateItem{
			{OrgID: 1, Code: "A", Name: "row A"},
			{OrgID: 1, Code: "B", Name: "row B"},
		}, stored)
	})

	t.Run("key set back before fn returns", func(t *testing.T) {
		db, stored := mutateSetup(t)
		m := &mutateCounter{ID: stored.ID}
		err := relational.Mutate(ctx, db, m, func(ctx context.Context, tx bun.Tx) error {
			m.ID = stored.ID + 1000
			m.ID = stored.ID
			m.Count++
			return nil
		})
		require.NoError(t, err)
		assert.Equal(t, int64(1), mutateLoad(t, db, stored.ID).Count)
	})
}
