# Concurrent writes

Two requests that change the same row at the same time can undo each other's work without either
one failing. This guide explains why, and which of compose's tools to use for each kind of write.

It covers:

- [The problem](#the-problem)
- [Choosing the tool](#choosing-the-tool)
- [Independent fields: `relational.Patch`](#independent-fields-relationalpatch)
- [Writes that depend on the row: `relational.Mutate`](#writes-that-depend-on-the-row-relationalmutate)
- [Locking a row yourself: `relational.LockRow`](#locking-a-row-yourself-relationallockrow)
- [What these tools do not cover](#what-these-tools-do-not-cover)
- [Running more than one instance](#running-more-than-one-instance)

## The problem

A model's `Update` usually writes the whole row:

```go
_, err = db.NewUpdate().Model(org).WherePK().Exec(ctx)
```

A handler loads the row, changes a field on the Go struct and calls `Update`. Between the load and
the write, the struct is a copy that can go stale:

1. Request A loads the organization and changes its name.
2. Request B loads the same organization and changes its phone.
3. A saves: new name, old phone.
4. B saves its copy: **old name**, new phone.

A's change is gone and nobody got an error. PostgreSQL made the two `UPDATE` statements run one
after the other, as it always does for one row, but each statement wrote every column from a copy
read earlier.

This needs two requests at the same time, not two servers. One instance serves requests
concurrently, so it happens there too; a second instance only makes it more likely.

There are two kinds of this problem, with different fixes:

- **Independent fields.** A and B changed different fields, and neither change depended on the
  other. Writing only the columns each request sent fixes it. No lock is needed.
- **Writes that depend on the row.** The new value, or whether the write is allowed at all, was
  decided in Go from the row's current values: close a ticket only if it is open, add to a
  balance, keep `starts_at` before `ends_at` when one request changes each. Writing fewer columns
  does not help, because the decision was made from a stale copy. The row must be locked and read
  again in the same transaction as the write.

## Choosing the tool

| The write | Use |
| --- | --- |
| A PATCH of fields that do not depend on each other | `relational.Patch` |
| The new value or its validity depends on the row's current values | `relational.Mutate` |
| Inserting a child row that is only valid while its parent is in some state | `relational.LockRow` on the parent, then the insert, in one transaction |
| A counter or a timestamp that only moves forward | One SQL statement: `SET count = count + 1`, or `SET last_active_at = ? WHERE last_active_at < ?` |
| "No two rows with the same value" | A unique index. A check before the insert does not hold under concurrent requests |

The rule behind the table: a write whose values come from a read must be in the same transaction
as that read, with the row locked when it was read. Locking after an earlier, unlocked read
protects nothing, because the stale values are already in the struct.

## Independent fields: `relational.Patch`

`types.Optional[T]` in a request body records whether the client sent each key. `Patch` collects
the columns of the keys that were sent and writes only those.

```go
type UpdateRequest struct {
	Name  types.Optional[string] `json:"name" minLength:"1"`
	Phone types.Optional[string] `json:"phone"`
}
```

```go
org, err := organization.Get(ctx, db, id) // a plain load, no lock
if err != nil {
	return nil, ez.Wrap(err)
}

var patch relational.Patch
relational.SetColumn(&patch, "name", &org.Name, body.Name)
relational.SetNullableColumn(&patch, "phone", &org.Phone, body.Phone)

// Only when the body changed something. Update is called either way: it is
// what returns the error of a null sent for name
if len(patch.Columns()) > 0 {
	org.UpdatedAt = time.Now().UTC()
	patch.Add("updated_at")
}

err = patch.Update(ctx, db, org)
if err != nil {
	return nil, ez.Wrap(err)
}
```

- **`SetColumn`** is for a column that cannot be null. An absent key does nothing. A value is
  copied into the model field and the column is recorded. A `null` is kept as an `EINVALID` error,
  whose data names the column, which `Update` returns before it writes anything.
- **`SetNullableColumn`** is for a nullable column held as a pointer. A `null` sets the field to
  nil, so the column is written as `NULL`.
- **`Add`** records a column the handler set itself: `updated_at`, a derived column, or a field
  whose type differs from the request's. For those, read the request field with `Get()`, convert
  it, assign it and call `Add`. A request that still uses pointer fields, where nil means the key
  was not sent, does the same inside `if body.Name != nil`: assign `*body.Name`, then
  `patch.Add("name")`.
- **`Update`** calls the model's `Validate()` and then runs one `UPDATE` of the recorded columns
  by primary key. With no column recorded it returns nil and does neither. A row that was deleted
  since the load is `ENOTFOUND`.
- **The primary key is not patchable.** The `UPDATE` finds the row by the key in the model, so a
  key changed there would write another row. A recorded primary-key column, any column of a
  composite key included, is an `EINTERNAL` error whose data names the column, and nothing is
  written. To change a key, run an `UPDATE` that sets it and finds the row by the old key.

The model's own `Update` method is not called, so anything it does besides the write, such as
setting `UpdatedAt`, is the handler's job, as in the example.

`Validate()` runs on the loaded copy with the sent fields applied. The fields that were not sent
are as old as the load. A rule that involves only the sent fields is checked correctly. A rule
across two fields that two requests could each change is not: use `Mutate` for that write.

## Writes that depend on the row: `relational.Mutate`

`Mutate` opens a transaction, locks the row and loads its current values into the model, runs your
function, then calls the model's `Update`. A second request for the same row waits at the lock,
then loads what the first one wrote.

```go
ticket := &tasks.Ticket{ID: id}

err := relational.Mutate(ctx, db, ticket, func(ctx context.Context, tx bun.Tx) error {
	if ticket.Status != tasks.StatusOpen {
		return ez.New(ez.ECONFLICT, "The ticket is already closed", nil)
	}

	ticket.Status = tasks.StatusClosed
	ticket.ClosedBy = userID

	return nil
})
```

- **Set fields inside the function.** Only the primary key of the model you pass in is used:
  every column is replaced by the stored row before the function runs.
- **Write through `tx`.** Other rows the function inserts or updates through `tx` are saved or
  rolled back together with the model. An error from the function, or from the model's `Update`,
  rolls everything back and is returned with its code.
- **Keep side effects out.** Sending an email or calling another service cannot be rolled back.
  Do it after `Mutate` returns nil.
- **Keep the function short.** Other requests for the same row wait until it returns. Do not call
  a slow outside service from inside it.
- **Lock in one order.** If the function locks other rows with `LockRow`, every code path must
  lock them in the same order; `Mutate` locks its own row before the function runs. Two requests
  that lock the same two rows in opposite orders deadlock, and PostgreSQL ends the deadlock by
  failing one of them.
- **Inside a transaction.** When `db` is already a `bun.Tx`, `Mutate` runs in a savepoint of it,
  and the caller's commit or rollback decides the outcome.

The lock only makes other writers of that row wait. Plain reads never wait, and requests for other
rows are not affected. The model's full-row `Update` is safe here because the row was read under
the lock.

On PostgreSQL the lock is `FOR NO KEY UPDATE`, which does not block inserts of rows that reference
this one by foreign key. SQLite has no row locks, so `Mutate` only loads the row there, and on a
file database a second transaction that loaded the row meanwhile fails its write with a
`database is locked` error instead of waiting.

## Locking a row yourself: `relational.LockRow`

`LockRow` is the first step of `Mutate` on its own: it locks the row until the transaction ends
and loads it. Use it when the locked row is not the one being written, for example a parent whose
state decides whether a child may be inserted.

```go
err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
	ticket := &tasks.Ticket{ID: ticketID}

	err := relational.LockRow(ctx, tx, ticket)
	if err != nil {
		return ez.Wrap(err)
	}

	if ticket.Status == tasks.StatusClosed {
		return ez.New(ez.ECONFLICT, "The ticket is closed", nil)
	}

	return comment.Insert(ctx, tx)
})
```

It takes a `bun.Tx`, not a `bun.IDB`: a lock taken outside a transaction is released as soon as
the statement ends, so the signature refuses that call.

## What these tools do not cover

- **A form that was open for a while.** A user opens an edit form, another user saves the same
  record, and the first user then saves the old values they still see. Each request is correct on
  its own, so neither a lock nor `Patch` detects it. With `Patch` only the fields that form sent
  are overwritten. Detecting it needs a version number the client sends back: a `revision` column
  and `UPDATE ... WHERE id = ? AND revision = ?`, as `components/auth/session`'s bun store does
  for sessions.
- **Rules across several rows with no single parent.** "At most three active rules per unit" needs
  the unit row locked before counting, or a constraint in the database.
- **Deadlocks and long waits.** A deadlock comes back as the driver's error, `EINTERNAL`. A request
  that waits on a lock longer than the pool's `statement_timeout` fails. Nothing is retried.

## Running more than one instance

The tools above protect the data in the database whatever the number of instances. A second
instance changes these other things:

- **Migrations** take a lock, so instances that boot together run each migration once. See
  [migrations](migrations.md).
- **`components/scheduler`** runs its jobs in the process that registered them. Two instances
  each run every job, unless each Scheduler is given a claimer (`scheduler.WithClaimer`), which
  runs each boundary of a recurring job on one instance. A job id excludes other runs of the id
  in its own process only, unless each Scheduler is given a leaser (`scheduler.WithLeaser`),
  which makes the id exclusive across the instances. See [scheduler](scheduler.md).
- **`components/ratelimit`** counts in memory, so each instance has its own counters and a client
  gets the limit once per instance: with N instances, up to N times the limit. A limit that has
  to hold across instances, such as attempts at guessing a code, belongs in the database, counted
  per target.
- **`session.NewMemoryStore`** keeps sessions in the process. Use the bun store when there is more
  than one instance.
