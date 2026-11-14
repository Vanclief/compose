package sqlite

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

type patchOrg struct {
	bun.BaseModel `bun:"table:patch_orgs"`

	ID    int64   `bun:",pk,autoincrement"`
	Name  string  `bun:",notnull"`
	City  string  `bun:",notnull"`
	Phone *string `bun:"phone"`
}

func (o *patchOrg) Validate() error {
	if o.Name == "" {
		return ez.New(ez.EINVALID, "Name is required", nil)
	}

	return nil
}

// patchQueryCounter counts the queries a DB runs
type patchQueryCounter struct {
	count int
}

func (c *patchQueryCounter) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	c.count++
	return ctx
}

func (c *patchQueryCounter) AfterQuery(context.Context, *bun.QueryEvent) {}

// patchSetup opens a memory database holding one org and returns a loaded copy of it
func patchSetup(t *testing.T) (*relational.DB, *patchOrg) {
	db, err := ConnectToMemoryDatabase()
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Close() // nolint:errcheck // Closing an in-memory database only frees it
	})

	err = db.CreateTables([]interface{}{(*patchOrg)(nil)})
	require.NoError(t, err)

	phone := "111"
	org := &patchOrg{Name: "Acme", City: "Lima", Phone: &phone}
	_, err = db.NewInsert().Model(org).Exec(context.Background())
	require.NoError(t, err)

	return db, patchLoad(t, db, org.ID)
}

func patchLoad(t *testing.T, db *relational.DB, id int64) *patchOrg {
	org := &patchOrg{ID: id}
	err := db.NewSelect().Model(org).WherePK().Scan(context.Background())
	require.NoError(t, err)

	return org
}

func TestPatchWritesOnlyRecordedColumns(t *testing.T) {
	db, org := patchSetup(t)

	var patch relational.Patch
	relational.SetColumn(&patch, "name", &org.Name, types.Some("Beta"))
	org.City = "Cusco"

	err := patch.Update(context.Background(), db, org)
	require.NoError(t, err)

	stored := patchLoad(t, db, org.ID)
	assert.Equal(t, "Beta", stored.Name)
	assert.Equal(t, "Lima", stored.City)
	require.NotNil(t, stored.Phone)
	assert.Equal(t, "111", *stored.Phone)
}

func TestPatchAbsentKeyRecordsNothing(t *testing.T) {
	_, org := patchSetup(t)

	var patch relational.Patch
	relational.SetColumn(&patch, "name", &org.Name, types.Optional[string]{})
	relational.SetNullableColumn(&patch, "phone", &org.Phone, types.Optional[string]{})

	assert.Empty(t, patch.Columns())
	assert.Equal(t, "Acme", org.Name)
	require.NotNil(t, org.Phone)
	assert.Equal(t, "111", *org.Phone)
}

func TestPatchNullForNonNullableColumn(t *testing.T) {
	db, org := patchSetup(t)

	var patch relational.Patch
	relational.SetColumn(&patch, "name", &org.Name, types.Null[string]())
	relational.SetColumn(&patch, "city", &org.City, types.Some("Cusco"))

	assert.Equal(t, "Acme", org.Name)
	assert.Equal(t, []string{"city"}, patch.Columns())

	err := patch.Update(context.Background(), db, org)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Equal(t, "name", ez.ErrorData(err)["column"])

	stored := patchLoad(t, db, org.ID)
	assert.Equal(t, "Acme", stored.Name)
	assert.Equal(t, "Lima", stored.City)
}

func TestPatchSetNullableColumn(t *testing.T) {
	db, org := patchSetup(t)

	var nullPatch relational.Patch
	relational.SetNullableColumn(&nullPatch, "phone", &org.Phone, types.Null[string]())
	assert.Nil(t, org.Phone)
	assert.Equal(t, []string{"phone"}, nullPatch.Columns())

	err := nullPatch.Update(context.Background(), db, org)
	require.NoError(t, err)
	assert.Nil(t, patchLoad(t, db, org.ID).Phone)

	value := types.Some("222")
	var valuePatch relational.Patch
	relational.SetNullableColumn(&valuePatch, "phone", &org.Phone, value)
	require.NotNil(t, org.Phone)
	assert.NotSame(t, &value.Value, org.Phone)

	err = valuePatch.Update(context.Background(), db, org)
	require.NoError(t, err)

	stored := patchLoad(t, db, org.ID)
	require.NotNil(t, stored.Phone)
	assert.Equal(t, "222", *stored.Phone)
}

func TestPatchEmptyRunsNoQueryAndSkipsValidate(t *testing.T) {
	db, org := patchSetup(t)

	counter := &patchQueryCounter{}
	db.AddQueryHook(counter)

	// The model fails Validate, so a nil error shows Validate did not run
	org.Name = ""

	var patch relational.Patch
	err := patch.Update(context.Background(), db, org)
	require.NoError(t, err)
	assert.Equal(t, 0, counter.count)

	// The hook does count the queries that run
	patchLoad(t, db, org.ID)
	assert.Equal(t, 1, counter.count)
}

func TestPatchFailingValidateWritesNothing(t *testing.T) {
	db, org := patchSetup(t)

	var patch relational.Patch
	relational.SetColumn(&patch, "name", &org.Name, types.Some(""))
	relational.SetColumn(&patch, "city", &org.City, types.Some("Cusco"))

	err := patch.Update(context.Background(), db, org)
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	stored := patchLoad(t, db, org.ID)
	assert.Equal(t, "Acme", stored.Name)
	assert.Equal(t, "Lima", stored.City)
}

func TestPatchMissingRowIsNotFound(t *testing.T) {
	db, org := patchSetup(t)

	_, err := db.NewDelete().Model(org).WherePK().Exec(context.Background())
	require.NoError(t, err)

	var patch relational.Patch
	relational.SetColumn(&patch, "name", &org.Name, types.Some("Beta"))

	err = patch.Update(context.Background(), db, org)
	require.Error(t, err)
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
}

func TestPatchColumnsOrderAndDuplicates(t *testing.T) {
	db, org := patchSetup(t)

	var patch relational.Patch
	patch.Add("city")
	relational.SetColumn(&patch, "name", &org.Name, types.Some("Beta"))
	patch.Add("city")
	relational.SetColumn(&patch, "name", &org.Name, types.Some("Gamma"))
	assert.Equal(t, []string{"city", "name"}, patch.Columns())

	// The returned slice is a copy
	columns := patch.Columns()
	columns[0] = "phone"
	assert.Equal(t, []string{"city", "name"}, patch.Columns())

	err := patch.Update(context.Background(), db, org)
	require.NoError(t, err)
	assert.Equal(t, "Gamma", patchLoad(t, db, org.ID).Name)
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
		db, org := patchSetup(t)
		id := org.ID

		other := &patchOrg{Name: "Other", City: "Quito"}
		_, err := db.NewInsert().Model(other).Exec(ctx)
		require.NoError(t, err)

		var patch relational.Patch
		relational.SetColumn(&patch, "id", &org.ID, types.Some(other.ID))
		relational.SetColumn(&patch, "name", &org.Name, types.Some("Beta"))

		err = patch.Update(ctx, db, org)
		require.Error(t, err)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		assert.Equal(t, "id", ez.ErrorData(err)["column"])

		assert.Equal(t, "Acme", patchLoad(t, db, id).Name)
		assert.Equal(t, "Other", patchLoad(t, db, other.ID).Name)
	})

	t.Run("composite key", func(t *testing.T) {
		db, _ := patchSetup(t)

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
