package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/suite"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/vanclief/compose/drivers/databases/relational"
	"github.com/vanclief/ez"
)

// schemaTestAccount - The main table of the schema tests: one column per
// kind of default Apply must tell apart, a unique and a partial index, and a
// CHECK constraint
type schemaTestAccount struct {
	bun.BaseModel `bun:"table:schema_test_accounts"`

	ID       int64     `bun:",pk,autoincrement"`
	Email    string    `bun:",notnull"`
	Nickname string    `bun:",notnull"`
	Status   string    `bun:",notnull,default:'active'"`
	Balance  int64     `bun:",notnull,default:0"`
	Score    int64     `bun:",notnull,default:0"`
	Token    string    `bun:"type:uuid,default:gen_random_uuid()"`
	SeenAt   time.Time `bun:",default:now()"`
}

func (*schemaTestAccount) Indexes() []relational.Index {
	return []relational.Index{
		{Name: "schema_test_accounts_email_uidx", Unique: true, Def: "(email)"},
		{Name: "schema_test_accounts_status_idx", Def: "(status) WHERE status <> 'closed'"},
	}
}

func (*schemaTestAccount) Constraints() []relational.Constraint {
	return []relational.Constraint{
		{Name: "schema_test_accounts_balance_check", Def: "CHECK (balance >= 0)"},
	}
}

// schemaTestTransfer - A second table with a foreign key to the first
type schemaTestTransfer struct {
	bun.BaseModel `bun:"table:schema_test_transfers"`

	ID        int64 `bun:",pk,autoincrement"`
	AccountID int64 `bun:",notnull"`
	Amount    int64 `bun:",notnull"`
}

func (*schemaTestTransfer) Indexes() []relational.Index {
	return []relational.Index{{Name: "schema_test_transfers_account_idx", Def: "(account_id)"}}
}

func (*schemaTestTransfer) Constraints() []relational.Constraint {
	return []relational.Constraint{
		{Name: "schema_test_transfers_account_fkey", Def: "FOREIGN KEY (account_id) REFERENCES schema_test_accounts (id)"},
	}
}

func schemaTestModels() []interface{} {
	return []interface{}{(*schemaTestAccount)(nil), (*schemaTestTransfer)(nil)}
}

// schemaTestToken - A table a later release gives a column Apply refuses
type schemaTestToken struct {
	bun.BaseModel `bun:"table:schema_test_tokens"`

	ID int64 `bun:",pk,autoincrement"`
}

// schemaTestTokenV2 - schema_test_tokens with a volatile-default column and
// an index on it
type schemaTestTokenV2 struct {
	bun.BaseModel `bun:"table:schema_test_tokens"`

	ID       int64  `bun:",pk,autoincrement"`
	PublicID string `bun:"type:uuid,notnull,default:gen_random_uuid()"`
}

func (*schemaTestTokenV2) Indexes() []relational.Index {
	return []relational.Index{{Name: "schema_test_tokens_public_id_idx", Def: "(public_id)"}}
}

// schemaTestGenerated - A table with a stored generated column
type schemaTestGenerated struct {
	bun.BaseModel `bun:"table:schema_test_generated"`

	ID      int64 `bun:",pk,autoincrement"`
	Amount  int64 `bun:",notnull"`
	Doubled int64 `bun:"type:bigint GENERATED ALWAYS AS (amount * 2) STORED"`
}

// schemaTestOrphan - Declares a FOREIGN KEY to a table no model declares,
// which the temporary tables of the comparison cannot reference
type schemaTestOrphan struct {
	bun.BaseModel `bun:"table:schema_test_orphans"`

	ID       int64 `bun:",pk,autoincrement"`
	LegacyID int64 `bun:",notnull"`
}

func (*schemaTestOrphan) Constraints() []relational.Constraint {
	return []relational.Constraint{
		{Name: "schema_test_orphans_legacy_fkey", Def: "FOREIGN KEY (legacy_id) REFERENCES schema_test_legacy (id)"},
	}
}

// schemaTestEvent - A table that is partitioned in the database
type schemaTestEvent struct {
	bun.BaseModel `bun:"table:schema_test_events"`

	Day  int64  `bun:",notnull"`
	Kind string `bun:",notnull"`
}

func (*schemaTestEvent) Indexes() []relational.Index {
	return []relational.Index{{Name: "schema_test_events_kind_idx", Def: "(kind)"}}
}

// schemaTestWide - One column or declaration per shape the comparison must
// round-trip exactly
type schemaTestWide struct {
	bun.BaseModel `bun:"table:schema_test_wides"`

	ID        int64                  `bun:",pk,identity"`
	Email     string                 `bun:",notnull,unique"`
	TenantID  int64                  `bun:",notnull,unique:schema_test_wides_tenant_code_key"`
	Code      string                 `bun:"type:varchar(32),notnull,unique:schema_test_wides_tenant_code_key"`
	Amount    float64                `bun:"type:numeric(12,2),notnull,default:0"`
	Tags      []string               `bun:"type:text[],notnull,default:'{}'"`
	Data      map[string]interface{} `bun:"type:jsonb"`
	Status    string                 `bun:",notnull,default:'active'"`
	DeletedAt time.Time              `bun:",nullzero"`
	CreatedAt time.Time              `bun:",nullzero,notnull,default:current_timestamp"`
}

func (*schemaTestWide) Indexes() []relational.Index {
	return []relational.Index{
		{Name: "schema_test_wides_tags_idx", Def: "USING gin (tags)"},
		{Name: "schema_test_wides_email_lower_idx", Def: "(lower(email))"},
		{Name: "schema_test_wides_live_idx", Def: "(tenant_id, status) WHERE status <> 'closed' AND deleted_at IS NULL"},
	}
}

// SchemaSuite - Runs the schema engine against a real Postgres. Every test
// works in a schema_test schema that is the only entry of the connection's
// search_path, so it starts from an empty namespace whatever else the shared
// compose_test database holds
type SchemaSuite struct {
	suite.Suite
	db *relational.DB
}

func TestSchemaSuite(t *testing.T) {
	suite.Run(t, new(SchemaSuite))
}

// connectSchemaTest - A handle on compose_test whose search_path is only
// schema_test
func connectSchemaTest() (*relational.DB, error) {
	sqldb := sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN("postgres://postgres@localhost:5432/compose_test?sslmode=disable"),
		pgdriver.WithConnParams(map[string]interface{}{"search_path": "schema_test"}),
	))
	db := bun.NewDB(sqldb, pgdialect.New())

	_, err := db.ExecContext(context.Background(), "SELECT 1")
	if err != nil {
		db.Close() // nolint:errcheck // The connection error is the one that matters
		return nil, err
	}

	return &relational.DB{DB: db}, nil
}

func (suite *SchemaSuite) SetupTest() {
	// Skips without a local PostgreSQL unless COMPOSE_TEST_POSTGRES is set,
	// as the connection suite in postgres_test.go does
	db, err := connectSchemaTest()
	if err != nil {
		if os.Getenv("COMPOSE_TEST_POSTGRES") != "" {
			suite.T().Fatalf("PostgreSQL testing is enabled but the database is unavailable: %v", err)
		}

		suite.T().Skipf("PostgreSQL is not available: %v", err)
	}

	suite.db = db
	suite.exec("DROP SCHEMA IF EXISTS schema_test CASCADE")
	suite.exec("CREATE SCHEMA schema_test")
}

func (suite *SchemaSuite) TearDownTest() {
	if suite.db == nil {
		return
	}

	suite.exec("DROP SCHEMA IF EXISTS schema_test CASCADE")
	suite.db.Close() // nolint:errcheck // Closing a test handle
	suite.db = nil
}

func (suite *SchemaSuite) exec(query string) {
	_, err := suite.db.ExecContext(context.Background(), query)
	suite.Require().NoError(err)
}

func (suite *SchemaSuite) migrate(schema relational.Schema) relational.Plan {
	plan, err := suite.db.Migrate(context.Background(), schema)
	suite.Require().NoError(err)

	return plan
}

func (suite *SchemaSuite) plan(models []interface{}) relational.Plan {
	plan, err := suite.db.Plan(context.Background(), relational.Schema{Models: models})
	suite.Require().NoError(err)

	return plan
}

// createFresh - Creates the declared schema on the empty namespace
func (suite *SchemaSuite) createFresh(models []interface{}) {
	plan := suite.migrate(relational.Schema{Models: models})
	suite.Require().Equal(relational.Plan{}, plan)
}

func (suite *SchemaSuite) count(query string, args ...interface{}) int {
	var count int
	err := suite.db.NewRaw(query, args...).Scan(context.Background(), &count)
	suite.Require().NoError(err)

	return count
}

func (suite *SchemaSuite) tableExists(name string) bool {
	return suite.count("SELECT count(*) FROM pg_class WHERE relnamespace = 'schema_test'::regnamespace AND relkind = 'r' AND relname = ?", name) > 0
}

func (suite *SchemaSuite) validIndexExists(name string) bool {
	return suite.count(`SELECT count(*) FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
		WHERE ic.relnamespace = 'schema_test'::regnamespace AND ic.relname = ? AND i.indisvalid`, name) > 0
}

func (suite *SchemaSuite) constraintExists(name string) bool {
	return suite.count("SELECT count(*) FROM pg_constraint WHERE connamespace = 'schema_test'::regnamespace AND conname = ?", name) > 0
}

func (suite *SchemaSuite) indexExists(name string) bool {
	return suite.count("SELECT count(*) FROM pg_class WHERE relnamespace = 'schema_test'::regnamespace AND relname = ?", name) > 0
}

// beginWriter - An open transaction that has inserted into
// schema_test_accounts, so it holds a lock that waits DDL on the table
func (suite *SchemaSuite) beginWriter() bun.Tx {
	ctx := context.Background()
	writer, err := suite.db.BeginTx(ctx, nil)
	suite.Require().NoError(err)

	_, err = writer.ExecContext(ctx, "INSERT INTO schema_test_accounts (email, nickname) VALUES ('writer@example.com', 'writer')")
	suite.Require().NoError(err)

	return writer
}

// rollbackOnceRunning - Rolls tx back as soon as another session is running
// a statement that starts with prefix, or after 10 seconds. Safe to call
// from another goroutine
func (suite *SchemaSuite) rollbackOnceRunning(tx bun.Tx, prefix string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var running int
		err := suite.db.NewRaw("SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND starts_with(query, ?)", prefix).
			Scan(context.Background(), &running)
		if err != nil {
			tx.Rollback() // nolint:errcheck // The query error is the one that matters
			return err
		}

		if running > 0 {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	return tx.Rollback()
}

// changeKeys - "kind table.name" for each change, for compact assertions
func changeKeys(changes []relational.Change) []string {
	keys := make([]string, 0, len(changes))
	for _, change := range changes {
		keys = append(keys, fmt.Sprintf("%s %s.%s", change.Kind, change.Table, change.Name))
	}

	return keys
}

func (suite *SchemaSuite) TestFreshCreateThenPlanIsEmpty() {
	step := relational.Step{
		Name: "never-runs",
		Run: func(ctx context.Context, db bun.IDB) error {
			return ez.New(ez.EINTERNAL, "A fresh database must not run steps", nil)
		},
	}

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Steps: []relational.Step{step}})
	suite.Equal(relational.Plan{}, plan)

	// The round trip through the temporary-table comparison finds nothing
	suite.Equal(relational.Plan{}, suite.plan(schemaTestModels()))
	suite.Equal(1, suite.count("SELECT count(*) FROM schema_steps WHERE name = 'never-runs'"))

	// A second boot takes the existing path and finds nothing either
	plan = suite.migrate(relational.Schema{Models: schemaTestModels(), Steps: []relational.Step{step}, Mode: relational.Apply})
	suite.Equal(relational.Plan{}, plan)
}

func (suite *SchemaSuite) TestApplyAddsDroppedColumn() {
	suite.createFresh(schemaTestModels())
	suite.exec("ALTER TABLE schema_test_accounts DROP COLUMN score")

	planned := suite.plan(schemaTestModels())
	suite.Equal([]relational.Change{
		{Kind: "column", Table: "schema_test_accounts", Name: "score", Want: "bigint NOT NULL DEFAULT 0"},
	}, planned.Missing)

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Require().Len(plan.Missing, 1)
	suite.Empty(plan.Missing[0].Refused)

	var dataType, nullable, columnDefault string
	err := suite.db.NewRaw(`SELECT data_type, is_nullable, column_default FROM information_schema.columns
		WHERE table_schema = 'schema_test' AND table_name = 'schema_test_accounts' AND column_name = 'score'`).
		Scan(context.Background(), &dataType, &nullable, &columnDefault)
	suite.Require().NoError(err)
	suite.Equal("bigint", dataType)
	suite.Equal("NO", nullable)
	suite.Equal("0", columnDefault)

	suite.Equal(relational.Plan{}, suite.plan(schemaTestModels()))
}

func (suite *SchemaSuite) TestApplyRefusesNotNullWithoutDefault() {
	suite.createFresh(schemaTestModels())
	suite.exec("ALTER TABLE schema_test_accounts DROP COLUMN nickname")

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Equal([]string{"column schema_test_accounts.nickname"}, changeKeys(plan.Missing))
	suite.Contains(plan.Missing[0].Refused, "older binary's inserts would fail")

	suite.Equal([]string{"column schema_test_accounts.nickname"}, changeKeys(suite.plan(schemaTestModels()).Missing))
}

func (suite *SchemaSuite) TestApplyRefusesVolatileDefaultOnly() {
	suite.createFresh(schemaTestModels())
	suite.exec("ALTER TABLE schema_test_accounts DROP COLUMN token, DROP COLUMN seen_at")

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Equal([]string{"column schema_test_accounts.seen_at", "column schema_test_accounts.token"}, changeKeys(plan.Missing))
	suite.Empty(plan.Missing[0].Refused, "now() is stable, so Postgres adds the column without a rewrite")
	suite.Contains(plan.Missing[1].Refused, "volatile")

	suite.Equal([]string{"column schema_test_accounts.token"}, changeKeys(suite.plan(schemaTestModels()).Missing))
}

func (suite *SchemaSuite) TestAlteredDefaultIsDifferent() {
	suite.createFresh(schemaTestModels())
	suite.exec("ALTER TABLE schema_test_accounts ALTER COLUMN status SET DEFAULT 'pending'")

	plan, err := suite.db.Migrate(context.Background(), relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Require().Error(err)
	suite.Equal(ez.ECONFLICT, ez.ErrorCode(err))
	suite.Equal([]string{"column schema_test_accounts.status"}, changeKeys(plan.Different))

	plan = suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Report})
	suite.Equal([]relational.Change{{
		Kind:  "column",
		Table: "schema_test_accounts",
		Name:  "status",
		Want:  "character varying NOT NULL DEFAULT 'active'::character varying",
		Have:  "character varying NOT NULL DEFAULT 'pending'::character varying",
	}}, plan.Different)
	suite.Empty(plan.Missing)
}

func (suite *SchemaSuite) TestUndeclaredColumnAndIndexAreExtra() {
	suite.createFresh(schemaTestModels())
	suite.exec("ALTER TABLE schema_test_accounts ADD COLUMN legacy text")
	suite.exec("CREATE INDEX schema_test_accounts_legacy_idx ON schema_test_accounts (legacy)")

	planned := suite.plan(schemaTestModels())
	suite.Empty(planned.Missing)
	suite.Empty(planned.Different)
	suite.Equal([]string{"column schema_test_accounts.legacy", "index schema_test_accounts.schema_test_accounts_legacy_idx"}, changeKeys(planned.Extra))

	// Extras do not block Apply, and Apply drops nothing
	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Equal(planned.Extra, plan.Extra)
	suite.True(suite.validIndexExists("schema_test_accounts_legacy_idx"))
}

func (suite *SchemaSuite) TestApplyBuildsMissingPartialIndex() {
	suite.createFresh(schemaTestModels())
	suite.exec("DROP INDEX schema_test_accounts_status_idx")

	suite.Equal([]string{"index schema_test_accounts.schema_test_accounts_status_idx"}, changeKeys(suite.plan(schemaTestModels()).Missing))

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Require().Len(plan.Missing, 1)
	suite.Empty(plan.Missing[0].Refused)
	suite.True(suite.validIndexExists("schema_test_accounts_status_idx"))

	suite.Equal(relational.Plan{}, suite.plan(schemaTestModels()))
}

func (suite *SchemaSuite) TestApplyReplacesInvalidIndexLeftover() {
	suite.createFresh(schemaTestModels())
	suite.exec("DROP INDEX schema_test_accounts_status_idx")
	suite.exec("INSERT INTO schema_test_accounts (email, nickname) VALUES ('a@example.com', 'a'), ('b@example.com', 'b')")

	// A concurrent build that fails leaves an invalid index behind
	_, err := suite.db.ExecContext(context.Background(), "CREATE UNIQUE INDEX CONCURRENTLY schema_test_accounts_status_idx ON schema_test_accounts (status)")
	suite.Require().Error(err)
	suite.Equal(1, suite.count("SELECT count(*) FROM pg_class WHERE relnamespace = 'schema_test'::regnamespace AND relname = 'schema_test_accounts_status_idx'"))
	suite.False(suite.validIndexExists("schema_test_accounts_status_idx"))

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Equal([]string{"index schema_test_accounts.schema_test_accounts_status_idx"}, changeKeys(plan.Missing))
	suite.Empty(plan.Missing[0].Refused)
	suite.True(suite.validIndexExists("schema_test_accounts_status_idx"))

	suite.Equal(relational.Plan{}, suite.plan(schemaTestModels()))
}

func (suite *SchemaSuite) TestUniqueIndexIsRefusedThenInstalledByStep() {
	suite.createFresh(schemaTestModels())
	suite.exec("DROP INDEX schema_test_accounts_email_uidx")

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Equal([]string{"index schema_test_accounts.schema_test_accounts_email_uidx"}, changeKeys(plan.Missing))
	suite.Contains(plan.Missing[0].Refused, "ApplyDeclared")
	suite.False(suite.validIndexExists("schema_test_accounts_email_uidx"))

	step := relational.Step{
		Name: "install-email-uidx",
		Run: func(ctx context.Context, db bun.IDB) error {
			return relational.ApplyDeclared(ctx, db, schemaTestModels(), "schema_test_accounts_email_uidx")
		},
	}

	plan = suite.migrate(relational.Schema{Models: schemaTestModels(), Steps: []relational.Step{step}, Mode: relational.Apply})
	suite.Equal(relational.Plan{}, plan)
	suite.True(suite.validIndexExists("schema_test_accounts_email_uidx"))
}

func (suite *SchemaSuite) TestConstraintIsRefusedThenInstalledByStep() {
	suite.createFresh(schemaTestModels())
	suite.exec("ALTER TABLE schema_test_accounts DROP CONSTRAINT schema_test_accounts_balance_check")

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Equal([]relational.Change{{
		Kind:    "constraint",
		Table:   "schema_test_accounts",
		Name:    "schema_test_accounts_balance_check",
		Want:    "CHECK ((balance >= 0))",
		Refused: "install it with a step calling ApplyDeclared once the data satisfies it",
	}}, plan.Missing)
	suite.False(suite.constraintExists("schema_test_accounts_balance_check"))

	step := relational.Step{
		Name: "install-balance-check",
		Run: func(ctx context.Context, db bun.IDB) error {
			return relational.ApplyDeclared(ctx, db, schemaTestModels(), "schema_test_accounts_balance_check")
		},
	}

	plan = suite.migrate(relational.Schema{Models: schemaTestModels(), Steps: []relational.Step{step}, Mode: relational.Apply})
	suite.Equal(relational.Plan{}, plan)
	suite.True(suite.constraintExists("schema_test_accounts_balance_check"))
}

func (suite *SchemaSuite) TestRedefinedIndexIsDifferent() {
	suite.createFresh(schemaTestModels())
	suite.exec("DROP INDEX schema_test_accounts_status_idx")
	suite.exec("CREATE INDEX schema_test_accounts_status_idx ON schema_test_accounts (status)")

	planned := suite.plan(schemaTestModels())
	suite.Empty(planned.Missing)
	suite.Equal([]relational.Change{{
		Kind:  "index",
		Table: "schema_test_accounts",
		Name:  "schema_test_accounts_status_idx",
		Want:  "CREATE INDEX schema_test_accounts_status_idx ON schema_test_accounts USING btree (status) WHERE ((status)::text <> 'closed'::text)",
		Have:  "CREATE INDEX schema_test_accounts_status_idx ON schema_test_accounts USING btree (status)",
	}}, planned.Different)
}

func (suite *SchemaSuite) TestMissingTableIsReportedThenCreated() {
	suite.createFresh([]interface{}{(*schemaTestAccount)(nil)})

	schema := relational.Schema{Models: schemaTestModels()}
	plan := suite.migrate(schema)
	suite.Equal([]relational.Change{{Kind: "table", Table: "schema_test_transfers", Name: "schema_test_transfers"}}, plan.Missing)
	suite.False(suite.tableExists("schema_test_transfers"), "Report creates nothing")

	schema.Mode = relational.Apply
	plan = suite.migrate(schema)
	suite.Require().Len(plan.Missing, 1)
	suite.Empty(plan.Missing[0].Refused)
	suite.True(suite.tableExists("schema_test_transfers"))
	suite.True(suite.validIndexExists("schema_test_transfers_account_idx"))
	suite.True(suite.constraintExists("schema_test_transfers_account_fkey"))

	suite.Equal(relational.Plan{}, suite.plan(schemaTestModels()))
}

func (suite *SchemaSuite) TestConcurrentMigrateRunsStepsOnce() {
	ctx := context.Background()
	suite.createFresh(schemaTestModels())

	// The lock holder also builds an index CONCURRENTLY while the other
	// process waits for the lock, which must not deadlock
	suite.exec("DROP INDEX schema_test_accounts_status_idx")

	step := relational.Step{
		Name: "seed-once",
		Run: func(ctx context.Context, db bun.IDB) error {
			_, err := db.ExecContext(ctx, "SELECT pg_sleep(0.5)")
			if err != nil {
				return err
			}

			_, err = db.ExecContext(ctx, "INSERT INTO schema_test_accounts (email, nickname) VALUES ('once@example.com', 'once')")
			return err
		},
	}
	schema := relational.Schema{Models: schemaTestModels(), Steps: []relational.Step{step}, Mode: relational.Apply}

	other, err := connectSchemaTest()
	suite.Require().NoError(err)
	defer other.Close() // nolint:errcheck // Closing a test handle

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, db := range []*relational.DB{suite.db, other} {
		wg.Go(func() {
			_, errs[i] = db.Migrate(ctx, schema)
		})
	}
	wg.Wait()

	suite.NoError(errs[0])
	suite.NoError(errs[1])
	suite.Equal(1, suite.count("SELECT count(*) FROM schema_test_accounts"))
	suite.Equal(1, suite.count("SELECT count(*) FROM schema_steps WHERE name = 'seed-once'"))
	suite.True(suite.validIndexExists("schema_test_accounts_status_idx"))
}

func (suite *SchemaSuite) TestExecShortSetsTimeouts() {
	suite.createFresh(schemaTestModels())

	var lockTimeoutAfter string
	step := relational.Step{
		Name: "short-ddl",
		Run: func(ctx context.Context, db bun.IDB) error {
			err := relational.ExecShort(ctx, db, `CREATE TABLE schema_test_settings AS
				SELECT current_setting('lock_timeout') AS lock_timeout, current_setting('statement_timeout') AS statement_timeout`)
			if err != nil {
				return err
			}

			return db.NewRaw("SELECT current_setting('lock_timeout')").Scan(ctx, &lockTimeoutAfter)
		},
	}

	suite.migrate(relational.Schema{Models: schemaTestModels(), Steps: []relational.Step{step}})

	var lockTimeout, statementTimeout string
	err := suite.db.NewRaw("SELECT lock_timeout, statement_timeout FROM schema_test_settings").
		Scan(context.Background(), &lockTimeout, &statementTimeout)
	suite.Require().NoError(err)
	suite.Equal("2s", lockTimeout)
	suite.Equal("10s", statementTimeout)
	suite.Equal("0", lockTimeoutAfter, "the timeouts are local to the statement's transaction")
}

func (suite *SchemaSuite) TestRefusedColumnRefusesIndexOnItsTable() {
	suite.createFresh([]interface{}{(*schemaTestToken)(nil)})

	plan := suite.migrate(relational.Schema{Models: []interface{}{(*schemaTestTokenV2)(nil)}, Mode: relational.Apply})
	suite.Equal([]string{"column schema_test_tokens.public_id", "index schema_test_tokens.schema_test_tokens_public_id_idx"}, changeKeys(plan.Missing))
	suite.Contains(plan.Missing[0].Refused, "volatile")
	suite.Equal("depends on a column Apply did not create", plan.Missing[1].Refused)
	suite.False(suite.indexExists("schema_test_tokens_public_id_idx"))
}

func (suite *SchemaSuite) TestSlowIndexBuildIsRefused() {
	suite.createFresh(schemaTestModels())
	suite.exec("DROP INDEX schema_test_accounts_status_idx")

	timeout := relational.IndexBuildLockTimeout
	relational.IndexBuildLockTimeout = time.Second
	defer func() {
		relational.IndexBuildLockTimeout = timeout
	}()

	// The concurrent build waits for the open writer until it times out.
	// Dropping the leftover it cancels with waits for the writer too, so
	// the writer ends once that drop runs
	writer := suite.beginWriter()
	released := make(chan error, 1)
	go func() {
		released <- suite.rollbackOnceRunning(writer, "DROP INDEX CONCURRENTLY")
	}()

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Require().NoError(<-released)
	suite.Equal([]string{"index schema_test_accounts.schema_test_accounts_status_idx"}, changeKeys(plan.Missing))
	suite.Equal("the build waited over 1s for other transactions, build it by hand", plan.Missing[0].Refused)
	suite.False(suite.indexExists("schema_test_accounts_status_idx"), "no invalid leftover remains")
}

func (suite *SchemaSuite) TestApplyDeclaredReplacesInvalidLeftover() {
	ctx := context.Background()
	suite.createFresh(schemaTestModels())
	suite.exec("DROP INDEX schema_test_accounts_email_uidx")
	suite.exec("INSERT INTO schema_test_accounts (email, nickname) VALUES ('dup@example.com', 'a'), ('dup@example.com', 'b')")

	// The statement Apply prints fails on the duplicates and leaves an invalid
	// index that IF NOT EXISTS would match by name
	_, err := suite.db.ExecContext(ctx, "CREATE UNIQUE INDEX CONCURRENTLY schema_test_accounts_email_uidx ON schema_test_accounts (email)")
	suite.Require().Error(err)
	suite.True(suite.indexExists("schema_test_accounts_email_uidx"))
	suite.False(suite.validIndexExists("schema_test_accounts_email_uidx"))

	step := relational.Step{
		Name: "dedup-emails",
		Run: func(ctx context.Context, db bun.IDB) error {
			return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
				_, err := tx.ExecContext(ctx, "DELETE FROM schema_test_accounts a USING schema_test_accounts b WHERE a.email = b.email AND a.id > b.id")
				if err != nil {
					return err
				}

				return relational.ApplyDeclared(ctx, tx, schemaTestModels(), "schema_test_accounts_email_uidx")
			})
		},
	}

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Steps: []relational.Step{step}, Mode: relational.Apply})
	suite.Equal(relational.Plan{}, plan)
	suite.True(suite.validIndexExists("schema_test_accounts_email_uidx"))

	_, err = suite.db.ExecContext(ctx, "INSERT INTO schema_test_accounts (email, nickname) VALUES ('dup@example.com', 'c')")
	suite.Require().Error(err, "the unique index is enforced")
}

func (suite *SchemaSuite) TestNewTableGivesUpOnBusyReferencedTable() {
	suite.createFresh([]interface{}{(*schemaTestAccount)(nil)})

	// The new table's FOREIGN KEY needs a lock on schema_test_accounts that
	// conflicts with the open writer's
	writer := suite.beginWriter()
	defer writer.Rollback() // nolint:errcheck // Ending the writer

	// Without lock_timeout the boot would wait for the writer indefinitely
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := suite.db.Migrate(ctx, relational.Schema{Models: schemaTestModels(), Mode: relational.Apply})
	suite.Require().Error(err)
	suite.Contains(err.Error(), "lock timeout")
	suite.False(suite.tableExists("schema_test_transfers"))
}

func (suite *SchemaSuite) TestGeneratedColumnRoundTripsAndIsRefused() {
	models := []interface{}{(*schemaTestGenerated)(nil)}
	suite.createFresh(models)
	suite.Equal(relational.Plan{}, suite.plan(models))

	suite.exec("ALTER TABLE schema_test_generated DROP COLUMN doubled")
	plan := suite.migrate(relational.Schema{Models: models, Mode: relational.Apply})
	suite.Equal([]relational.Change{{
		Kind:    "column",
		Table:   "schema_test_generated",
		Name:    "doubled",
		Want:    "bigint GENERATED ALWAYS AS ((amount * 2)) STORED",
		Refused: "generated column, add it with a step",
	}}, plan.Missing)
}

func (suite *SchemaSuite) TestUnbuildableDeclarationIsInvalid() {
	ctx := context.Background()
	suite.createFresh([]interface{}{(*schemaTestAccount)(nil)})
	suite.exec("CREATE TABLE schema_test_legacy (id bigint PRIMARY KEY)")
	models := []interface{}{(*schemaTestAccount)(nil), (*schemaTestOrphan)(nil)}

	_, err := suite.db.Plan(ctx, relational.Schema{Models: models})
	suite.Require().Error(err)
	suite.Equal(ez.EINVALID, ez.ErrorCode(err))
	suite.Contains(err.Error(), "schema_test_orphans_legacy_fkey")
	suite.Contains(err.Error(), "constraints on temporary tables may reference only temporary tables")

	_, err = suite.db.Migrate(ctx, relational.Schema{Models: models, Mode: relational.Apply})
	suite.Require().Error(err)
	suite.Equal(ez.EINVALID, ez.ErrorCode(err))

	// Report logs the error and boots without a plan
	suite.Equal(relational.Plan{}, suite.migrate(relational.Schema{Models: models}))
}

func (suite *SchemaSuite) TestPartitionedTableIndex() {
	suite.createFresh([]interface{}{(*schemaTestAccount)(nil)})
	suite.exec("CREATE TABLE schema_test_events (day bigint NOT NULL, kind varchar NOT NULL) PARTITION BY RANGE (day)")
	models := []interface{}{(*schemaTestAccount)(nil), (*schemaTestEvent)(nil)}

	plan := suite.migrate(relational.Schema{Models: models, Mode: relational.Apply})
	suite.Equal([]string{"index schema_test_events.schema_test_events_kind_idx"}, changeKeys(plan.Missing))
	suite.Contains(plan.Missing[0].Refused, "partitioned")
	suite.Empty(plan.Different)

	// Built by hand it matches the declaration, although pg_get_indexdef
	// prints ON ONLY for it
	suite.exec("CREATE INDEX schema_test_events_kind_idx ON schema_test_events (kind)")
	suite.Equal(relational.Plan{}, suite.plan(models))
}

func (suite *SchemaSuite) TestTagConstraintIsRefusedWithoutApplyDeclared() {
	models := []interface{}{(*schemaTestWide)(nil)}
	suite.createFresh(models)
	suite.exec("ALTER TABLE schema_test_wides DROP CONSTRAINT schema_test_wides_tenant_code_key")

	plan := suite.migrate(relational.Schema{Models: models, Mode: relational.Apply})
	suite.Equal([]string{"constraint schema_test_wides.schema_test_wides_tenant_code_key"}, changeKeys(plan.Missing))
	suite.Equal("add it with a step", plan.Missing[0].Refused)
}

func (suite *SchemaSuite) TestMaxIndexTableSizeRefusesLargerTable() {
	suite.createFresh(schemaTestModels())
	suite.exec("DROP INDEX schema_test_accounts_status_idx")
	suite.exec("INSERT INTO schema_test_accounts (email, nickname) VALUES ('a@example.com', 'a')")

	plan := suite.migrate(relational.Schema{Models: schemaTestModels(), Mode: relational.Apply, MaxIndexTableSize: 1})
	suite.Equal([]string{"index schema_test_accounts.schema_test_accounts_status_idx"}, changeKeys(plan.Missing))
	suite.Contains(plan.Missing[0].Refused, "over the 1 byte limit")
	suite.False(suite.indexExists("schema_test_accounts_status_idx"))
}

func (suite *SchemaSuite) TestWideModelRoundTrips() {
	models := []interface{}{(*schemaTestWide)(nil)}
	suite.createFresh(models)
	suite.Equal(relational.Plan{}, suite.plan(models))

	// ALWAYS and BY DEFAULT identities compare different
	suite.exec("ALTER TABLE schema_test_wides ALTER COLUMN id SET GENERATED ALWAYS")
	suite.Equal([]relational.Change{{
		Kind:  "column",
		Table: "schema_test_wides",
		Name:  "id",
		Want:  "bigint NOT NULL GENERATED BY DEFAULT AS IDENTITY",
		Have:  "bigint NOT NULL GENERATED ALWAYS AS IDENTITY",
	}}, suite.plan(models).Different)
}
