package relational

import (
	"context"
	"database/sql"
	"errors"
	"reflect"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
	"github.com/uptrace/bun/schema"
	"github.com/vanclief/ez"
)

// LockRow locks the row model's primary key names until tx ends and loads
// its current values into model. Other transactions that lock or write the
// row wait; plain reads do not.
//
// It takes a bun.Tx rather than a bun.IDB because a row lock taken outside a
// transaction is released when the statement ends and protects nothing.
//
// On PostgreSQL the lock is FOR NO KEY UPDATE rather than FOR UPDATE, so
// inserts of rows that reference this one by foreign key are not blocked.
// SQLite has no row locks, so there the row is only loaded, and on a file
// database a second transaction that loaded the row meanwhile fails its write
// with a "database is locked" error instead of waiting. A missing row is
// ENOTFOUND.
func LockRow(ctx context.Context, tx bun.Tx, model any) error {
	query := tx.NewSelect().Model(model).WherePK()
	if tx.Dialect().Name() == dialect.PG {
		query = query.For("NO KEY UPDATE")
	}

	err := query.Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return ez.New(ez.ENOTFOUND, "Record not found", err)
	}
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// Mutate locks the row model's primary key names, loads it into model, runs
// fn, then calls model.Update, all in one transaction. Set fields inside fn:
// whatever was set on model before the call is overwritten by the load.
//
// An error from the load, fn or Update rolls the transaction back and is
// returned with its code. When db is already a bun.Tx, Mutate runs inside a
// savepoint of it, and the caller's commit or rollback decides the outcome.
//
// fn must not change model's primary key: Update finds the row by the key in
// model, so it would write a row that was never locked. A key that differs
// after fn from the one locked, in any column of a composite key, is
// EINTERNAL, returned before Update is called and rolled back like any other
// error.
//
// fn should write through tx, so its writes roll back with the rest. Side
// effects that cannot be rolled back, such as sending an email or calling
// another service, belong after Mutate returns, not inside fn: fn may have
// run for a change that was never saved.
//
// When fn locks other rows too, with LockRow, every code path must lock them
// in the same order, counting model's row, which is locked before fn runs, or
// two requests can deadlock and PostgreSQL fails one of them.
func Mutate(ctx context.Context, db bun.IDB, model DBModel, fn func(ctx context.Context, tx bun.Tx) error) error {
	// The steps use Mutate's ctx, not the one bun hands the closure: inside a
	// caller's transaction bun passes that transaction's ctx, which would drop
	// a deadline set for this call only.
	err := db.RunInTx(ctx, nil, func(_ context.Context, tx bun.Tx) error {
		err := LockRow(ctx, tx, model)
		if err != nil {
			return ez.Wrap(err)
		}

		table, err := rowTable(tx, model)
		if err != nil {
			return ez.Wrap(err)
		}

		locked := primaryKey(tx, table, model)

		err = fn(ctx, tx)
		if err != nil {
			return ez.Wrap(err)
		}

		current := primaryKey(tx, table, model)
		for i, field := range table.PKs {
			if current[i] != locked[i] {
				return ez.New(ez.EINTERNAL, "The record's identifier cannot be changed", nil).AddData("column", field.Name)
			}
		}

		err = model.Update(ctx, tx)
		if err != nil {
			return ez.Wrap(err)
		}

		return nil
	})
	if err != nil {
		return ez.Wrap(err)
	}

	return nil
}

// primaryKey returns the SQL text of each primary-key value of model, in the
// order of table.PKs: what a write by primary key puts in its WHERE clause.
func primaryKey(db bun.IDB, table *schema.Table, model any) []string {
	gen := schema.NewQueryGen(db.Dialect())
	row := reflect.ValueOf(model).Elem()

	values := make([]string, len(table.PKs))
	for i, field := range table.PKs {
		values[i] = string(field.AppendValue(gen, nil, row))
	}

	return values
}
