package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/compose/types"
	"github.com/vanclief/ez"
)

type patchAccount struct {
	bun.BaseModel `bun:"table:patch_accounts"`

	ID   int64  `bun:",pk,autoincrement"`
	Name string `bun:",notnull"`
	City string `bun:",notnull"`
}

func (a *patchAccount) Validate() error {
	return nil
}

// patchSetup creates a database holding one account and returns its ID
func patchSetup(t *testing.T) (*relational.DB, int64) {
	db, _ := newBaselineDB(t)

	err := db.CreateTables([]interface{}{(*patchAccount)(nil)})
	require.NoError(t, err)

	account := &patchAccount{Name: "Acme", City: "Lima"}
	_, err = db.NewInsert().Model(account).Exec(context.Background())
	require.NoError(t, err)

	return db, account.ID
}

func patchLoad(t *testing.T, db *relational.DB, id int64) *patchAccount {
	account := &patchAccount{ID: id}
	err := db.NewSelect().Model(account).WherePK().Scan(context.Background())
	require.NoError(t, err)

	return account
}

func TestPatchKeepsConcurrentEditsOfOtherColumns(t *testing.T) {
	db, id := patchSetup(t)
	ctx := context.Background()

	// Two requests load the same row before either writes
	first := patchLoad(t, db, id)
	second := patchLoad(t, db, id)

	var firstPatch relational.Patch
	relational.SetColumn(&firstPatch, "name", &first.Name, types.Some("Beta"))
	err := firstPatch.Update(ctx, db, first)
	require.NoError(t, err)

	// A column recorded twice is written once, which PostgreSQL requires
	var secondPatch relational.Patch
	relational.SetColumn(&secondPatch, "city", &second.City, types.Some("Cusco"))
	secondPatch.Add("city")
	err = secondPatch.Update(ctx, db, second)
	require.NoError(t, err)

	stored := patchLoad(t, db, id)
	assert.Equal(t, "Beta", stored.Name)
	assert.Equal(t, "Cusco", stored.City)
}

func TestPatchMissingRowIsNotFound(t *testing.T) {
	db, id := patchSetup(t)
	ctx := context.Background()

	account := patchLoad(t, db, id)
	_, err := db.NewDelete().Model(account).WherePK().Exec(ctx)
	require.NoError(t, err)

	var patch relational.Patch
	relational.SetColumn(&patch, "name", &account.Name, types.Some("Beta"))

	err = patch.Update(ctx, db, account)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
}

type patchItem struct {
	bun.BaseModel `bun:"table:patch_items"`

	OrgID int64  `bun:",pk"`
	Code  string `bun:",pk"`
	Name  string `bun:",notnull"`
}

func (i *patchItem) Validate() error {
	return nil
}

func TestPatchRefusesPrimaryKeyColumn(t *testing.T) {
	ctx := context.Background()

	t.Run("single key", func(t *testing.T) {
		db, id := patchSetup(t)

		other := &patchAccount{Name: "Other", City: "Quito"}
		_, err := db.NewInsert().Model(other).Exec(ctx)
		require.NoError(t, err)

		account := patchLoad(t, db, id)
		var patch relational.Patch
		relational.SetColumn(&patch, "id", &account.ID, types.Some(other.ID))
		relational.SetColumn(&patch, "name", &account.Name, types.Some("Beta"))

		err = patch.Update(ctx, db, account)
		require.Error(t, err)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		assert.Equal(t, "id", ez.ErrorData(err)["column"])

		assert.Equal(t, "Acme", patchLoad(t, db, id).Name)
		assert.Equal(t, "Other", patchLoad(t, db, other.ID).Name)
	})

	t.Run("composite key", func(t *testing.T) {
		db, _ := newBaselineDB(t)

		err := db.CreateTables([]interface{}{(*patchItem)(nil)})
		require.NoError(t, err)

		_, err = db.NewInsert().Model(&[]patchItem{
			{OrgID: 1, Code: "A", Name: "row A"},
			{OrgID: 1, Code: "B", Name: "row B"},
		}).Exec(ctx)
		require.NoError(t, err)

		item := &patchItem{OrgID: 1, Code: "A", Name: "row A"}
		var patch relational.Patch
		relational.SetColumn(&patch, "name", &item.Name, types.Some("renamed"))
		relational.SetColumn(&patch, "code", &item.Code, types.Some("B"))

		err = patch.Update(ctx, db, item)
		require.Error(t, err)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		assert.Equal(t, "code", ez.ErrorData(err)["column"])

		var stored []patchItem
		err = db.NewSelect().Model(&stored).Order("code").Scan(ctx)
		require.NoError(t, err)
		assert.Equal(t, []patchItem{
			{OrgID: 1, Code: "A", Name: "row A"},
			{OrgID: 1, Code: "B", Name: "row B"},
		}, stored)
	})
}
