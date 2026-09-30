package relational

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/vanclief/ez"
)

// Mode - What Migrate may do to an existing database beyond running steps
type Mode int

const (
	// Report runs pending steps and logs the plan. It changes nothing else
	Report Mode = iota
	// Apply also creates what is missing when that is safe while an older
	// binary still serves: new tables with their declarations, nullable or
	// constant-default columns, and non-unique indexes on small tables
	Apply
)

// Index - A secondary index declared by a model. Def is what follows
// "ON table": columns, method, predicate. For example
// "(transfer_id) WHERE transfer_id IS NOT NULL" or "USING gin (tags)"
type Index struct {
	Name   string
	Unique bool
	Def    string
}

// Constraint - A CHECK or FOREIGN KEY constraint declared by a model. Def is
// what follows "ADD CONSTRAINT name"
type Constraint struct {
	Name string
	Def  string
}

// IndexedModel - Optional interface a model implements to declare indexes
// that bun tags cannot express. The table is the model's own
type IndexedModel interface{ Indexes() []Index }

// ConstrainedModel - Optional interface a model implements to declare
// constraints that bun tags cannot express. The table is the model's own
type ConstrainedModel interface{ Constraints() []Constraint }

// Step - Moves an existing database off a shape the models no longer
// describe: a rename, a backfill, a type change, a drop, or installing a
// declared unique index or constraint on a populated table. A fresh
// database is created at the declared shape and never runs one. Run
// receives the pinned connection Migrate holds its lock on
type Step struct {
	Name string
	Run  func(ctx context.Context, db bun.IDB) error
}

// Schema - Everything a database must match
type Schema struct {
	Models     []interface{}
	Extensions []string // Postgres only, created before anything else
	Steps      []Step   // ordered, append at the bottom, delete from the top
	Floor      string   // name of the newest deleted step, "" when none
	Mode       Mode
	// MaxIndexTableSize - Apply builds a missing index at boot only when
	// pg_relation_size of its table is at most this many bytes, zero means
	// DefaultMaxIndexTableSize. The build must also finish inside the
	// connection's timeouts (30s by default in compose, see
	// IndexBuildLockTimeout). Larger indexes are pre-built by an operator
	// with the statement Apply logs
	MaxIndexTableSize int64
}

// DefaultMaxIndexTableSize - The largest table, in bytes, Apply builds a
// missing index on when Schema.MaxIndexTableSize is zero
const DefaultMaxIndexTableSize int64 = 256 << 20

// Change - One difference between the declared schema and the database
type Change struct {
	Kind    string // "table", "column", "index", "constraint"
	Table   string
	Name    string // column, index or constraint name, equals Table for a table
	Want    string // canonical declared definition ("" for Extra and for tables)
	Have    string // canonical live definition ("" for Missing and for tables)
	Refused string // Apply only: why this missing object was not created
}

// Plan - What Migrate found. Missing is declared but absent, Different is
// present with another definition, Extra is present but undeclared
type Plan struct {
	Missing   []Change
	Different []Change
	Extra     []Change
}

// declaredTable - A model's table with the indexes and constraints it declares
type declaredTable struct {
	Name        string
	Model       interface{}
	Indexes     []Index
	Constraints []Constraint
}

// declare - Collects the tables the models map to with their declared
// indexes and constraints. Table, index and constraint names must be unique
// across all models, since ApplyDeclared finds objects by name
func declare(db bun.IDB, models []interface{}) ([]declaredTable, error) {
	tables := make([]declaredTable, 0, len(models))
	seen := map[string]bool{}

	for _, model := range models {
		bunTable := db.Dialect().Tables().Get(reflect.TypeOf(model).Elem())
		if strings.Contains(bunTable.Name, ".") {
			msg := fmt.Sprintf("Table %q is schema-qualified, but the schema engine only works in the connection's current schema", bunTable.Name)
			return nil, ez.New(ez.EINVALID, msg, nil)
		}

		table := declaredTable{Name: bunTable.Name, Model: model}

		// ZeroIface is a non-nil pointer, so value and pointer receivers
		// both work
		indexed, ok := bunTable.ZeroIface.(IndexedModel)
		if ok {
			table.Indexes = indexed.Indexes()
		}

		constrained, ok := bunTable.ZeroIface.(ConstrainedModel)
		if ok {
			table.Constraints = constrained.Constraints()
		}

		if len(table.Constraints) > 0 && db.Dialect().Name() == dialect.SQLite {
			msg := fmt.Sprintf("Constraint %q is declared, but declared constraints are only supported by PostgreSQL", table.Constraints[0].Name)
			return nil, ez.New(ez.ENOTIMPLEMENTED, msg, nil)
		}

		names := []string{table.Name}
		for _, index := range table.Indexes {
			names = append(names, index.Name)
		}

		for _, constraint := range table.Constraints {
			names = append(names, constraint.Name)
		}

		for _, name := range names {
			if seen[name] {
				msg := fmt.Sprintf("%q is declared more than once, table, index and constraint names must be unique", name)
				return nil, ez.New(ez.EINVALID, msg, nil)
			}
			seen[name] = true
		}

		tables = append(tables, table)
	}

	return tables, nil
}

// findIndex - The declared index with this name and the table it belongs to
func findIndex(tables []declaredTable, name string) (string, Index, bool) {
	for _, table := range tables {
		for _, index := range table.Indexes {
			if index.Name == name {
				return table.Name, index, true
			}
		}
	}

	return "", Index{}, false
}

// findConstraint - The declared constraint with this name and the table it
// belongs to
func findConstraint(tables []declaredTable, name string) (string, Constraint, bool) {
	for _, table := range tables {
		for _, constraint := range table.Constraints {
			if constraint.Name == name {
				return table.Name, constraint, true
			}
		}
	}

	return "", Constraint{}, false
}

// quoteIdent - Quotes an identifier through bun's formatter for the dialect
func quoteIdent(db bun.IDB, name string) string {
	return string(dialect.AppendIdent(nil, name, db.Dialect().IdentQuote()))
}

// createIndexSQL - Renders CREATE [UNIQUE] INDEX [modifier] name ON table
// def, where modifier is "", "IF NOT EXISTS" or "CONCURRENTLY IF NOT EXISTS"
func createIndexSQL(db bun.IDB, table string, index Index, modifier string) string {
	statement := "CREATE "
	if index.Unique {
		statement += "UNIQUE "
	}

	statement += "INDEX "
	if modifier != "" {
		statement += modifier + " "
	}

	return statement + quoteIdent(db, index.Name) + " ON " + quoteIdent(db, table) + " " + index.Def
}

// addConstraintSQL - Renders ALTER TABLE table ADD CONSTRAINT name def
func addConstraintSQL(db bun.IDB, table, name, def string) string {
	return "ALTER TABLE " + quoteIdent(db, table) + " ADD CONSTRAINT " + quoteIdent(db, name) + " " + def
}

// createTables - Creates the tables, then their declared indexes, then their
// declared constraints, so a constraint may reference any of the tables. The
// caller runs it in a transaction. modifier is "" or "IF NOT EXISTS" and
// applies to the tables and indexes
func createTables(ctx context.Context, db bun.IDB, tables []declaredTable, modifier string) error {
	for _, table := range tables {
		query := db.NewCreateTable().Model(table.Model)
		if modifier != "" {
			query = query.IfNotExists()
		}

		_, err := query.Exec(ctx)
		if err != nil {
			return ez.Wrap(err)
		}
	}

	for _, table := range tables {
		for _, index := range table.Indexes {
			_, err := db.ExecContext(ctx, createIndexSQL(db, table.Name, index, modifier))
			if err != nil {
				return ez.Wrap(err)
			}
		}
	}

	for _, table := range tables {
		for _, constraint := range table.Constraints {
			_, err := db.ExecContext(ctx, addConstraintSQL(db, table.Name, constraint.Name, constraint.Def))
			if err != nil {
				return ez.Wrap(err)
			}
		}
	}

	return nil
}

// ApplyDeclared - Creates one declared object by name from inside a step:
// a table (with its declared indexes and constraints), an index, or a
// constraint. IF NOT EXISTS where the dialect supports it. EINVALID when no
// declaration has that name
func ApplyDeclared(ctx context.Context, db bun.IDB, models []interface{}, name string) error {
	tables, err := declare(db, models)
	if err != nil {
		return ez.Wrap(err)
	}

	for _, table := range tables {
		if table.Name != name {
			continue
		}

		err = db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return createTables(ctx, tx, []declaredTable{table}, "IF NOT EXISTS")
		})
		if err != nil {
			return ez.Wrap(err)
		}

		return nil
	}

	table, index, ok := findIndex(tables, name)
	if ok {
		err = createDeclaredIndex(ctx, db, table, index)
		if err != nil {
			return ez.Wrap(err)
		}

		return nil
	}

	table, constraint, ok := findConstraint(tables, name)
	if ok {
		_, err = db.ExecContext(ctx, addConstraintSQL(db, table, constraint.Name, constraint.Def))
		if err != nil {
			return ez.Wrap(err)
		}

		return nil
	}

	msg := fmt.Sprintf("No declared table, index or constraint is named %q", name)
	return ez.New(ez.EINVALID, msg, nil)
}

// createDeclaredIndex - Creates a declared index unless it exists. On
// Postgres IF NOT EXISTS matches by name, so an invalid leftover of a failed
// concurrent build is dropped first, with a plain DROP since the step may run
// in a transaction, and the index must end up valid
func createDeclaredIndex(ctx context.Context, db bun.IDB, table string, index Index) error {
	statement := createIndexSQL(db, table, index, "IF NOT EXISTS")
	if db.Dialect().Name() != dialect.PG {
		_, err := db.ExecContext(ctx, statement)
		if err != nil {
			return ez.Wrap(err)
		}

		return nil
	}

	var validity []bool
	err := db.NewRaw(indexValidity, index.Name).Scan(ctx, &validity)
	if err != nil {
		return ez.Wrap(err)
	}

	if len(validity) > 0 && !validity[0] {
		_, err = db.ExecContext(ctx, "DROP INDEX IF EXISTS "+quoteIdent(db, index.Name))
		if err != nil {
			return ez.Wrap(err)
		}
	}

	_, err = db.ExecContext(ctx, statement)
	if err != nil {
		return ez.Wrap(err)
	}

	var valid bool
	err = db.NewRaw(indexValidity, index.Name).Scan(ctx, &valid)
	if err != nil {
		return ez.Wrap(err)
	}

	if !valid {
		msg := fmt.Sprintf("Index %q exists but is not valid", index.Name)
		return ez.New(ez.EINTERNAL, msg, nil)
	}

	return nil
}

// ExecShort - Runs one DDL statement in a transaction with lock_timeout 2s
// and statement_timeout 10s on Postgres (plain Exec on SQLite), for steps
// that take an ACCESS EXCLUSIVE lock
func ExecShort(ctx context.Context, db bun.IDB, statement string) error {
	if db.Dialect().Name() != dialect.PG {
		_, err := db.ExecContext(ctx, statement)
		if err != nil {
			return ez.Wrap(err)
		}

		return nil
	}

	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		err := setShortTimeouts(ctx, tx)
		if err != nil {
			return err
		}

		_, err = tx.ExecContext(ctx, statement)
		return err
	})
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// setShortTimeouts - Sets lock_timeout 2s and statement_timeout 10s for the
// rest of a Postgres transaction, so DDL that waits for a lock gives up
// before the writers queued behind it pile up
func setShortTimeouts(ctx context.Context, tx bun.Tx) error {
	_, err := tx.ExecContext(ctx, "SET LOCAL lock_timeout = '2s'")
	if err != nil {
		return ez.Wrap(err)
	}

	_, err = tx.ExecContext(ctx, "SET LOCAL statement_timeout = '10s'")
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}
