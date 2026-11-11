// Package claims keeps the claims of a scheduler.Scheduler in a PostgreSQL
// table, so that an app running several instances runs each minute boundary
// of a recurring job on one of them. It works on PostgreSQL only.
//
// The table has one row per job id, so it does not grow with time and needs
// no cleanup. A claim is one statement: an upsert that takes the row only
// when its stored tick is older than the claimed one and its job is not busy.
// "Now" and the busy deadline are computed by the database, so instances
// whose clocks disagree still agree on whether a claim is busy, and a tick
// more than a minute ahead of the database clock is refused with an error.
package claims

import (
	"context"
	"time"

	"github.com/uptrace/bun"
	"github.com/vanclief/compose/components/scheduler"
	"github.com/vanclief/ez"
)

// maxTickAhead is how far ahead of the database clock a claimed tick may be.
// A later tick means the clock of the instance or of the database is wrong:
// stored, it would make every instance's claims older than it, so it is
// refused.
const maxTickAhead = time.Minute

// createTableLock names the transaction-level advisory lock CreateTable takes,
// so instances booting together create the table one at a time.
const createTableLock = "compose/scheduler/claims.CreateTable"

// bunStore keeps claims in one table of a PostgreSQL bun database.
type bunStore struct {
	db    *bun.DB
	table string
}

// bunRecord is the row shape. The table name is set per query from the table
// given to NewBunStore. tick is the latest boundary claimed for the job and
// busy_until is when that claim stops blocking later boundaries, NULL once it
// is released.
type bunRecord struct {
	bun.BaseModel `bun:"table:scheduler_claims"`

	JobID     string    `bun:"job_id,pk,notnull,type:text"`
	Tick      time.Time `bun:"tick,notnull,type:timestamptz"`
	BusyUntil time.Time `bun:"busy_until,nullzero,type:timestamptz"`
}

// NewBunStore returns a scheduler.Claimer backed by one PostgreSQL table with
// the columns job_id, tick and busy_until. Call CreateTable with the same
// table at startup to create it, and pass the store to scheduler.WithClaimer
// on every instance.
//
// For one job id it grants a tick to exactly one claim, and refuses a tick
// older than or equal to the last one claimed, so an instance that asks late
// cannot run a tick again. While a claim is busy, from the claim until it is
// released or its lease expires, it refuses later ticks too. A tick more than
// a minute ahead of the database clock is an ez.EINVALID error and is not
// stored.
func NewBunStore(db *bun.DB, table string) scheduler.Claimer {
	return &bunStore{db: db, table: table}
}

// CreateTable creates the table of NewBunStore when it does not exist. Every
// instance can call it at startup: it holds a transaction-level advisory lock
// while it creates the table, since concurrent CREATE TABLE IF NOT EXISTS
// statements can fail on PostgreSQL's catalog. The role needs CREATE on the
// schema even when the table exists.
func CreateTable(ctx context.Context, db *bun.DB, table string) error {
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext(?))", createTableLock)
		if err != nil {
			return err
		}
		_, err = tx.NewCreateTable().
			Model((*bunRecord)(nil)).
			ModelTableExpr("?", bun.Ident(table)).
			IfNotExists().
			Exec(ctx)
		return err
	})
	if err != nil {
		return ez.New(ez.EINTERNAL, "claims: creating the "+table+" table failed", err)
	}
	return nil
}

// Claim inserts the row of id, or takes it over when its tick is older than
// tick and it is not busy, in one statement, and reports whether a row was
// written. Concurrent claims on one row are serialized by PostgreSQL, and the
// later one re-checks the condition against the row the earlier one wrote. A
// tick more than maxTickAhead ahead of the database clock writes nothing, on
// the insert and the takeover alike, and is returned as an error.
//
// A ctx that is already cancelled is an error and nothing is written. After
// that the statement runs on ctx's deadline alone: bun's pgdriver does not
// stop a statement it has sent when ctx is cancelled, only at its deadline,
// while database/sql can drop the answer of a cancelled ctx, which would
// report a committed claim as failed. So a claim cancelled on the way is
// still answered, and can be won.
func (s *bunStore) Claim(ctx context.Context, id string, tick time.Time, lease time.Duration) (bool, error) {
	err := ctx.Err()
	if err != nil {
		return false, ez.New(ez.EINTERNAL, "claims: claiming the job failed", err)
	}

	ctx, cancel := withoutCancel(ctx)
	defer cancel()

	var ahead, won bool
	err = s.db.QueryRowContext(ctx, `WITH request AS (
	SELECT r.job_id, r.tick, r.tick > now() + ? * interval '1 microsecond' AS ahead
	FROM (VALUES (?::text, ?::timestamptz)) AS r (job_id, tick)
), claimed AS (
	INSERT INTO ? AS claim (job_id, tick, busy_until)
	SELECT job_id, tick, now() + ? * interval '1 microsecond' FROM request WHERE NOT ahead
	ON CONFLICT (job_id) DO UPDATE SET tick = EXCLUDED.tick, busy_until = EXCLUDED.busy_until
	WHERE claim.tick < EXCLUDED.tick AND (claim.busy_until IS NULL OR claim.busy_until <= now())
	RETURNING 1
)
SELECT request.ahead, EXISTS (SELECT 1 FROM claimed) FROM request`,
		maxTickAhead.Microseconds(), id, tick, bun.Ident(s.table), lease.Microseconds()).Scan(&ahead, &won)
	if err != nil {
		return false, ez.New(ez.EINTERNAL, "claims: claiming the job failed", err)
	}
	if ahead {
		return false, ez.New(ez.EINVALID, "claims: the tick is more than a minute ahead of the database clock, the clock of this instance or of the database is wrong", nil)
	}
	return won, nil
}

// withoutCancel returns a context that keeps ctx's values and deadline but
// not its cancellation.
func withoutCancel(ctx context.Context) (context.Context, context.CancelFunc) {
	detached := context.WithoutCancel(ctx)

	deadline, ok := ctx.Deadline()
	if !ok {
		return detached, func() {}
	}

	return context.WithDeadline(detached, deadline)
}

// Release clears the busy mark of id only while the row still holds tick, so
// a release that comes after a newer claim leaves that claim busy.
func (s *bunStore) Release(ctx context.Context, id string, tick time.Time) error {
	_, err := s.db.NewUpdate().
		TableExpr("?", bun.Ident(s.table)).
		Set("busy_until = NULL").
		Where("job_id = ?", id).
		Where("tick = ?", tick).
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "claims: releasing the job failed", err)
	}
	return nil
}
