package relational

import (
	"context"
	"reflect"
	"slices"

	"github.com/rs/zerolog/log"
	"github.com/uptrace/bun"
	"github.com/vanclief/ez"
)

// sqliteTableCount - Tables in the database, not counting SQLite's own
const sqliteTableCount = `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND substr(name, 1, 7) <> 'sqlite_'`

// sqliteTableExists - Whether the database has a table by name
const sqliteTableExists = `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`

// sqliteMissing - The declared tables, columns and indexes the database
// lacks, by name. SQLite gets no catalog comparison, so Different and Extra
// stay empty
func sqliteMissing(ctx context.Context, conn bun.Conn, tables []declaredTable) (Plan, error) {
	var tableNames []string
	err := conn.NewRaw("SELECT name FROM sqlite_master WHERE type = 'table'").Scan(ctx, &tableNames)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}

	var indexNames []string
	err = conn.NewRaw("SELECT name FROM sqlite_master WHERE type = 'index'").Scan(ctx, &indexNames)
	if err != nil {
		return Plan{}, ez.Wrap(err)
	}

	var plan Plan
	for _, table := range tables {
		if !slices.Contains(tableNames, table.Name) {
			plan.Missing = append(plan.Missing, Change{Kind: "table", Table: table.Name, Name: table.Name})
			continue
		}

		var columnNames []string
		err = conn.NewRaw("SELECT name FROM pragma_table_info(?)", table.Name).Scan(ctx, &columnNames)
		if err != nil {
			return Plan{}, ez.Wrap(err)
		}

		bunTable := conn.Dialect().Tables().Get(reflect.TypeOf(table.Model).Elem())
		for _, field := range bunTable.Fields {
			if slices.Contains(columnNames, field.Name) {
				continue
			}

			want := field.CreateTableSQLType
			if field.NotNull {
				want += " NOT NULL"
			}

			if field.SQLDefault != "" {
				want += " DEFAULT " + field.SQLDefault
			}

			plan.Missing = append(plan.Missing, Change{Kind: "column", Table: table.Name, Name: field.Name, Want: want})
		}

		for _, index := range table.Indexes {
			if !slices.Contains(indexNames, index.Name) {
				want := createIndexSQL(conn, table.Name, index, "")
				plan.Missing = append(plan.Missing, Change{Kind: "index", Table: table.Name, Name: index.Name, Want: want})
			}
		}
	}

	return plan, nil
}

// applySQLite - Creates missing tables with their declared indexes and
// missing indexes. A missing column is refused: SQLite gets no column
// renderer in this version, so it is added with a step
func applySQLite(ctx context.Context, conn bun.Conn, tables []declaredTable, plan Plan) (Plan, error) {
	created := missingTables(tables, plan)
	if len(created) > 0 {
		err := conn.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
			return createTables(ctx, tx, created, "")
		})
		if err != nil {
			return plan, ez.Wrap(err)
		}
	}

	for i, change := range plan.Missing {
		switch change.Kind {
		case "table":
			log.Info().Str("Table", change.Table).Msg("Created declared table")
		case "column":
			statement := "ALTER TABLE " + quoteIdent(conn, change.Table) + " ADD COLUMN " + quoteIdent(conn, change.Name) + " " + change.Want
			plan.Missing[i] = refuse(change, "add it with a step", statement)
		case "index":
			table, index, ok := findIndex(tables, change.Name)
			if !ok {
				return plan, ez.New(ez.EINTERNAL, "A missing index has no declaration", nil)
			}

			_, err := conn.ExecContext(ctx, createIndexSQL(conn, table, index, "IF NOT EXISTS"))
			if err != nil {
				return plan, ez.Wrap(err)
			}

			log.Info().Str("Table", table).Str("Name", index.Name).Msg("Created declared index")
		}
	}

	return plan, nil
}
