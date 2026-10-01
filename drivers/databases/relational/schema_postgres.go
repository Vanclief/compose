package relational

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/vanclief/ez"
)

// schemaLockKey - The session advisory lock Migrate, Plan and ResetSchema
// hold on Postgres: the ASCII bytes of "compose"
const schemaLockKey int64 = 0x636f6d706f7365

// schemaLockPoll - How often a waiting boot retries the schema lock
const schemaLockPoll = 500 * time.Millisecond

// applicationTables - Filters pg_class c to application tables: ordinary or
// partitioned, and not owned by an extension
const applicationTables = `c.relkind IN ('r', 'p')
	AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')`

// postgresTableCount - Application tables in the current schema
const postgresTableCount = `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = current_schema() AND ` + applicationTables

// postgresTableExists - Whether the current schema has a table by name
const postgresTableExists = `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND c.relname = ?`

// IndexBuildLockTimeout - How long a concurrent index build Apply runs may
// wait for another transaction, such as one of the older binary's, before the
// build is canceled, its leftover dropped and the index refused. It is a
// lock_timeout because pgdriver closes the connection, and with it the schema
// lock, on the error statement_timeout raises. The connection's own timeouts
// (30s by default in compose) still cap the whole build. A variable so tests
// can shorten it
var IndexBuildLockTimeout = 20 * time.Second

// lockSchema - Takes the schema lock on Postgres, a no-op elsewhere. It polls
// pg_try_advisory_lock instead of blocking in pg_advisory_lock: a session
// blocked in that statement holds a snapshot, and CREATE INDEX CONCURRENTLY
// in the lock holder waits for older snapshots, which Postgres resolves by
// failing one side with "deadlock detected"
func lockSchema(ctx context.Context, conn bun.Conn) error {
	if conn.Dialect().Name() != dialect.PG {
		return nil
	}

	waiting := false
	for {
		var locked bool
		err := conn.NewRaw("SELECT pg_try_advisory_lock(?)", schemaLockKey).Scan(ctx, &locked)
		if err != nil {
			return ez.Wrap(err)
		}

		if locked {
			return nil
		}

		if !waiting {
			log.Info().Msg("Waiting for another process to finish migrating the schema")
			waiting = true
		}

		select {
		case <-ctx.Done():
			return ez.Wrap(ctx.Err())
		case <-time.After(schemaLockPoll):
		}
	}
}

// unlockSchema - Releases the schema lock on Postgres, even when ctx is done,
// so the pooled connection does not keep it
func unlockSchema(ctx context.Context, conn bun.Conn) {
	if conn.Dialect().Name() != dialect.PG {
		return
	}

	_, err := conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(?)", schemaLockKey)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to release the schema lock")
	}
}

// comparePostgres - Reads the live catalog, then builds the declared schema
// as temporary tables in a transaction that is never committed and reads
// its catalog. The order matters: temporary tables shadow real ones by name
// and Postgres qualifies shadowed names in what it prints. It returns the
// plan and the desired catalog with raw default expressions
func comparePostgres(ctx context.Context, conn bun.Conn, tables []declaredTable) (Plan, Catalog, error) {
	var namespaceID int64
	var namespace string
	err := conn.NewRaw("SELECT oid::bigint, quote_ident(nspname) FROM pg_namespace WHERE nspname = current_schema()").Scan(ctx, &namespaceID, &namespace)
	if err != nil {
		return Plan{}, nil, ez.Wrap(err)
	}

	live, err := readCatalog(ctx, conn, namespaceID)
	if err != nil {
		return Plan{}, nil, ez.Wrap(err)
	}
	delete(live, ledgerTable)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return Plan{}, nil, ez.Wrap(err)
	}
	defer tx.Rollback() // nolint:errcheck // Rolling back is how the scratch tables go away

	desired, err := buildDesired(ctx, tx, tables)
	if err != nil {
		return Plan{}, nil, ez.Wrap(err)
	}

	err = markVolatileDefaults(ctx, tx, desired, live)
	if err != nil {
		return Plan{}, nil, ez.Wrap(err)
	}

	plan := diff(normalizeDefaults(desired, namespace), normalizeDefaults(live, namespace))
	return plan, desired, nil
}

// buildDesired - Creates the declared tables as temporary tables, then their
// declared indexes and constraints unqualified so they land on the temporary
// tables, and reads the catalog of the temporary schema
func buildDesired(ctx context.Context, tx bun.Tx, tables []declaredTable) (Catalog, error) {
	for _, table := range tables {
		statement := tx.NewCreateTable().Model(table.Model).Temp().String() + " ON COMMIT DROP"
		_, err := tx.ExecContext(ctx, statement)
		if err != nil {
			return nil, declarationError("table", table.Name, err)
		}
	}

	for _, table := range tables {
		for _, index := range table.Indexes {
			_, err := tx.ExecContext(ctx, createIndexSQL(tx, table.Name, index, ""))
			if err != nil {
				return nil, declarationError("index", index.Name, err)
			}
		}
	}

	for _, table := range tables {
		for _, constraint := range table.Constraints {
			_, err := tx.ExecContext(ctx, addConstraintSQL(tx, table.Name, constraint.Name, constraint.Def))
			if err != nil {
				return nil, declarationError("constraint", constraint.Name, err)
			}
		}
	}

	var namespaceID int64
	err := tx.NewRaw("SELECT pg_my_temp_schema()::bigint").Scan(ctx, &namespaceID)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	catalog, err := readCatalog(ctx, tx, namespaceID)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	return catalog, nil
}

// declarationError - EINVALID for a declaration Postgres refused to build as
// part of the desired catalog, for example a FOREIGN KEY to an undeclared
// table, which a temporary table cannot reference. Errors that did not come
// from the server are only wrapped
func declarationError(kind, name string, err error) error {
	var pgErr pgdriver.Error
	if !errors.As(err, &pgErr) {
		return ez.Wrap(err)
	}

	msg := fmt.Sprintf("Declared %s %q cannot be built for the comparison: %s", kind, name, pgErr.Field('M'))
	return ez.New(ez.EINVALID, msg, nil)
}

// readCatalog - The application tables of one namespace with their columns,
// valid indexes and constraints. Indexes that back a constraint are left out,
// since they are compared as constraints
func readCatalog(ctx context.Context, db bun.IDB, namespaceID int64) (Catalog, error) {
	var tableNames []string
	err := db.NewRaw("SELECT c.relname FROM pg_class c WHERE c.relnamespace = ? AND "+applicationTables, namespaceID).Scan(ctx, &tableNames)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	catalog := make(Catalog, len(tableNames))
	for _, name := range tableNames {
		catalog[name] = Table{Columns: map[string]Column{}, Indexes: map[string]string{}, Constraints: map[string]string{}}
	}

	var columns []struct {
		TableName   string `bun:"table_name"`
		Name        string `bun:"name"`
		TypeName    string `bun:"type_name"`
		NotNull     bool   `bun:"not_null"`
		Expression  string `bun:"expression"`
		IsGenerated bool   `bun:"is_generated"`
		Identity    string `bun:"identity"`
	}
	err = db.NewRaw(`SELECT c.relname AS table_name, a.attname AS name,
		format_type(a.atttypid, a.atttypmod) AS type_name, a.attnotnull AS not_null,
		coalesce(pg_get_expr(ad.adbin, ad.adrelid), '') AS expression,
		a.attgenerated <> '' AS is_generated, a.attidentity::text AS identity
		FROM pg_class c
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE c.relnamespace = ? AND `+applicationTables, namespaceID).Scan(ctx, &columns)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	// A table created between these queries is skipped rather than half read
	for _, column := range columns {
		table, ok := catalog[column.TableName]
		if !ok {
			continue
		}

		table.Columns[column.Name] = Column{
			Type:      column.TypeName,
			NotNull:   column.NotNull,
			Default:   column.Expression,
			Generated: column.IsGenerated,
			Identity:  column.Identity,
		}
	}

	var indexes []struct {
		TableName   string `bun:"table_name"`
		Name        string `bun:"name"`
		Namespace   string `bun:"namespace"`
		Definition  string `bun:"definition"`
		Partitioned bool   `bun:"partitioned"`
	}
	err = db.NewRaw(`SELECT c.relname AS table_name, ic.relname AS name,
		quote_ident(CASE WHEN n.oid = pg_my_temp_schema() THEN 'pg_temp' ELSE n.nspname END) AS namespace,
		pg_get_indexdef(i.indexrelid) AS definition, c.relkind = 'p' AS partitioned
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_index i ON i.indrelid = c.oid AND i.indisvalid
		JOIN pg_class ic ON ic.oid = i.indexrelid
		WHERE c.relnamespace = ? AND `+applicationTables+`
		AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conindid = i.indexrelid AND k.conrelid = c.oid AND k.contype IN ('p', 'u', 'x'))`,
		namespaceID).Scan(ctx, &indexes)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	for _, index := range indexes {
		table, ok := catalog[index.TableName]
		if !ok {
			continue
		}

		// pg_get_indexdef always qualifies the table in the ON clause, naming
		// the session's own temporary schema pg_temp, and says ON ONLY for a
		// partitioned table, which the declared temporary table never is
		on := " ON "
		if index.Partitioned {
			on = " ON ONLY "
		}

		table.Indexes[index.Name] = strings.Replace(index.Definition, on+index.Namespace+".", " ON ", 1)
	}

	var constraints []struct {
		TableName  string `bun:"table_name"`
		Name       string `bun:"name"`
		Definition string `bun:"definition"`
	}
	err = db.NewRaw(`SELECT c.relname AS table_name, k.conname AS name, pg_get_constraintdef(k.oid) AS definition
		FROM pg_class c
		JOIN pg_constraint k ON k.conrelid = c.oid AND k.contype IN ('p', 'u', 'c', 'f', 'x')
		WHERE c.relnamespace = ? AND `+applicationTables, namespaceID).Scan(ctx, &constraints)
	if err != nil {
		return nil, ez.Wrap(err)
	}

	for _, constraint := range constraints {
		table, ok := catalog[constraint.TableName]
		if !ok {
			continue
		}

		table.Constraints[constraint.Name] = constraint.Definition
	}

	return catalog, nil
}

// normalizeDefaults - A copy of the catalog for comparison only, with the
// pg_temp_<n>, pg_temp or current schema qualifier stripped from identifiers
// in default expressions. Never execute what it returns
func normalizeDefaults(catalog Catalog, namespace string) Catalog {
	qualifier := regexp.MustCompile(`(^|[^\w$"])(?:pg_temp(?:_\d+)?|` + regexp.QuoteMeta(namespace) + `)\.`)

	normalized := make(Catalog, len(catalog))
	for name, table := range catalog {
		columns := make(map[string]Column, len(table.Columns))
		for columnName, column := range table.Columns {
			column.Default = normalizeDefault(column.Default, qualifier)
			columns[columnName] = column
		}

		normalized[name] = Table{Columns: columns, Indexes: table.Indexes, Constraints: table.Constraints}
	}

	return normalized
}

// normalizeDefault - One default expression with the qualifier stripped
// outside string literals, which are values and stay as written, and inside
// a regclass literal, which names a relation (nextval of a sequence).
// pg_get_expr writes a literal with doubled quotes and no escapes
func normalizeDefault(expression string, qualifier *regexp.Regexp) string {
	var out strings.Builder
	for expression != "" {
		start := strings.IndexByte(expression, '\'')
		if start < 0 {
			out.WriteString(qualifier.ReplaceAllString(expression, "${1}"))
			break
		}

		out.WriteString(qualifier.ReplaceAllString(expression[:start], "${1}"))

		end := literalEnd(expression, start)
		literal := expression[start:end]
		expression = expression[end:]
		if strings.HasPrefix(expression, "::regclass") {
			literal = qualifier.ReplaceAllString(literal, "${1}")
		}

		out.WriteString(literal)
	}

	return out.String()
}

// literalEnd - The index just past the string literal opening at start,
// where a doubled quote is part of the literal. An unterminated literal runs
// to the end
func literalEnd(expression string, start int) int {
	for i := start + 1; i < len(expression); i++ {
		if expression[i] != '\'' {
			continue
		}

		if i+1 < len(expression) && expression[i+1] == '\'' {
			i++
			continue
		}

		return i + 1
	}

	return len(expression)
}

// markVolatileDefaults - Sets VolatileDefault on each desired column with a
// default that an existing live table lacks
func markVolatileDefaults(ctx context.Context, tx bun.Tx, desired, live Catalog) error {
	for tableName, table := range desired {
		liveTable, ok := live[tableName]
		if !ok {
			continue
		}

		for name, column := range table.Columns {
			// A generation expression may reference other columns, which
			// the probe table lacks, and generated columns are refused anyway
			_, exists := liveTable.Columns[name]
			if exists || column.Default == "" || column.Generated {
				continue
			}

			volatile, err := defaultIsVolatile(ctx, tx, column)
			if err != nil {
				return ez.Wrap(err)
			}

			column.VolatileDefault = volatile
			table.Columns[name] = column
		}
	}

	return nil
}

// defaultIsVolatile - Whether adding the column would rewrite the table.
// Postgres stores a non-volatile default of an added column as a "missing"
// value instead, so this adds the column to an empty scratch table and reads
// atthasmissing. pg_depend cannot answer it: Postgres records no dependency
// on built-in functions such as gen_random_uuid()
func defaultIsVolatile(ctx context.Context, tx bun.Tx, column Column) (bool, error) {
	_, err := tx.ExecContext(ctx, "CREATE TEMP TABLE schema_default_probe () ON COMMIT DROP")
	if err != nil {
		return false, ez.Wrap(err)
	}

	_, err = tx.ExecContext(ctx, "ALTER TABLE schema_default_probe ADD COLUMN probe "+column.Type+" DEFAULT "+column.Default)
	if err != nil {
		return false, ez.Wrap(err)
	}

	var stored bool
	err = tx.NewRaw("SELECT atthasmissing FROM pg_attribute WHERE attrelid = 'schema_default_probe'::regclass AND attname = 'probe'").Scan(ctx, &stored)
	if err != nil {
		return false, ez.Wrap(err)
	}

	_, err = tx.ExecContext(ctx, "DROP TABLE schema_default_probe")
	if err != nil {
		return false, ez.Wrap(err)
	}

	return !stored, nil
}

// addColumnSQL - Renders ALTER TABLE ADD COLUMN IF NOT EXISTS from a desired
// column. bun's own renderer omits NOT NULL
func addColumnSQL(db bun.IDB, table, name string, column Column) string {
	statement := "ALTER TABLE " + quoteIdent(db, table) + " ADD COLUMN IF NOT EXISTS " + quoteIdent(db, name) + " " + column.Type
	if column.NotNull {
		statement += " NOT NULL"
	}

	switch {
	case column.Generated:
		statement += " GENERATED ALWAYS AS (" + column.Default + ") STORED"
	case column.Default != "":
		statement += " DEFAULT " + column.Default
	}

	return statement
}

// columnRefusal - Why Apply must not add a missing column to a table an
// older binary still writes, "" when it may
func columnRefusal(column Column) string {
	switch {
	case column.Generated:
		return "generated column, add it with a step"
	case column.Identity != "":
		return "an identity column would rewrite the table, add it with a step"
	case strings.Contains(column.Default, "nextval("):
		return "a sequence default would rewrite the table, add it with a step"
	case column.NotNull && column.Default == "":
		return "older binary's inserts would fail, add a default or add it nullable and tighten with a step"
	case column.VolatileDefault:
		return "a volatile default would rewrite the table, add it with a step"
	}

	return ""
}

// applyPostgres - Creates the missing tables, columns and indexes Apply
// allows, in that order, and marks the rest refused. Missing constraints are
// always refused
func applyPostgres(ctx context.Context, conn bun.Conn, schema Schema, tables []declaredTable, desired Catalog, plan Plan) (Plan, error) {
	// One transaction for all new tables, so their constraints may reference
	// each other. No older binary knows these tables, but a FOREIGN KEY locks
	// the existing table it references, so the short timeouts keep that
	// table's writers from queueing behind it
	created := missingTables(tables, plan)
	if len(created) > 0 {
		err := conn.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			err := setShortTimeouts(ctx, tx)
			if err != nil {
				return err
			}

			return createTables(ctx, tx, created, "")
		})
		if err != nil {
			return plan, ez.Wrap(err)
		}

		for _, table := range created {
			log.Info().Str("Table", table.Name).Msg("Created declared table")
		}
	}

	// The tables left without a declared column, whose missing indexes may
	// need it
	lackingColumns := map[string]bool{}
	for i, change := range plan.Missing {
		if change.Kind != "column" {
			continue
		}

		column := desired[change.Table].Columns[change.Name]
		statement := addColumnSQL(conn, change.Table, change.Name, column)

		reason := columnRefusal(column)
		if reason != "" {
			plan.Missing[i] = refuse(change, reason, statement)
			lackingColumns[change.Table] = true
			continue
		}

		err := ExecShort(ctx, conn, statement)
		if err != nil {
			return plan, ez.Wrap(err)
		}

		log.Info().Str("Table", change.Table).Str("Name", change.Name).Msg("Created declared column")
	}

	maxSize := schema.MaxIndexTableSize
	if maxSize == 0 {
		maxSize = DefaultMaxIndexTableSize
	}

	for i, change := range plan.Missing {
		if change.Kind != "index" {
			continue
		}

		table, index, ok := findIndex(tables, change.Name)
		if !ok {
			return plan, ez.New(ez.EINTERNAL, "A missing index has no declaration", nil)
		}

		statement := createIndexSQL(conn, table, index, "CONCURRENTLY IF NOT EXISTS")
		if lackingColumns[table] {
			plan.Missing[i] = refuse(change, refusalLackingColumn, statement)
			continue
		}

		if index.Unique {
			plan.Missing[i] = refuse(change, refusalUniqueIndex, statement)
			continue
		}

		// The identifier is quoted inside the literal because regclass input
		// parses it
		var partitioned bool
		var size int64
		err := conn.NewRaw("SELECT relkind = 'p', pg_relation_size(oid) FROM pg_class WHERE oid = ?::regclass", quoteIdent(conn, table)).
			Scan(ctx, &partitioned, &size)
		if err != nil {
			return plan, ez.Wrap(err)
		}

		if partitioned {
			plan.Missing[i] = refuse(change, "CONCURRENTLY does not support partitioned tables, build it with a step", createIndexSQL(conn, table, index, "IF NOT EXISTS"))
			continue
		}

		if size > maxSize {
			reason := fmt.Sprintf("the table is %d bytes, over the %d byte limit, build it by hand", size, maxSize)
			plan.Missing[i] = refuse(change, reason, statement)
			continue
		}

		reason, err := buildIndex(ctx, conn, table, index)
		if err != nil {
			return plan, ez.Wrap(err)
		}

		if reason != "" {
			plan.Missing[i] = refuse(change, reason, statement)
			continue
		}

		log.Info().Str("Table", table).Str("Name", index.Name).Msg("Created declared index")
	}

	for i, change := range plan.Missing {
		if change.Kind != "constraint" {
			continue
		}

		statement := addConstraintSQL(conn, change.Table, change.Name, desired[change.Table].Constraints[change.Name])

		// ApplyDeclared only knows declared constraints, not those from bun
		// tags or primary keys
		reason := "add it with a step"
		_, _, ok := findConstraint(tables, change.Name)
		if ok {
			reason = "install it with a step calling ApplyDeclared once the data satisfies it"
		}

		plan.Missing[i] = refuse(change, reason, statement)
	}

	return plan, nil
}

// postgresLiveIndex - The table an index of the current schema is on, by
// index name, and whether the index is valid
const postgresLiveIndex = `SELECT t.relname AS table_name, i.indisvalid AS valid FROM pg_index i
	JOIN pg_class ic ON ic.oid = i.indexrelid
	JOIN pg_class t ON t.oid = i.indrelid
	JOIN pg_namespace n ON n.oid = ic.relnamespace
	WHERE n.nspname = current_schema() AND ic.relname = ?`

// buildIndex - Builds a missing non-unique index without blocking writes,
// returning the reason when it refuses to. Index names are unique per
// schema, so a name another table owns is refused, and so is a name a
// constraint's index on the table already has, since the comparison leaves
// those out. An invalid leftover of an interrupted build on the table is
// dropped first, the build runs CONCURRENTLY outside any transaction, and
// the result must be valid. Each wait for another transaction is bounded by
// IndexBuildLockTimeout: when one runs out, what the canceled build left is
// dropped if it can be
func buildIndex(ctx context.Context, conn bun.Conn, table string, index Index) (string, error) {
	live, err := findLiveIndex(ctx, conn, index.Name)
	if err != nil {
		return "", ez.Wrap(err)
	}

	if live.Found && live.Table != table {
		return refusalOwnedElsewhere(index.Name, live.Table), nil
	}

	if live.Found && live.Valid {
		return fmt.Sprintf("an index named %q already backs a constraint on the table, rename the declaration or declare the constraint", index.Name), nil
	}

	// CONCURRENTLY cannot run in a transaction, so SET LOCAL does not apply
	_, err = conn.ExecContext(ctx, fmt.Sprintf("SET lock_timeout = %d", IndexBuildLockTimeout.Milliseconds()))
	if err != nil {
		return "", ez.Wrap(err)
	}

	defer func() {
		_, err := conn.ExecContext(context.WithoutCancel(ctx), "RESET lock_timeout")
		if err != nil {
			log.Warn().Err(err).Msg("Failed to reset lock_timeout after an index build")
		}
	}()

	waited := fmt.Sprintf("the build waited over %s for other transactions, build it by hand", IndexBuildLockTimeout)
	drop := "DROP INDEX CONCURRENTLY IF EXISTS " + quoteIdent(conn, index.Name)
	if live.Found {
		_, err = conn.ExecContext(ctx, drop)
		if lockTimedOut(err) {
			return waited, nil
		}

		if err != nil {
			return "", ez.Wrap(err)
		}
	}

	_, err = conn.ExecContext(ctx, createIndexSQL(conn, table, index, "CONCURRENTLY"))
	if lockTimedOut(err) {
		// The canceled build leaves an invalid index behind, and dropping it
		// waits for the same transactions the build waited for
		_, err = conn.ExecContext(ctx, drop)
		if lockTimedOut(err) {
			log.Warn().Str("Name", index.Name).Msg("Timed out dropping the invalid index a canceled build left, the next build drops it first")
			return waited, nil
		}

		if err != nil {
			return "", ez.Wrap(err)
		}

		return waited, nil
	}

	if err != nil {
		return "", ez.Wrap(err)
	}

	live, err = findLiveIndex(ctx, conn, index.Name)
	if err != nil {
		return "", ez.Wrap(err)
	}

	if !live.Found || live.Table != table || !live.Valid {
		msg := fmt.Sprintf("Index %q was built but is not a valid index of table %q", index.Name, table)
		return "", ez.New(ez.EINTERNAL, msg, nil)
	}

	return "", nil
}

// lockTimedOut - Whether Postgres canceled the statement because
// lock_timeout ran out (SQLSTATE 55P03)
func lockTimedOut(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "55P03"
}
