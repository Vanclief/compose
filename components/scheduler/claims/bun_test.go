package claims

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/vanclief/compose/components/scheduler"
	"github.com/vanclief/compose/drivers/databases/relational/postgres"
	"github.com/vanclief/compose/drivers/databases/relational/postgres/pgtest"
	"github.com/vanclief/ez"
)

const testTable = "scheduler_claims_test"

// hour is the lease of tests that do not wait for one to expire.
const hour = time.Hour

// newTestDB creates a database for the test with newEmptyTestDB and creates
// the claims table in it.
func newTestDB(t *testing.T) *bun.DB {
	db := newEmptyTestDB(t)
	err := CreateTable(context.Background(), db, testTable)
	require.NoError(t, err)
	return db
}

// newEmptyTestDB creates a database for the test with pgtest and connects to
// it, dropping the database when the test ends. It skips when PostgreSQL is
// unreachable, unless COMPOSE_TEST_POSTGRES is set.
func newEmptyTestDB(t *testing.T) *bun.DB {
	cfg := postgres.ConnectionConfig{
		Username: "postgres",
		Host:     "localhost:5432",
		Database: "postgres",
	}
	admin, err := postgres.ConnectToDatabase(&cfg)
	if err != nil {
		if os.Getenv("COMPOSE_TEST_POSTGRES") != "" {
			t.Fatalf("PostgreSQL testing is enabled but the database is unavailable: %v", err)
		}
		t.Skipf("PostgreSQL is not available: %v", err)
	}
	admin.Close() // nolint:errcheck

	cfg.Database = "compose_claims"
	created, err := pgtest.Create(cfg)
	require.NoError(t, err)
	db, err := postgres.ConnectToDatabase(&created)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.Close() // nolint:errcheck
		err := pgtest.Drop(created)
		assert.NoError(t, err)
	})
	return db.DB
}

var tick0 = time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)

// tick returns the n-th minute boundary after tick0.
func tick(n int) time.Time {
	return tick0.Add(time.Duration(n) * time.Minute)
}

// queryLog records the SQL of every query run on a bun database after it is
// added as a hook.
type queryLog struct {
	mu      sync.Mutex
	queries []string
}

func (l *queryLog) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	return ctx
}

func (l *queryLog) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queries = append(l.queries, event.Query)
}

func (l *queryLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.queries...)
}

// cancelBeforeQuery cancels a context as the next query starts, then records
// the state of the context that query runs on. It does nothing once it fired.
type cancelBeforeQuery struct {
	cancel      context.CancelFunc
	err         error
	deadline    time.Time
	hasDeadline bool
}

func (h *cancelBeforeQuery) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	if h.cancel == nil {
		return ctx
	}
	h.cancel()
	h.cancel = nil
	h.err = ctx.Err()
	h.deadline, h.hasDeadline = ctx.Deadline()
	return ctx
}

func (h *cancelBeforeQuery) AfterQuery(ctx context.Context, event *bun.QueryEvent) {}

// rowCount returns how many rows of the test table have job id.
func rowCount(t *testing.T, db *bun.DB, id string) int64 {
	t.Helper()
	count, err := db.NewSelect().
		TableExpr("?", bun.Ident(testTable)).
		Where("job_id = ?", id).
		Count(context.Background())
	require.NoError(t, err)
	return count
}

func claim(t *testing.T, c scheduler.Claimer, id string, at time.Time, lease time.Duration) bool {
	t.Helper()
	won, err := c.Claim(context.Background(), id, at, lease)
	require.NoError(t, err)
	return won
}

func release(t *testing.T, c scheduler.Claimer, id string, at time.Time) {
	t.Helper()
	err := c.Release(context.Background(), id, at)
	require.NoError(t, err)
}

func TestClaimsPostgres(t *testing.T) {
	db := newTestDB(t)

	// Each subtest uses its own job ids, so they share the table.
	t.Run("racing claimers win a tick exactly once", func(t *testing.T) {
		// Two stores over the same table stand for two instances.
		claimers := []scheduler.Claimer{NewBunStore(db, testTable), NewBunStore(db, testTable)}
		const rounds = 50
		const perClaimer = 4

		// Round 0 races the insert of a new row, later rounds race the
		// takeover of a released one.
		for round := 0; round < rounds; round++ {
			var wg sync.WaitGroup
			results := make(chan bool, len(claimers)*perClaimer)
			for _, c := range claimers {
				for i := 0; i < perClaimer; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						won, err := c.Claim(context.Background(), "race", tick(round), hour)
						assert.NoError(t, err)
						results <- won
					}()
				}
			}
			wg.Wait()
			close(results)

			won := 0
			for r := range results {
				if r {
					won++
				}
			}
			require.Equal(t, 1, won, "round %d", round)
			release(t, claimers[0], "race", tick(round))
		}
	})

	t.Run("old or equal tick is refused", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		require.True(t, claim(t, c, "late", tick(5), hour))
		release(t, c, "late", tick(5))

		assert.False(t, claim(t, c, "late", tick(5), hour), "equal tick")
		assert.False(t, claim(t, c, "late", tick(4), hour), "older tick")
		assert.True(t, claim(t, c, "late", tick(6), hour), "newer tick")
	})

	t.Run("newer tick refused while busy and won after release", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		require.True(t, claim(t, c, "busy", tick(0), hour))

		assert.False(t, claim(t, c, "busy", tick(1), hour))
		release(t, c, "busy", tick(0))
		assert.True(t, claim(t, c, "busy", tick(2), hour))
	})

	t.Run("newer tick won after the lease expires without release", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		require.True(t, claim(t, c, "lease", tick(0), 500*time.Millisecond))
		assert.False(t, claim(t, c, "lease", tick(1), hour))

		require.Eventually(t, func() bool {
			return claim(t, c, "lease", tick(2), hour)
		}, 5*time.Second, 20*time.Millisecond)
	})

	t.Run("release of an older tick does not free a newer claim", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		require.True(t, claim(t, c, "stale", tick(0), 100*time.Millisecond))
		require.Eventually(t, func() bool {
			return claim(t, c, "stale", tick(1), hour)
		}, 5*time.Second, 20*time.Millisecond)

		// The instance that held tick 0 releases it late.
		release(t, c, "stale", tick(0))
		assert.False(t, claim(t, c, "stale", tick(2), hour), "tick 1 is still busy")

		release(t, c, "stale", tick(1))
		assert.True(t, claim(t, c, "stale", tick(3), hour))
	})

	t.Run("job ids do not affect each other", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		require.True(t, claim(t, c, "job-a", tick(10), hour))

		assert.True(t, claim(t, c, "job-b", tick(0), hour), "older tick of another id")
		assert.True(t, claim(t, c, "job-c", tick(10), hour), "same tick of another id")
		release(t, c, "job-b", tick(0))
		assert.False(t, claim(t, c, "job-a", tick(11), hour), "job-a is still busy")
	})

	t.Run("one row per job id", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		for i := 0; i < 5; i++ {
			require.True(t, claim(t, c, "rows", tick(i), hour))
			release(t, c, "rows", tick(i))
		}

		assert.Equal(t, int64(1), rowCount(t, db, "rows"))
	})

	t.Run("claim and release are one statement each", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		log := &queryLog{}
		db.AddQueryHook(log)

		require.True(t, claim(t, c, "sql", tick(0), hour))
		release(t, c, "sql", tick(0))

		queries := log.get()
		require.Len(t, queries, 2)
		assert.Contains(t, queries[0], `INSERT INTO "`+testTable+`" AS claim`)
		assert.Contains(t, queries[0], `WHERE claim.tick < EXCLUDED.tick AND (claim.busy_until IS NULL OR claim.busy_until <= now())`)
		assert.Contains(t, queries[1], `UPDATE "`+testTable+`" SET busy_until = NULL WHERE (job_id = 'sql') AND (tick = `)
	})

	t.Run("tick ahead of the database clock is refused", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		var dbNow time.Time
		err := db.QueryRowContext(context.Background(), "SELECT now()").Scan(&dbNow)
		require.NoError(t, err)
		dayAhead := dbNow.Add(24 * time.Hour).Truncate(time.Minute)

		// A new id: nothing is inserted.
		_, err = c.Claim(context.Background(), "ahead-new", dayAhead, hour)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		assert.Contains(t, ez.ErrorMessage(err), "ahead of the database clock")
		assert.Equal(t, int64(0), rowCount(t, db, "ahead-new"))

		// An existing, released row: it keeps its tick.
		require.True(t, claim(t, c, "ahead-old", tick(0), hour))
		release(t, c, "ahead-old", tick(0))
		_, err = c.Claim(context.Background(), "ahead-old", dayAhead, hour)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		var stored time.Time
		err = db.NewSelect().
			TableExpr("?", bun.Ident(testTable)).
			Column("tick").
			Where("job_id = ?", "ahead-old").
			Scan(context.Background(), &stored)
		require.NoError(t, err)
		assert.True(t, stored.Equal(tick(0)), "stored tick %s", stored)

		// Within the tolerance: won.
		nextMinute := dbNow.Truncate(time.Minute).Add(time.Minute)
		assert.True(t, claim(t, c, "ahead-old", nextMinute, hour))
	})

	t.Run("largest lease is stored and blocks", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		require.True(t, claim(t, c, "forever", tick(0), time.Duration(math.MaxInt64)))
		assert.False(t, claim(t, c, "forever", tick(1), hour), "still busy")

		var busyUntil time.Time
		err := db.NewSelect().
			TableExpr("?", bun.Ident(testTable)).
			Column("busy_until").
			Where("job_id = ?", "forever").
			Scan(context.Background(), &busyUntil)
		require.NoError(t, err)
		assert.True(t, busyUntil.After(time.Now().Add(200*365*24*time.Hour)), "busy_until %s", busyUntil)
	})

	t.Run("claim cancelled while it waits for the row is still answered", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		ctx := context.Background()
		require.True(t, claim(t, c, "cancelled", tick(0), hour))
		release(t, c, "cancelled", tick(0))

		// database/sql drops the answer of a cancelled statement only some of
		// the time, so the claim is cancelled over several rounds.
		for round := 1; round <= 15; round++ {
			holder, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			_, err = holder.ExecContext(ctx, "SELECT 1 FROM ? WHERE job_id = ? FOR UPDATE", bun.Ident(testTable), "cancelled")
			require.NoError(t, err)

			claimCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			type answer struct {
				won bool
				err error
			}
			answered := make(chan answer, 1)
			go func() {
				won, err := c.Claim(claimCtx, "cancelled", tick(round), hour)
				answered <- answer{won: won, err: err}
			}()

			// The claim waits for the row lock: cancel it there, then let
			// the statement commit.
			time.Sleep(100 * time.Millisecond)
			cancel()
			time.Sleep(20 * time.Millisecond)
			require.NoError(t, holder.Commit())

			got := <-answered
			require.NoError(t, got.err, "round %d", round)
			require.True(t, got.won, "round %d", round)
			release(t, c, "cancelled", tick(round))
		}
	})

	t.Run("claim statement keeps the deadline of its context but not the cancellation", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		deadline := time.Now().Add(10 * time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		hook := &cancelBeforeQuery{cancel: cancel}
		db.AddQueryHook(hook)

		won, err := c.Claim(ctx, "detached", tick(0), hour)
		require.NoError(t, err)
		assert.True(t, won)
		assert.Error(t, ctx.Err(), "the caller's context was cancelled as the statement started")
		assert.NoError(t, hook.err, "the statement's context is not cancelled")
		assert.True(t, hook.hasDeadline)
		assert.True(t, hook.deadline.Equal(deadline), "deadline %s", hook.deadline)
	})

	t.Run("claim on a cancelled context writes nothing", func(t *testing.T) {
		c := NewBunStore(db, testTable)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		won, err := c.Claim(ctx, "done", tick(0), hour)
		assert.False(t, won)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		assert.Zero(t, rowCount(t, db, "done"))
	})

	t.Run("CreateTable is idempotent", func(t *testing.T) {
		err := CreateTable(context.Background(), db, testTable)
		assert.NoError(t, err)
	})

	t.Run("missing table is an internal error", func(t *testing.T) {
		c := NewBunStore(db, "no_such_table")
		_, err := c.Claim(context.Background(), "a", tick(0), hour)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		err = c.Release(context.Background(), "a", tick(0))
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	})
}

// TestCreateTableConcurrently: instances booting together against a new
// database all create the table without an error.
func TestCreateTableConcurrently(t *testing.T) {
	db := newEmptyTestDB(t)

	const rounds = 10
	const instances = 8
	for round := 0; round < rounds; round++ {
		table := fmt.Sprintf("claims_boot_%d", round)
		var wg sync.WaitGroup
		errs := make(chan error, instances)
		for i := 0; i < instances; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- CreateTable(context.Background(), db, table)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "round %d", round)
		}
	}
}
