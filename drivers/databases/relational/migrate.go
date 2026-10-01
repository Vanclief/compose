package relational

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/vanclief/ez"
)

// schemaStep - A row of the ledger of applied steps
type schemaStep struct {
	bun.BaseModel `bun:"table:schema_steps"`

	ID        int64     `bun:",pk,autoincrement"`
	Name      string    `bun:",notnull,unique"`
	AppliedAt time.Time `bun:",notnull"`
}

// ledgerTable - The ledger's table, which the comparison leaves out
const ledgerTable = "schema_steps"

// Migrate - Brings the database to the declared schema on one pinned
// connection, holding a lock on Postgres so concurrent boots take turns. An
// empty database is created at the declared shape in one transaction, with
// the floor and every step recorded as applied. An existing database has its
// ledger checked before any DDL, runs its pending steps, is compared with the
// declarations and, in Apply mode, gets the safe subset of what is missing
func (db *DB) Migrate(ctx context.Context, schema Schema) (Plan, error) {
	tables, err := validateSchema(db.DB, schema)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}
	defer conn.Close() // nolint:errcheck // Closing only returns the connection to the pool

	err = lockSchema(ctx, conn)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}
	defer unlockSchema(ctx, conn)

	fresh, err := isFresh(ctx, conn)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}

	if fresh {
		err = createExtensions(ctx, conn, schema.Extensions)
		if err != nil {
			return Plan{}, ez.Wrap(err)
		}

		err = createFresh(ctx, conn, schema, tables)
		if err != nil {
			return Plan{}, ez.Wrap(err)
		}

		log.Info().Int("Tables", len(tables)).Msg("Created the declared schema")
		return Plan{}, nil
	}

	pending, ahead, err := checkLedger(ctx, conn, schema)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}

	if ahead {
		return Plan{}, nil
	}

	err = createExtensions(ctx, conn, schema.Extensions)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}

	err = runPending(ctx, conn, pending)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}

	plan, desired, err := compare(ctx, conn, tables)
	if err != nil {
		// A declaration the comparison cannot build is a bug to fix, but not
		// one worth refusing to serve over when nothing would be applied
		if schema.Mode == Report && ez.ErrorCode(err) == ez.EINVALID {
			log.Error().Err(err).Msg("Failed to compare the declared schema with the database")
			return Plan{}, nil
		}

		return Plan{}, ez.Wrap(err)
	}

	if schema.Mode == Report {
		logPlan(plan)
		return plan, nil
	}

	if len(plan.Different) > 0 {
		return plan, ez.New(ez.ECONFLICT, differentMessage(plan.Different), nil)
	}

	for _, change := range plan.Extra {
		changeEvent(log.Info(), change).Msg("In the database but not declared")
	}

	if conn.Dialect().Name() == dialect.SQLite {
		plan, err = applySQLite(ctx, conn, tables, plan)
	} else {
		plan, err = applyPostgres(ctx, conn, schema, tables, desired, plan)
	}
	if err != nil {
		return plan, ez.Wrap(err)
	}

	return plan, nil
}

// Plan - Compares the declarations with the database, holding the same lock
// as Migrate. It runs no steps and changes nothing
func (db *DB) Plan(ctx context.Context, schema Schema) (Plan, error) {
	tables, err := validateSchema(db.DB, schema)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}
	defer conn.Close() // nolint:errcheck // Closing only returns the connection to the pool

	err = lockSchema(ctx, conn)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}
	defer unlockSchema(ctx, conn)

	plan, _, err := compare(ctx, conn, tables)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}

	return plan, nil
}

// validateSchema - Checks the dialect is supported and the schema is well
// formed, returning its declared tables
func validateSchema(db bun.IDB, schema Schema) ([]declaredTable, error) {
	name := db.Dialect().Name()
	if name != dialect.PG && name != dialect.SQLite {
		return nil, ez.New(ez.ENOTIMPLEMENTED, "Schema migration is only supported by PostgreSQL and SQLite", nil)
	}

	if schema.Mode != Report && schema.Mode != Apply {
		return nil, ez.New(ez.EINVALID, "Schema mode must be Report or Apply", nil)
	}

	tables, err := declare(db, schema.Models)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	names := map[string]bool{}
	for _, floor := range schema.Floors {
		if floor == "" {
			return nil, ez.New(ez.EINVALID, "Schema floors must have a name", nil)
		}

		if names[floor] {
			msg := fmt.Sprintf("Schema floor %q appears twice", floor)
			return nil, ez.New(ez.EINVALID, msg, nil)
		}
		names[floor] = true
	}

	for i, step := range schema.Steps {
		if step.Name == "" {
			msg := fmt.Sprintf("Schema step %d has no name", i)
			return nil, ez.New(ez.EINVALID, msg, nil)
		}

		if step.Run == nil {
			msg := fmt.Sprintf("Schema step %q has no Run function", step.Name)
			return nil, ez.New(ez.EINVALID, msg, nil)
		}

		if names[step.Name] {
			msg := fmt.Sprintf("Schema step %q appears twice or is a floor, step names must be unique", step.Name)
			return nil, ez.New(ez.EINVALID, msg, nil)
		}
		names[step.Name] = true
	}

	return tables, nil
}

// isFresh - Whether the database holds no tables at all
func isFresh(ctx context.Context, conn bun.Conn) (bool, error) {
	query := postgresTableCount
	if conn.Dialect().Name() == dialect.SQLite {
		query = sqliteTableCount
	}

	var count int
	err := conn.NewRaw(query).Scan(ctx, &count)
	if err != nil {
		return false, ez.Wrap(err)
	}

	return count == 0, nil
}

// createFresh - Creates every declared table, index and constraint and the
// ledger in one transaction, recording the floors and then every step as
// applied. Steps do not run: the declarations already describe their result
func createFresh(ctx context.Context, conn bun.Conn, schema Schema, tables []declaredTable) error {
	err := conn.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		err := createTables(ctx, tx, tables, "")
		if err != nil {
			return err
		}

		_, err = tx.NewCreateTable().Model((*schemaStep)(nil)).Exec(ctx)
		if err != nil {
			return err
		}

		for _, floor := range schema.Floors {
			err = recordStep(ctx, tx, floor)
			if err != nil {
				return err
			}
		}

		for _, step := range schema.Steps {
			err = recordStep(ctx, tx, step.Name)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// recordStep - Adds a step to the ledger
func recordStep(ctx context.Context, db bun.IDB, name string) error {
	_, err := db.NewInsert().Model(&schemaStep{Name: name, AppliedAt: time.Now()}).Exec(ctx)
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// ledgerExists - Whether the ledger table exists, found without creating it
func ledgerExists(ctx context.Context, conn bun.Conn) (bool, error) {
	query := postgresTableExists
	if conn.Dialect().Name() == dialect.SQLite {
		query = sqliteTableExists
	}

	var count int
	err := conn.NewRaw(query, ledgerTable).Scan(ctx, &count)
	if err != nil {
		return false, ez.Wrap(err)
	}

	return count > 0, nil
}

// checkLedger - Reads the ledger and checks the floors and step names against
// it before any DDL, then returns the pending steps in order. It returns
// ahead true, with nothing to run, when a newer release has migrated the
// database: a rolled-back binary must still boot. A missing ledger means no
// step ever ran, so a set floor is refused and otherwise the ledger is
// created once the checks pass
func checkLedger(ctx context.Context, conn bun.Conn, schema Schema) ([]Step, bool, error) {
	exists, err := ledgerExists(ctx, conn)
	if err != nil {
		return nil, false, ez.Wrap(err)
	}

	var rows []schemaStep
	if exists {
		err = conn.NewSelect().Model(&rows).Scan(ctx)
		if err != nil {
			return nil, false, ez.Wrap(err)
		}
	}

	applied := make(map[string]int64, len(rows))
	for _, row := range rows {
		applied[row.Name] = row.ID
	}

	// The newest recorded floor. Steps are deleted as a prefix, so a recorded
	// floor or live step shows the database passed every deletion this
	// release made. A newer release that created the database recorded its
	// own floors, which include this release's unless they were dropped since
	var floorID int64
	var floorName string
	if len(schema.Floors) > 0 {
		for _, floor := range schema.Floors {
			id, ok := applied[floor]
			if ok && id > floorID {
				floorID = id
				floorName = floor
			}
		}

		passed := slices.ContainsFunc(schema.Steps, func(step Step) bool {
			_, ok := applied[step.Name]
			return ok
		})

		if floorName == "" && !passed {
			newest := schema.Floors[len(schema.Floors)-1]
			msg := fmt.Sprintf("The database never ran schema step %q, which this release no longer carries. Deploy an older release that still has it, or rebuild the database", newest)
			return nil, false, ez.New(ez.ECONFLICT, msg, nil)
		}
	}

	// known is the newest ledger row this release can account for
	known := floorID
	live := make(map[string]bool, len(schema.Steps))
	for _, step := range schema.Steps {
		live[step.Name] = true

		id, ok := applied[step.Name]
		if !ok {
			continue
		}

		if id < floorID {
			msg := fmt.Sprintf("Schema step %q reuses the name of a step applied before the floor %q, rename it", step.Name, floorName)
			return nil, false, ez.New(ez.EINVALID, msg, nil)
		}

		known = max(known, id)
	}

	var unknown []string
	for _, row := range rows {
		if row.ID > known && !live[row.Name] {
			unknown = append(unknown, row.Name)
		}
	}

	if len(unknown) > 0 && len(schema.Floors) == 0 {
		log.Error().
			Strs("Steps", unknown).
			Msg("The database records schema steps this release does not carry and Floors is empty, so it is treated as migrated by a newer release: no schema steps run and nothing is created. If steps were deleted, append the newest deleted one to Floors")
		return nil, true, nil
	}

	if len(unknown) > 0 {
		log.Warn().
			Strs("Steps", unknown).
			Msg("The database was migrated by a newer release, running no schema steps or changes")
		return nil, true, nil
	}

	// Steps are recorded in slice order, so a pending step above a recorded
	// one means a newer release created the database with a later floor
	var pending []Step
	for _, step := range schema.Steps {
		_, ok := applied[step.Name]
		if !ok {
			pending = append(pending, step)
			continue
		}

		if len(pending) > 0 {
			log.Warn().
				Str("Pending", pending[0].Name).
				Str("Recorded", step.Name).
				Msg("A schema step is pending above a recorded one: a newer release created the database, or a step was inserted instead of appended. Running no schema steps or changes")
			return nil, true, nil
		}
	}

	if !exists {
		_, err = conn.NewCreateTable().Model((*schemaStep)(nil)).Exec(ctx)
		if err != nil {
			return nil, false, ez.Wrap(err)
		}
	}

	return pending, false, nil
}

// runPending - Runs the pending steps in order on the pinned connection,
// recording each after it succeeds. On the first failure the failed step
// stays pending
func runPending(ctx context.Context, conn bun.Conn, steps []Step) error {
	for _, step := range steps {
		err := step.Run(ctx, conn)
		if err != nil {
			log.Error().Str("Name", step.Name).Err(err).Msg("Failed to apply schema step")
			return ez.Wrap(err)
		}

		err = recordStep(ctx, conn, step.Name)
		if err != nil {
			return ez.Wrap(err)
		}

		log.Warn().Str("Name", step.Name).Msg("Applied schema step")
	}

	return nil
}

// compare - The plan for the database. On Postgres it also returns the
// desired catalog, which Apply renders missing columns from
func compare(ctx context.Context, conn bun.Conn, tables []declaredTable) (Plan, Catalog, error) {
	if conn.Dialect().Name() == dialect.SQLite {
		plan, err := sqliteMissing(ctx, conn, tables)
		if err != nil {
			return Plan{}, nil, ez.Wrap(err)
		}

		return plan, nil, nil
	}

	plan, desired, err := comparePostgres(ctx, conn, tables)
	if err != nil {
		return Plan{}, nil, ez.Wrap(err)
	}

	return plan, desired, nil
}

// missingTables - The declared tables the plan reports missing, in model
// order
func missingTables(tables []declaredTable, plan Plan) []declaredTable {
	missing := map[string]bool{}
	for _, change := range plan.Missing {
		if change.Kind == "table" {
			missing[change.Name] = true
		}
	}

	var result []declaredTable
	for _, table := range tables {
		if missing[table.Name] {
			result = append(result, table)
		}
	}

	return result
}

// logPlan - Logs a plan in Report mode
func logPlan(plan Plan) {
	for _, change := range plan.Missing {
		changeEvent(log.Warn(), change).Msg("Declared but missing")
	}

	for _, change := range plan.Different {
		changeEvent(log.Warn(), change).Msg("Declared with another definition")
	}

	for _, change := range plan.Extra {
		changeEvent(log.Info(), change).Msg("In the database but not declared")
	}
}

// changeEvent - Adds a change's fields to a log event
func changeEvent(event *zerolog.Event, change Change) *zerolog.Event {
	return event.
		Str("Kind", change.Kind).
		Str("Table", change.Table).
		Str("Name", change.Name).
		Str("Want", change.Want).
		Str("Have", change.Have)
}

// Why Apply refuses a missing index on an existing table, on either dialect
const (
	refusalLackingColumn = "depends on a column Apply did not create"
	refusalUniqueIndex   = "can reject the older binary's writes or fail on duplicates, install it with a step calling ApplyDeclared"
)

// refusalOwnedElsewhere - Why Apply refuses an index whose name another table
// already uses
func refusalOwnedElsewhere(name, table string) string {
	return fmt.Sprintf("an index named %q already exists on table %q, rename the declaration", name, table)
}

// refuse - Records and logs why Apply did not create a missing object, with
// the statement an operator could run instead
func refuse(change Change, reason, statement string) Change {
	change.Refused = reason
	changeEvent(log.Warn(), change).
		Str("Reason", reason).
		Str("Statement", statement).
		Msg("Declared but not created")

	return change
}

// differentMessage - The ECONFLICT message for Apply when definitions differ
func differentMessage(changes []Change) string {
	parts := make([]string, 0, len(changes))
	for _, change := range changes {
		parts = append(parts, fmt.Sprintf("%s %s.%s is %q but is declared as %q", change.Kind, change.Table, change.Name, change.Have, change.Want))
	}

	return "The database differs from the declared schema: " + strings.Join(parts, ", ") +
		". Fix the declaration or add a step, or boot in Report mode to proceed anyway"
}
