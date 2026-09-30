package relational

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/vanclief/ez"

	// Registers the cgo-free "sqlite" database/sql driver
	_ "modernc.org/sqlite"
)

// migrateParent - A table with a declared index
type migrateParent struct {
	bun.BaseModel `bun:"table:migrate_parents"`

	ID   int64  `bun:",pk,autoincrement"`
	Code string `bun:",notnull"`
}

func (*migrateParent) Indexes() []Index {
	return []Index{{Name: "migrate_parents_code_idx", Def: "(code)"}}
}

// migrateParentV2 - migrate_parents with a column the database may lack. Its
// value receiver checks declarations are found through either receiver
type migrateParentV2 struct {
	bun.BaseModel `bun:"table:migrate_parents"`

	ID   int64  `bun:",pk,autoincrement"`
	Code string `bun:",notnull"`
	Note string `bun:",notnull,default:''"`
}

func (migrateParentV2) Indexes() []Index {
	return []Index{{Name: "migrate_parents_code_idx", Def: "(code)"}}
}

// migrateChild - A second table with a declared index
type migrateChild struct {
	bun.BaseModel `bun:"table:migrate_children"`

	ID       int64 `bun:",pk,autoincrement"`
	ParentID int64 `bun:",notnull"`
}

func (*migrateChild) Indexes() []Index {
	return []Index{{Name: "migrate_children_parent_idx", Def: "(parent_id)"}}
}

// migrateClash - Declares an index name migrate_parents already uses
type migrateClash struct {
	bun.BaseModel `bun:"table:migrate_clashes"`

	ID int64 `bun:",pk,autoincrement"`
}

func (*migrateClash) Indexes() []Index {
	return []Index{{Name: "migrate_parents_code_idx", Def: "(id)"}}
}

// migrateChecked - Declares a constraint, which SQLite does not support
type migrateChecked struct {
	bun.BaseModel `bun:"table:migrate_checked"`

	ID int64 `bun:",pk,autoincrement"`
}

func (*migrateChecked) Constraints() []Constraint {
	return []Constraint{{Name: "migrate_checked_id_check", Def: "CHECK (id > 0)"}}
}

// migrateQualified - A schema-qualified table, which the engine refuses
type migrateQualified struct {
	bun.BaseModel `bun:"table:other.migrate_qualified"`

	ID int64 `bun:",pk,autoincrement"`
}

// newMigrateDB - An in-memory SQLite database pinned to one connection, as
// sqlite.ConnectToMemoryDatabase opens it. With a single connection, any
// work Migrate did outside its pinned connection would deadlock the test
func newMigrateDB(t *testing.T) *DB {
	sqldb, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	require.NoError(t, err)

	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)
	sqldb.SetConnMaxLifetime(0)
	sqldb.SetConnMaxIdleTime(0)

	db := &DB{DB: bun.NewDB(sqldb, sqlitedialect.New())}
	t.Cleanup(func() {
		db.Close() // nolint:errcheck // Closing an in-memory test database
	})

	return db
}

// recordingStep - A step that uses the connection it is given and appends
// its name to ran
func recordingStep(name string, ran *[]string) Step {
	return Step{
		Name: name,
		Run: func(ctx context.Context, db bun.IDB) error {
			_, err := db.ExecContext(ctx, "SELECT 1")
			if err != nil {
				return err
			}

			*ran = append(*ran, name)
			return nil
		},
	}
}

// ledgerNames - The ledger's step names in the order they were recorded
func ledgerNames(t *testing.T, db *DB) []string {
	var names []string
	err := db.NewRaw("SELECT name FROM schema_steps ORDER BY id").Scan(context.Background(), &names)
	require.NoError(t, err)

	return names
}

// sqliteObjectExists - Whether sqlite_master has a table or index by name
func sqliteObjectExists(t *testing.T, db *DB, kind, name string) bool {
	var count int
	err := db.NewRaw("SELECT count(*) FROM sqlite_master WHERE type = ? AND name = ?", kind, name).Scan(context.Background(), &count)
	require.NoError(t, err)

	return count > 0
}

func TestDiff(t *testing.T) {
	desired := Catalog{
		"accounts": {
			Columns: map[string]Column{
				"id":     {Type: "bigint", NotNull: true, Default: "nextval('accounts_id_seq'::regclass)"},
				"email":  {Type: "text", NotNull: true},
				"status": {Type: "text", NotNull: true, Default: "'active'::text"},
			},
			Indexes:     map[string]string{"accounts_status_idx": "CREATE INDEX accounts_status_idx ON accounts USING btree (status)"},
			Constraints: map[string]string{"accounts_pkey": "PRIMARY KEY (id)", "accounts_email_check": "CHECK ((email <> ''::text))"},
		},
		"transfers": {
			Columns: map[string]Column{"id": {Type: "bigint", NotNull: true}},
		},
	}

	live := Catalog{
		"accounts": {
			Columns: map[string]Column{
				"id":     {Type: "bigint", NotNull: true, Default: "nextval('accounts_id_seq'::regclass)"},
				"status": {Type: "text", NotNull: true, Default: "'pending'::text"},
				"legacy": {Type: "text"},
			},
			Indexes: map[string]string{
				"accounts_status_idx": "CREATE INDEX accounts_status_idx ON accounts USING btree (status)",
				"accounts_legacy_idx": "CREATE INDEX accounts_legacy_idx ON accounts USING btree (legacy)",
			},
			Constraints: map[string]string{"accounts_pkey": "PRIMARY KEY (id)"},
		},
		"old_things": {
			Columns: map[string]Column{"id": {Type: "integer"}},
		},
	}

	plan := diff(desired, live)

	assert.Equal(t, []Change{
		{Kind: "column", Table: "accounts", Name: "email", Want: "text NOT NULL"},
		{Kind: "constraint", Table: "accounts", Name: "accounts_email_check", Want: "CHECK ((email <> ''::text))"},
		{Kind: "table", Table: "transfers", Name: "transfers"},
	}, plan.Missing)

	assert.Equal(t, []Change{
		{Kind: "column", Table: "accounts", Name: "status", Want: "text NOT NULL DEFAULT 'active'::text", Have: "text NOT NULL DEFAULT 'pending'::text"},
	}, plan.Different)

	assert.Equal(t, []Change{
		{Kind: "column", Table: "accounts", Name: "legacy", Have: "text"},
		{Kind: "index", Table: "accounts", Name: "accounts_legacy_idx", Have: "CREATE INDEX accounts_legacy_idx ON accounts USING btree (legacy)"},
		{Kind: "table", Table: "old_things", Name: "old_things"},
	}, plan.Extra)

	// Identical catalogs have nothing to report
	assert.Equal(t, Plan{}, diff(live, live))
}

func TestNormalizeDefaults(t *testing.T) {
	catalog := Catalog{
		"accounts": {
			Columns: map[string]Column{
				"temp":     {Type: "bigint", Default: "nextval('pg_temp_3.accounts_id_seq'::regclass)"},
				"session":  {Type: "bigint", Default: "nextval('pg_temp.accounts_id_seq'::regclass)"},
				"current":  {Type: "bigint", Default: "public.next_code()"},
				"other":    {Type: "bigint", Default: "xpublic.next_code()"},
				"constant": {Type: "text", Default: "'public'::text"},
			},
		},
	}

	columns := normalizeDefaults(catalog, "public")["accounts"].Columns
	assert.Equal(t, "nextval('accounts_id_seq'::regclass)", columns["temp"].Default)
	assert.Equal(t, "nextval('accounts_id_seq'::regclass)", columns["session"].Default)
	assert.Equal(t, "next_code()", columns["current"].Default)
	assert.Equal(t, "xpublic.next_code()", columns["other"].Default)
	assert.Equal(t, "'public'::text", columns["constant"].Default)

	// The raw catalog, which Apply executes from, is left alone
	assert.Equal(t, "public.next_code()", catalog["accounts"].Columns["current"].Default)

	quoted := Catalog{"t": {Columns: map[string]Column{"c": {Default: `"My Schema".next_code()`}}}}
	assert.Equal(t, "next_code()", normalizeDefaults(quoted, `"My Schema"`)["t"].Columns["c"].Default)
}

func TestMigrateFreshStampsStepsWithoutRunning(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)

	var ran []string
	plan, err := db.Migrate(ctx, Schema{
		Models: []interface{}{(*migrateParent)(nil), (*migrateChild)(nil)},
		Steps:  []Step{recordingStep("first", &ran), recordingStep("second", &ran)},
		Floor:  "deleted",
	})
	require.NoError(t, err)

	assert.Equal(t, Plan{}, plan)
	assert.Empty(t, ran, "a fresh database is created at the declared shape")
	assert.Equal(t, []string{"deleted", "first", "second"}, ledgerNames(t, db))
	assert.True(t, sqliteObjectExists(t, db, "table", "migrate_parents"))
	assert.True(t, sqliteObjectExists(t, db, "index", "migrate_parents_code_idx"))
	assert.True(t, sqliteObjectExists(t, db, "index", "migrate_children_parent_idx"))
}

func TestMigrateRunsPendingStepsInOrder(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)
	models := []interface{}{(*migrateParent)(nil)}

	var ran []string
	_, err := db.Migrate(ctx, Schema{Models: models, Steps: []Step{recordingStep("first", &ran)}})
	require.NoError(t, err)

	schema := Schema{
		Models: models,
		Steps:  []Step{recordingStep("first", &ran), recordingStep("second", &ran), recordingStep("third", &ran)},
	}

	plan, err := db.Migrate(ctx, schema)
	require.NoError(t, err)
	assert.Equal(t, Plan{}, plan)
	assert.Equal(t, []string{"second", "third"}, ran)
	assert.Equal(t, []string{"first", "second", "third"}, ledgerNames(t, db))

	// Nothing is pending on the next boot
	_, err = db.Migrate(ctx, schema)
	require.NoError(t, err)
	assert.Equal(t, []string{"second", "third"}, ran)
}

func TestMigrateFailingStepStaysPending(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)
	models := []interface{}{(*migrateParent)(nil)}

	_, err := db.Migrate(ctx, Schema{Models: models})
	require.NoError(t, err)

	var ran []string
	failing := true
	flaky := Step{
		Name: "flaky",
		Run: func(ctx context.Context, db bun.IDB) error {
			if failing {
				return errors.New("backfill failed")
			}

			ran = append(ran, "flaky")
			return nil
		},
	}
	schema := Schema{
		Models: models,
		Steps:  []Step{recordingStep("first", &ran), flaky, recordingStep("third", &ran)},
	}

	_, err = db.Migrate(ctx, schema)
	require.Error(t, err)
	assert.Equal(t, []string{"first"}, ran, "the steps after a failure do not run")
	assert.Equal(t, []string{"first"}, ledgerNames(t, db))

	failing = false
	_, err = db.Migrate(ctx, schema)
	require.NoError(t, err)
	assert.Equal(t, []string{"first", "flaky", "third"}, ran)
	assert.Equal(t, []string{"first", "flaky", "third"}, ledgerNames(t, db))
}

func TestMigrateRefusesDatabaseBelowFloor(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)

	var ran []string
	_, err := db.Migrate(ctx, Schema{
		Models: []interface{}{(*migrateParent)(nil)},
		Steps:  []Step{recordingStep("old", &ran)},
	})
	require.NoError(t, err)

	// This release deleted "never-ran", which the database never recorded
	_, err = db.Migrate(ctx, Schema{
		Models: []interface{}{(*migrateParent)(nil), (*migrateChild)(nil)},
		Steps:  []Step{recordingStep("new", &ran)},
		Floor:  "never-ran",
		Mode:   Apply,
	})
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
	assert.Empty(t, ran)
	assert.Equal(t, []string{"old"}, ledgerNames(t, db))
	assert.False(t, sqliteObjectExists(t, db, "table", "migrate_children"), "no DDL happens below the floor")
}

func TestMigrateRefusesReusedStepName(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)
	models := []interface{}{(*migrateParent)(nil)}

	var ran []string
	_, err := db.Migrate(ctx, Schema{
		Models: models,
		Steps:  []Step{recordingStep("rename-codes", &ran), recordingStep("drop-legacy", &ran)},
	})
	require.NoError(t, err)

	// Both steps were deleted and a new step reuses the older name
	_, err = db.Migrate(ctx, Schema{
		Models: models,
		Steps:  []Step{recordingStep("rename-codes", &ran)},
		Floor:  "drop-legacy",
	})
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
	assert.Empty(t, ran)
}

func TestMigrateSkipsDatabaseAheadOfRelease(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)

	var ran []string
	_, err := db.Migrate(ctx, Schema{
		Models: []interface{}{(*migrateParent)(nil)},
		Steps:  []Step{recordingStep("shared", &ran)},
	})
	require.NoError(t, err)

	// A newer release recorded a step this one does not carry
	_, err = db.NewInsert().Model(&schemaStep{Name: "from-newer-release", AppliedAt: time.Now()}).Exec(ctx)
	require.NoError(t, err)

	plan, err := db.Migrate(ctx, Schema{
		Models: []interface{}{(*migrateParent)(nil), (*migrateChild)(nil)},
		Steps:  []Step{recordingStep("shared", &ran), recordingStep("pending", &ran)},
		Mode:   Apply,
	})
	require.NoError(t, err, "a rolled-back binary must still boot")
	assert.Equal(t, Plan{}, plan)
	assert.Empty(t, ran)
	assert.Equal(t, []string{"shared", "from-newer-release"}, ledgerNames(t, db))
	assert.False(t, sqliteObjectExists(t, db, "table", "migrate_children"))
}

func TestMigrateOlderReleaseBootsDatabaseCreatedByNewerRelease(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)
	models := []interface{}{(*migrateParent)(nil)}

	// Release N+1 deleted s1, s2 and a, and creates the database
	var ran []string
	_, err := db.Migrate(ctx, Schema{Models: models, Steps: []Step{recordingStep("b", &ran)}, Floor: "a"})
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, ledgerNames(t, db))

	// Release N without a floor must not run s1 and s2 against the newer shape
	plan, err := db.Migrate(ctx, Schema{
		Models: models,
		Steps:  []Step{recordingStep("s1", &ran), recordingStep("s2", &ran), recordingStep("a", &ran), recordingStep("b", &ran)},
		Mode:   Apply,
	})
	require.NoError(t, err, "a rolled-back binary must still boot")
	assert.Equal(t, Plan{}, plan)
	assert.Empty(t, ran)

	// Release N with an older floor: the recorded live steps show the
	// database passed F0, although F0 itself was never recorded
	_, err = db.Migrate(ctx, Schema{
		Models: models,
		Steps:  []Step{recordingStep("a", &ran), recordingStep("b", &ran)},
		Floor:  "F0",
		Mode:   Apply,
	})
	require.NoError(t, err)
	assert.Empty(t, ran)
	assert.Equal(t, []string{"a", "b"}, ledgerNames(t, db))
}

func TestMigrateFloorWithoutLedgerCreatesNothing(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)

	// An existing database that never had a ledger
	_, err := db.ExecContext(ctx, "CREATE TABLE legacy (id INTEGER)")
	require.NoError(t, err)

	var ran []string
	schema := Schema{
		Models: []interface{}{(*migrateParent)(nil)},
		Steps:  []Step{recordingStep("first", &ran)},
		Floor:  "deleted",
		Mode:   Apply,
	}

	_, err = db.Migrate(ctx, schema)
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
	assert.Empty(t, ran)
	assert.False(t, sqliteObjectExists(t, db, "table", "schema_steps"), "no DDL happens before the ledger check")
	assert.False(t, sqliteObjectExists(t, db, "table", "migrate_parents"))

	// Without a floor the ledger is created and the steps run
	schema.Floor = ""
	_, err = db.Migrate(ctx, schema)
	require.NoError(t, err)
	assert.Equal(t, []string{"first"}, ran)
	assert.Equal(t, []string{"first"}, ledgerNames(t, db))
	assert.True(t, sqliteObjectExists(t, db, "table", "migrate_parents"))
}

func TestPlanRunsNoSteps(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)
	models := []interface{}{(*migrateParent)(nil)}

	_, err := db.Migrate(ctx, Schema{Models: models})
	require.NoError(t, err)

	failing := Step{
		Name: "must-not-run",
		Run: func(ctx context.Context, db bun.IDB) error {
			return errors.New("Plan ran a step")
		},
	}

	plan, err := db.Plan(ctx, Schema{Models: models, Steps: []Step{failing}})
	require.NoError(t, err)
	assert.Equal(t, Plan{}, plan)
	assert.Empty(t, ledgerNames(t, db))
}

func TestMigrateRejectsInvalidSteps(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)
	models := []interface{}{(*migrateParent)(nil)}
	run := func(ctx context.Context, db bun.IDB) error {
		return nil
	}

	for name, schema := range map[string]Schema{
		"unnamed step":     {Models: models, Steps: []Step{{Run: run}}},
		"step without Run": {Models: models, Steps: []Step{{Name: "no-run"}}},
		"qualified table":  {Models: []interface{}{(*migrateQualified)(nil)}},
	} {
		_, err := db.Migrate(ctx, schema)
		require.Error(t, err, name)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), name)
	}

	assert.False(t, sqliteObjectExists(t, db, "table", "migrate_parents"), "validation happens before any DDL")
}

func TestMigrateRejectsDuplicateNames(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)

	_, err := db.Migrate(ctx, Schema{Models: []interface{}{(*migrateParent)(nil), (*migrateClash)(nil)}})
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	var ran []string
	_, err = db.Migrate(ctx, Schema{
		Models: []interface{}{(*migrateParent)(nil)},
		Steps:  []Step{recordingStep("twice", &ran), recordingStep("twice", &ran)},
	})
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	assert.False(t, sqliteObjectExists(t, db, "table", "migrate_parents"), "validation happens before any DDL")
}

func TestMigrateRejectsConstraintsOnSQLite(t *testing.T) {
	db := newMigrateDB(t)

	_, err := db.Migrate(context.Background(), Schema{Models: []interface{}{(*migrateChecked)(nil)}})
	require.Error(t, err)
	assert.Equal(t, ez.ENOTIMPLEMENTED, ez.ErrorCode(err))
}

func TestApplyDeclared(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)
	models := []interface{}{(*migrateParent)(nil), (*migrateChild)(nil)}

	err := ApplyDeclared(ctx, db.DB, models, "no_such_index")
	require.Error(t, err)
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))

	// A table comes with its declared index
	err = ApplyDeclared(ctx, db.DB, models, "migrate_children")
	require.NoError(t, err)
	assert.True(t, sqliteObjectExists(t, db, "table", "migrate_children"))
	assert.True(t, sqliteObjectExists(t, db, "index", "migrate_children_parent_idx"))

	// An index on its own, and again without error
	err = ApplyDeclared(ctx, db.DB, models, "migrate_parents")
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, "DROP INDEX migrate_parents_code_idx")
	require.NoError(t, err)

	err = ApplyDeclared(ctx, db.DB, models, "migrate_parents_code_idx")
	require.NoError(t, err)
	assert.True(t, sqliteObjectExists(t, db, "index", "migrate_parents_code_idx"))

	err = ApplyDeclared(ctx, db.DB, models, "migrate_parents_code_idx")
	require.NoError(t, err)
}

func TestMigrateSQLiteReportAndApply(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)

	_, err := db.Migrate(ctx, Schema{Models: []interface{}{(*migrateParent)(nil)}})
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, "DROP INDEX migrate_parents_code_idx")
	require.NoError(t, err)

	schema := Schema{Models: []interface{}{(*migrateParentV2)(nil), (*migrateChild)(nil)}}
	wantMissing := []Change{
		{Kind: "column", Table: "migrate_parents", Name: "note", Want: "VARCHAR NOT NULL DEFAULT ''"},
		{Kind: "index", Table: "migrate_parents", Name: "migrate_parents_code_idx", Want: `CREATE INDEX "migrate_parents_code_idx" ON "migrate_parents" (code)`},
		{Kind: "table", Table: "migrate_children", Name: "migrate_children"},
	}

	// Report changes nothing
	plan, err := db.Migrate(ctx, schema)
	require.NoError(t, err)
	assert.Equal(t, wantMissing, plan.Missing)
	assert.Empty(t, plan.Different)
	assert.Empty(t, plan.Extra)
	assert.False(t, sqliteObjectExists(t, db, "table", "migrate_children"))
	assert.False(t, sqliteObjectExists(t, db, "index", "migrate_parents_code_idx"))

	// Plan agrees with what Migrate found
	planned, err := db.Plan(ctx, schema)
	require.NoError(t, err)
	assert.Equal(t, wantMissing, planned.Missing)

	schema.Mode = Apply
	plan, err = db.Migrate(ctx, schema)
	require.NoError(t, err)
	require.Len(t, plan.Missing, 3)
	assert.Equal(t, "add it with a step", plan.Missing[0].Refused)
	assert.Empty(t, plan.Missing[1].Refused)
	assert.Empty(t, plan.Missing[2].Refused)
	assert.True(t, sqliteObjectExists(t, db, "table", "migrate_children"))
	assert.True(t, sqliteObjectExists(t, db, "index", "migrate_children_parent_idx"))
	assert.True(t, sqliteObjectExists(t, db, "index", "migrate_parents_code_idx"))

	// Only the refused column is left
	planned, err = db.Plan(ctx, schema)
	require.NoError(t, err)
	assert.Equal(t, wantMissing[:1], planned.Missing)
}

func TestResetSchema(t *testing.T) {
	ctx := context.Background()
	db := newMigrateDB(t)

	var ran []string
	schema := Schema{
		Models: []interface{}{(*migrateParent)(nil), (*migrateChild)(nil)},
		Steps:  []Step{recordingStep("first", &ran)},
	}

	_, err := db.Migrate(ctx, schema)
	require.NoError(t, err)

	_, err = db.NewInsert().Model(&migrateParent{Code: "before reset"}).Exec(ctx)
	require.NoError(t, err)

	schema.Steps = append(schema.Steps, recordingStep("second", &ran))
	schema.Floor = "deleted"
	err = db.ResetSchema(ctx, schema)
	require.NoError(t, err)

	count, err := db.NewSelect().Model((*migrateParent)(nil)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, ran)
	assert.Equal(t, []string{"deleted", "first", "second"}, ledgerNames(t, db))
	assert.True(t, sqliteObjectExists(t, db, "index", "migrate_children_parent_idx"))
}
