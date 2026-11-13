package claims

import (
	"context"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/vanclief/compose/components/scheduler"
	"github.com/vanclief/ez"
)

// bunLeaser keeps leases in one table of a PostgreSQL bun database.
type bunLeaser struct {
	db    *bun.DB
	table string
}

// leaseRecord is the row shape of the leases table. The table name is set per
// query from the table given to NewBunLeaser. owner holds the lease until
// held_until, and superseded marks a lease that an acquire with supersede was
// refused on.
type leaseRecord struct {
	bun.BaseModel `bun:"table:scheduler_leases"`

	JobID      string    `bun:"job_id,pk,notnull,type:text"`
	Owner      string    `bun:"owner,notnull,type:text"`
	HeldUntil  time.Time `bun:"held_until,notnull,type:timestamptz"`
	Superseded bool      `bun:"superseded,notnull,type:boolean,default:false"`
}

// NewBunLeaser returns a scheduler.Leaser backed by one PostgreSQL table with
// the columns job_id, owner, held_until and superseded. Call CreateLeaseTable
// with the same table at startup to create it, and pass the leaser to
// scheduler.WithLeaser on every instance.
//
// Each of Acquire, Renew and Release is one statement in its own implicit
// transaction, and reads the time from the database's now(), so it works
// through a pooler in transaction mode, and instances whose clocks disagree
// agree on whether a lease is live. Release also deletes the rows whose lease
// lapsed more than an hour ago, left by instances that stopped without
// releasing for ids not used since.
//
// The statements are written for the default READ COMMITTED isolation only.
// Under a database or role default of REPEATABLE READ or SERIALIZABLE,
// Acquire, Renew and Release can all fail with a serialization error when
// acquires race the holder: a contended Acquire fails instead of being
// refused, a failed Renew fails the instance's whole batch, and a failed
// Release leaves the id blocked until the lease lapses. Two runs of an id
// still never hold it together.
//
// The job id is the table's primary key, so it must fit in a btree index
// entry, about 2.7 KB after PostgreSQL's compression, and must not contain a
// NUL byte, which PostgreSQL's text refuses. Every Acquire of an id that
// breaks either limit fails.
func NewBunLeaser(db *bun.DB, table string) scheduler.Leaser {
	return &bunLeaser{db: db, table: table}
}

// CreateLeaseTable creates the table of NewBunLeaser when it does not exist.
// Every instance can call it at startup: like CreateTable, it holds a
// transaction-level advisory lock while it creates the table. The role needs
// CREATE on the schema even when the table exists.
func CreateLeaseTable(ctx context.Context, db *bun.DB, table string) error {
	err := db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		_, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext(?))", createTableLock)
		if err != nil {
			return err
		}
		_, err = tx.NewCreateTable().
			Model((*leaseRecord)(nil)).
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

// Acquire inserts the row of id for owner, or takes it over when its lease is
// owner's or has lapsed, in one upsert, and reports whether the row is now
// owner's. A takeover from another owner clears superseded, and a retry by the
// same owner keeps it. When the row is another owner's live lease the upsert
// changes nothing but, when supersede is true, sets superseded on it.
// Concurrent acquires of one id are serialized on its row by PostgreSQL, and
// at READ COMMITTED each re-checks the condition against the row the one
// before it wrote.
//
// A ctx that is already cancelled is an error and nothing is written. After
// that the statement runs on ctx's deadline alone, as Claim does, so an
// acquire cancelled on the way, by a Supersede or a shutdown on this
// instance, is still answered and can be won; the Scheduler then runs the job
// queued in place of the cancelled one on that lease, or releases it.
//
// An acquire that runs out of its deadline after the statement was sent can
// still commit with its answer lost. A RunNext or Supersede that asks again
// does so with the same owner and wins that lease. A RunOnce or recurring
// run, which asks once, releases the lease at once, and a RunNext or
// Supersede that had such a failure releases it when it gives up; when that
// release reaches the database before the commit it ends nothing: the lease
// is then held by no run until its ttl passes.
func (l *bunLeaser) Acquire(ctx context.Context, id, owner string, ttl time.Duration, supersede bool) (bool, error) {
	err := ctx.Err()
	if err != nil {
		return false, ez.New(ez.EINTERNAL, "claims: acquiring the job lease failed", err)
	}

	ctx, cancel := withoutCancel(ctx)
	defer cancel()

	var won bool
	err = l.db.QueryRowContext(ctx, `WITH acquired AS (
	INSERT INTO ? AS lease (job_id, owner, held_until, superseded)
	VALUES (?::text, ?::text, now() + ? * interval '1 microsecond', false)
	ON CONFLICT (job_id) DO UPDATE SET
		owner = CASE WHEN lease.owner = EXCLUDED.owner OR lease.held_until <= now() THEN EXCLUDED.owner ELSE lease.owner END,
		held_until = CASE WHEN lease.owner = EXCLUDED.owner OR lease.held_until <= now() THEN EXCLUDED.held_until ELSE lease.held_until END,
		superseded = CASE WHEN lease.owner = EXCLUDED.owner THEN lease.superseded WHEN lease.held_until <= now() THEN false ELSE true END
	WHERE lease.owner = EXCLUDED.owner OR lease.held_until <= now() OR ?::boolean
	RETURNING lease.owner
)
SELECT EXISTS (SELECT 1 FROM acquired WHERE owner = ?::text)`,
		bun.Ident(l.table), id, owner, ttl.Microseconds(), supersede, owner).Scan(&won)
	if err != nil {
		return false, ez.New(ez.EINTERNAL, "claims: acquiring the job lease failed", err)
	}
	return won, nil
}

// Renew extends, in one statement, every lease of leases whose row still
// names its owner, and returns those with their superseded mark. An empty
// leases makes no query.
func (l *bunLeaser) Renew(ctx context.Context, leases []scheduler.Lease, ttl time.Duration) ([]scheduler.Lease, error) {
	if len(leases) == 0 {
		return nil, nil
	}

	values := make([]string, 0, len(leases))
	args := make([]any, 0, 2+2*len(leases))
	args = append(args, bun.Ident(l.table), ttl.Microseconds())
	for _, lease := range leases {
		values = append(values, "(?::text, ?::text)")
		args = append(args, lease.ID, lease.Owner)
	}

	rows, err := l.db.QueryContext(ctx, `UPDATE ? AS lease SET held_until = now() + ? * interval '1 microsecond'
FROM (VALUES `+strings.Join(values, ", ")+`) AS v (job_id, owner)
WHERE lease.job_id = v.job_id AND lease.owner = v.owner
RETURNING lease.job_id, lease.owner, lease.superseded`, args...)
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "claims: renewing the job leases failed", err)
	}
	defer rows.Close() // nolint:errcheck // Next closes the rows when they end, and on an early return the scan error is the one returned

	renewed := make([]scheduler.Lease, 0, len(leases))
	for rows.Next() {
		var lease scheduler.Lease
		err = rows.Scan(&lease.ID, &lease.Owner, &lease.Superseded)
		if err != nil {
			return nil, ez.New(ez.EINTERNAL, "claims: renewing the job leases failed", err)
		}
		renewed = append(renewed, lease)
	}
	err = rows.Err()
	if err != nil {
		return nil, ez.New(ez.EINTERNAL, "claims: renewing the job leases failed", err)
	}
	return renewed, nil
}

// Release deletes the row of id while it is owner's, so a late release cannot
// end a lease another owner took since, and in the same statement deletes
// every row whose lease lapsed more than an hour ago.
func (l *bunLeaser) Release(ctx context.Context, id, owner string) error {
	_, err := l.db.NewDelete().
		TableExpr("?", bun.Ident(l.table)).
		Where("(job_id = ? AND owner = ?)", id, owner).
		WhereOr("held_until < now() - interval '1 hour'").
		Exec(ctx)
	if err != nil {
		return ez.New(ez.EINTERNAL, "claims: releasing the job lease failed", err)
	}
	return nil
}
