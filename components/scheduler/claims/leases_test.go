package claims

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/vanclief/compose/components/scheduler"
	"github.com/vanclief/ez"
)

const leaseTable = "scheduler_leases_test"

// newLeaseTestDB creates a database for the test with newEmptyTestDB and
// creates the leases table in it.
func newLeaseTestDB(t *testing.T) *bun.DB {
	db := newEmptyTestDB(t)
	err := CreateLeaseTable(context.Background(), db, leaseTable)
	require.NoError(t, err)
	return db
}

func acquire(t *testing.T, l scheduler.Leaser, id, owner string, ttl time.Duration, supersede bool) bool {
	t.Helper()
	won, err := l.Acquire(context.Background(), id, owner, ttl, supersede)
	require.NoError(t, err)
	return won
}

func renew(t *testing.T, l scheduler.Leaser, leases ...scheduler.Lease) []scheduler.Lease {
	t.Helper()
	renewed, err := l.Renew(context.Background(), leases, hour)
	require.NoError(t, err)
	return renewed
}

// marked reports whether owner's lease on id is renewed and marked
// superseded, failing the test when it is not renewed.
func marked(t *testing.T, l scheduler.Leaser, id, owner string) bool {
	t.Helper()
	renewed := renew(t, l, scheduler.Lease{ID: id, Owner: owner})
	require.Len(t, renewed, 1)
	return renewed[0].Superseded
}

// leaseRow returns the owner and superseded mark of id's row.
func leaseRow(t *testing.T, db *bun.DB, id string) (string, bool) {
	t.Helper()
	var owner string
	var superseded bool
	err := db.QueryRowContext(context.Background(), "SELECT owner, superseded FROM ? WHERE job_id = ?", bun.Ident(leaseTable), id).Scan(&owner, &superseded)
	require.NoError(t, err)
	return owner, superseded
}

// lapse makes the lease of id lapse, as if its ttl had passed.
func lapse(t *testing.T, db *bun.DB, id string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), "UPDATE ? SET held_until = now() - interval '1 second' WHERE job_id = ?", bun.Ident(leaseTable), id)
	require.NoError(t, err)
}

func leaseRowCount(t *testing.T, db *bun.DB, id string) int64 {
	t.Helper()
	count, err := db.NewSelect().
		TableExpr("?", bun.Ident(leaseTable)).
		Where("job_id = ?", id).
		Count(context.Background())
	require.NoError(t, err)
	return count
}

func TestLeasesPostgres(t *testing.T) {
	db := newLeaseTestDB(t)

	// Each subtest uses its own job ids, so they share the table.
	t.Run("acquire is exclusive", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)
		require.True(t, acquire(t, l, "excl", "a/1", hour, false))
		assert.False(t, acquire(t, l, "excl", "b/1", hour, false))
		assert.True(t, acquire(t, l, "excl", "a/1", hour, false), "a retry by the owner")
		assert.False(t, acquire(t, l, "excl", "b/1", hour, true))

		owner, _ := leaseRow(t, db, "excl")
		assert.Equal(t, "a/1", owner)
	})

	t.Run("racing acquirers win exactly once", func(t *testing.T) {
		// Two leasers over the same table stand for two instances.
		leasers := []scheduler.Leaser{NewBunLeaser(db, leaseTable), NewBunLeaser(db, leaseTable)}
		const rounds = 50
		const acquirers = 8

		// Round 0 races the insert of a new row, later rounds the takeover
		// of a lapsed one: the row is made to lapse between rounds. Half the
		// acquirers ask to supersede.
		for round := 0; round < rounds; round++ {
			var wg sync.WaitGroup
			results := make(chan bool, acquirers)
			for i := 0; i < acquirers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					owner := fmt.Sprintf("owner/%d/%d", round, i)
					won, err := leasers[i%2].Acquire(context.Background(), "race", owner, hour, i%2 == 1)
					assert.NoError(t, err)
					results <- won
				}()
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
			lapse(t, db, "race")
		}
	})

	t.Run("a retry by the owner keeps the superseded mark", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)
		require.True(t, acquire(t, l, "retry", "a/1", hour, false))
		require.False(t, acquire(t, l, "retry", "b/1", hour, true))

		assert.True(t, acquire(t, l, "retry", "a/1", hour, false))
		assert.True(t, marked(t, l, "retry", "a/1"))
	})

	t.Run("a takeover after the lease lapsed clears the mark", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)
		require.True(t, acquire(t, l, "lapse", "a/1", hour, false))
		require.False(t, acquire(t, l, "lapse", "b/1", hour, true))
		_, superseded := leaseRow(t, db, "lapse")
		require.True(t, superseded)

		lapse(t, db, "lapse")
		require.True(t, acquire(t, l, "lapse", "c/1", hour, false))
		assert.False(t, marked(t, l, "lapse", "c/1"))
		assert.Empty(t, renew(t, l, scheduler.Lease{ID: "lapse", Owner: "a/1"}), "the lapsed owner lost it")
	})

	t.Run("supersede marks only another owner's live lease, on a refused acquire", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)

		// A new row.
		require.True(t, acquire(t, l, "mark", "a/1", hour, true))
		_, superseded := leaseRow(t, db, "mark")
		assert.False(t, superseded, "insert")

		// The owner's own retry.
		require.True(t, acquire(t, l, "mark", "a/1", hour, true))
		_, superseded = leaseRow(t, db, "mark")
		assert.False(t, superseded, "same owner")

		// Refused without supersede.
		require.False(t, acquire(t, l, "mark", "b/1", hour, false))
		_, superseded = leaseRow(t, db, "mark")
		assert.False(t, superseded, "refused without supersede")

		// A lapsed lease is taken, not marked.
		lapse(t, db, "mark")
		require.True(t, acquire(t, l, "mark", "c/1", hour, true))
		owner, superseded := leaseRow(t, db, "mark")
		assert.Equal(t, "c/1", owner)
		assert.False(t, superseded, "takeover")

		// Refused with supersede: marked, owner and lease unchanged.
		require.False(t, acquire(t, l, "mark", "d/1", hour, true))
		owner, superseded = leaseRow(t, db, "mark")
		assert.Equal(t, "c/1", owner)
		assert.True(t, superseded, "refused with supersede")
	})

	t.Run("renew returns held leases with their marks and omits lost ones", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)
		require.True(t, acquire(t, l, "renew-a", "a/1", hour, false))
		// Lapsed but not taken: still a/1's, so the renewal extends it.
		lapse(t, db, "renew-a")
		require.True(t, acquire(t, l, "renew-b", "b/1", hour, false))
		require.False(t, acquire(t, l, "renew-b", "x/1", hour, true))
		require.True(t, acquire(t, l, "renew-c", "c/1", hour, false))
		require.NoError(t, l.Release(context.Background(), "renew-c", "c/1"))
		require.True(t, acquire(t, l, "renew-d", "d/2", hour, false))

		renewed := renew(t, l,
			scheduler.Lease{ID: "renew-a", Owner: "a/1"},
			scheduler.Lease{ID: "renew-b", Owner: "b/1"},
			scheduler.Lease{ID: "renew-c", Owner: "c/1"},
			scheduler.Lease{ID: "renew-d", Owner: "d/1"},
		)
		assert.ElementsMatch(t, []scheduler.Lease{
			{ID: "renew-a", Owner: "a/1"},
			{ID: "renew-b", Owner: "b/1", Superseded: true},
		}, renewed)

		// renew-a was extended by an hour from the renewal.
		assert.False(t, acquire(t, l, "renew-a", "z/1", hour, false))
	})

	t.Run("renew of no leases makes no query", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)
		log := &queryLog{}
		db.AddQueryHook(log)

		renewed, err := l.Renew(context.Background(), nil, hour)
		assert.NoError(t, err)
		assert.Empty(t, renewed)
		renewed, err = l.Renew(context.Background(), []scheduler.Lease{}, hour)
		assert.NoError(t, err)
		assert.Empty(t, renewed)
		assert.Empty(t, log.get())
	})

	t.Run("release ends only the caller's lease and sweeps hour-old rows", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)
		_, err := db.ExecContext(context.Background(),
			"INSERT INTO ? (job_id, owner, held_until) VALUES ('sweep-old', 'dead/1', now() - interval '2 hours'), ('sweep-recent', 'dead/2', now() - interval '30 minutes')",
			bun.Ident(leaseTable))
		require.NoError(t, err)
		require.True(t, acquire(t, l, "rel", "a/1", hour, false))

		require.NoError(t, l.Release(context.Background(), "rel", "b/1"))
		assert.Equal(t, int64(1), leaseRowCount(t, db, "rel"), "another owner's release")
		assert.Equal(t, int64(0), leaseRowCount(t, db, "sweep-old"), "lapsed two hours ago")
		assert.Equal(t, int64(1), leaseRowCount(t, db, "sweep-recent"), "lapsed half an hour ago")

		require.NoError(t, l.Release(context.Background(), "rel", "a/1"))
		assert.Equal(t, int64(0), leaseRowCount(t, db, "rel"))
		assert.True(t, acquire(t, l, "rel", "b/1", hour, false))
	})

	t.Run("each operation is one statement", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)
		log := &queryLog{}
		db.AddQueryHook(log)

		require.True(t, acquire(t, l, "sql", "a/1", hour, false))
		renew(t, l, scheduler.Lease{ID: "sql", Owner: "a/1"}, scheduler.Lease{ID: "sql-other", Owner: "b/1"})
		require.NoError(t, l.Release(context.Background(), "sql", "a/1"))

		queries := log.get()
		require.Len(t, queries, 3)
		assert.Contains(t, queries[0], `INSERT INTO "`+leaseTable+`" AS lease`)
		assert.Contains(t, queries[1], `UPDATE "`+leaseTable+`" AS lease SET held_until = now()`)
		assert.Contains(t, queries[1], `(VALUES ('sql'::text, 'a/1'::text), ('sql-other'::text, 'b/1'::text))`)
		assert.Contains(t, queries[2], `DELETE FROM "`+leaseTable+`" WHERE ((job_id = 'sql' AND owner = 'a/1')) OR (held_until < now() - interval '1 hour')`)
	})

	t.Run("acquire statement keeps the deadline of its context but not the cancellation", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)
		deadline := time.Now().Add(10 * time.Second)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		hook := &cancelBeforeQuery{cancel: cancel}
		db.AddQueryHook(hook)

		won, err := l.Acquire(ctx, "detached", "a/1", hour, false)
		require.NoError(t, err)
		assert.True(t, won)
		assert.Error(t, ctx.Err(), "the caller's context was cancelled as the statement started")
		assert.NoError(t, hook.err, "the statement's context is not cancelled")
		assert.True(t, hook.hasDeadline)
		assert.True(t, hook.deadline.Equal(deadline), "deadline %s", hook.deadline)
	})

	t.Run("acquire on a cancelled context writes nothing", func(t *testing.T) {
		l := NewBunLeaser(db, leaseTable)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		won, err := l.Acquire(ctx, "done", "a/1", hour, false)
		assert.False(t, won)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		assert.Zero(t, leaseRowCount(t, db, "done"))
	})

	t.Run("CreateLeaseTable is idempotent", func(t *testing.T) {
		err := CreateLeaseTable(context.Background(), db, leaseTable)
		assert.NoError(t, err)
	})

	t.Run("missing table is an internal error", func(t *testing.T) {
		l := NewBunLeaser(db, "no_such_table")
		_, err := l.Acquire(context.Background(), "a", "a/1", hour, false)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		_, err = l.Renew(context.Background(), []scheduler.Lease{{ID: "a", Owner: "a/1"}}, hour)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		err = l.Release(context.Background(), "a", "a/1")
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	})
}

// TestCreateLeaseTableConcurrently: instances booting together against a new
// database all create the table without an error.
func TestCreateLeaseTableConcurrently(t *testing.T) {
	db := newEmptyTestDB(t)

	const rounds = 10
	const instances = 8
	for round := 0; round < rounds; round++ {
		table := fmt.Sprintf("leases_boot_%d", round)
		var wg sync.WaitGroup
		errs := make(chan error, instances)
		for i := 0; i < instances; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- CreateLeaseTable(context.Background(), db, table)
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "round %d", round)
		}
	}
}

// TestLeasesSupersedeAcrossSchedulers: two Schedulers on one table stand for
// two instances. Supersede on one cancels the run on the other with
// ErrSuperseded, at its next renewal, and its job runs once that run returned.
func TestLeasesSupersedeAcrossSchedulers(t *testing.T) {
	db := newLeaseTestDB(t)
	a, err := scheduler.New(scheduler.WithLeaser(NewBunLeaser(db, leaseTable)))
	require.NoError(t, err)
	b, err := scheduler.New(scheduler.WithLeaser(NewBunLeaser(db, leaseTable)))
	require.NoError(t, err)

	const wait = 15 * time.Second
	started := make(chan struct{})
	causes := make(chan error, 1)
	var mu sync.Mutex
	firstReturned := false
	require.True(t, a.RunOnce(context.Background(), "x", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		mu.Lock()
		firstReturned = true
		mu.Unlock()
		causes <- context.Cause(ctx)
	}))
	select {
	case <-started:
	case <-time.After(wait):
		t.Fatal("the first run did not start")
	}

	replacementSaw := make(chan bool, 1)
	require.True(t, b.Supersede(context.Background(), "x", func(context.Context) {
		mu.Lock()
		defer mu.Unlock()
		replacementSaw <- firstReturned
	}))

	select {
	case cause := <-causes:
		assert.True(t, errors.Is(cause, scheduler.ErrSuperseded), "cause %v", cause)
	case <-time.After(wait):
		t.Fatal("the first run was not cancelled")
	}
	select {
	case sawFirstReturned := <-replacementSaw:
		assert.True(t, sawFirstReturned)
	case <-time.After(wait):
		t.Fatal("the replacement did not run")
	}

	require.Eventually(t, func() bool { return a.Active() == 0 && b.Active() == 0 }, wait, 10*time.Millisecond)
	assert.Equal(t, int64(0), leaseRowCount(t, db, "x"), "both leases released")
}
