package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/auth/token"
	"github.com/vanclief/ez"
)

func TestMemoryStore(t *testing.T) {
	testStore(t, NewMemoryStore())
}

// newRecord returns an unrotated record for subject with a fresh token.
// Times are UTC microseconds, the precision both SQL backends keep.
func newRecord(t *testing.T, subject string) Record {
	tok, err := token.Generate("apps")
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	return Record{
		Session: Session{
			ID:         subject + "-" + tok.Hint,
			Subject:    subject,
			CreatedAt:  now,
			LastUsedAt: now,
			RotatedAt:  now,
			ExpiresAt:  now.Add(time.Hour),
		},
		TokenHash: tok.Hash,
	}
}

func assertRecord(t *testing.T, want, got Record) {
	t.Helper()
	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.Subject, got.Subject)
	assert.Equal(t, want.TokenHash, got.TokenHash)
	assert.Equal(t, want.PreviousHash, got.PreviousHash)
	assert.True(t, want.CreatedAt.Equal(got.CreatedAt), "created_at %v != %v", want.CreatedAt, got.CreatedAt)
	assert.True(t, want.LastUsedAt.Equal(got.LastUsedAt), "last_used_at %v != %v", want.LastUsedAt, got.LastUsedAt)
	assert.True(t, want.RotatedAt.Equal(got.RotatedAt), "rotated_at %v != %v", want.RotatedAt, got.RotatedAt)
	assert.True(t, want.FirstUsedAt.Equal(got.FirstUsedAt), "first_used_at %v != %v", want.FirstUsedAt, got.FirstUsedAt)
	assert.True(t, want.ExpiresAt.Equal(got.ExpiresAt), "expires_at %v != %v", want.ExpiresAt, got.ExpiresAt)
	assert.Equal(t, want.Revision, got.Revision)
}

// testStore checks the Store contract against an empty store.
func testStore(t *testing.T, store Store) {
	ctx := context.Background()

	t.Run("insert and find by current hash", func(t *testing.T) {
		r := newRecord(t, "find")
		err := store.Insert(ctx, r)
		require.NoError(t, err)

		got, err := store.FindByHash(ctx, r.TokenHash)
		require.NoError(t, err)
		assertRecord(t, r, got)
		assert.Nil(t, got.PreviousHash)
		assert.True(t, got.FirstUsedAt.IsZero())
		assert.Equal(t, int64(0), got.Revision)
	})

	t.Run("find unknown hash", func(t *testing.T) {
		_, err := store.FindByHash(ctx, token.Hash("nothing"))
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	})

	t.Run("update writes every mutable column", func(t *testing.T) {
		r := newRecord(t, "update")
		require.NoError(t, store.Insert(ctx, r))

		oldHash := r.TokenHash
		next := newRecord(t, "other")
		later := r.LastUsedAt.Add(time.Minute)
		r.TokenHash = next.TokenHash
		r.PreviousHash = oldHash
		r.FirstUsedAt = later
		r.LastUsedAt = later
		r.RotatedAt = later
		r.ExpiresAt = later.Add(time.Hour)
		err := store.Update(ctx, r)
		require.NoError(t, err)
		r.Revision++

		got, err := store.FindByHash(ctx, next.TokenHash)
		require.NoError(t, err)
		assertRecord(t, r, got)

		got, err = store.FindByHash(ctx, oldHash)
		require.NoError(t, err, "the previous hash still finds the record")
		assertRecord(t, r, got)

		r.FirstUsedAt = time.Time{}
		r.ExpiresAt = time.Time{}
		err = store.Update(ctx, r)
		require.NoError(t, err)
		got, err = store.FindByHash(ctx, r.TokenHash)
		require.NoError(t, err)
		assert.True(t, got.FirstUsedAt.IsZero(), "zero times round-trip")
		assert.True(t, got.ExpiresAt.IsZero())
		assert.Equal(t, int64(2), got.Revision, "each update bumps the revision by one")
	})

	t.Run("update with a stale revision matches nothing", func(t *testing.T) {
		r := newRecord(t, "stale")
		require.NoError(t, store.Insert(ctx, r))

		// A concurrent request writes first: only the first use changes, the
		// token hash stays the same.
		used := r
		used.FirstUsedAt = r.LastUsedAt.Add(time.Second)
		used.LastUsedAt = used.FirstUsedAt
		err := store.Update(ctx, used)
		require.NoError(t, err)
		used.Revision = 1

		// A write based on the read before it matches nothing.
		stale := r
		next := newRecord(t, "other")
		stale.TokenHash = next.TokenHash
		stale.PreviousHash = r.TokenHash
		stale.LastUsedAt = r.LastUsedAt.Add(time.Hour)
		err = store.Update(ctx, stale)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))

		got, err := store.FindByHash(ctx, r.TokenHash)
		require.NoError(t, err)
		assertRecord(t, used, got)
		_, err = store.FindByHash(ctx, next.TokenHash)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err), "the stale rotation was not written")

		missing := newRecord(t, "missing")
		err = store.Update(ctx, missing)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	})

	t.Run("delete", func(t *testing.T) {
		r := newRecord(t, "delete")
		require.NoError(t, store.Insert(ctx, r))

		err := store.Delete(ctx, r.ID)
		require.NoError(t, err)
		_, err = store.FindByHash(ctx, r.TokenHash)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))

		err = store.Delete(ctx, r.ID)
		require.NoError(t, err, "deleting a missing session is not an error")
	})

	t.Run("delete revision", func(t *testing.T) {
		r := newRecord(t, "delete-revision")
		require.NoError(t, store.Insert(ctx, r))

		// A concurrent request writes first and bumps the revision.
		used := r
		used.FirstUsedAt = r.LastUsedAt.Add(time.Second)
		used.LastUsedAt = used.FirstUsedAt
		require.NoError(t, store.Update(ctx, used))
		used.Revision = 1

		// A delete based on the read before it matches nothing.
		err := store.DeleteRevision(ctx, r.ID, r.Revision)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
		got, err := store.FindByHash(ctx, r.TokenHash)
		require.NoError(t, err, "the stale delete left the row")
		assertRecord(t, used, got)

		err = store.DeleteRevision(ctx, r.ID, used.Revision)
		require.NoError(t, err)
		_, err = store.FindByHash(ctx, r.TokenHash)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err), "the matching revision deletes the row")

		err = store.DeleteRevision(ctx, r.ID, used.Revision)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err), "a deleted session matches nothing")

		missing := newRecord(t, "missing")
		err = store.DeleteRevision(ctx, missing.ID, 0)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	})

	t.Run("delete by subject", func(t *testing.T) {
		first := newRecord(t, "revoked")
		second := newRecord(t, "revoked")
		kept := newRecord(t, "kept")
		for _, r := range []Record{first, second, kept} {
			require.NoError(t, store.Insert(ctx, r))
		}

		err := store.DeleteBySubject(ctx, "revoked")
		require.NoError(t, err)
		_, err = store.FindByHash(ctx, first.TokenHash)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
		_, err = store.FindByHash(ctx, second.TokenHash)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
		_, err = store.FindByHash(ctx, kept.TokenHash)
		require.NoError(t, err)
	})

	t.Run("delete expired", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Microsecond)
		idleBefore := now.Add(-time.Hour)

		// Past MaxTTL: ExpiresAt is not after now.
		maxExpired := newRecord(t, "sweep-max")
		maxExpired.ExpiresAt = now
		// MaxTTL Never and unused since idleBefore.
		idle := newRecord(t, "sweep-idle")
		idle.ExpiresAt = time.Time{}
		idle.LastUsedAt = idleBefore
		live := newRecord(t, "sweep-live")
		live.LastUsedAt = now
		live.ExpiresAt = now.Add(time.Hour)
		never := newRecord(t, "sweep-never")
		never.ExpiresAt = time.Time{}
		never.LastUsedAt = now
		for _, r := range []Record{maxExpired, idle, live, never} {
			require.NoError(t, store.Insert(ctx, r))
		}

		count, err := store.DeleteExpired(ctx, now, idleBefore)
		require.NoError(t, err)
		assert.Equal(t, int64(2), count)
		_, err = store.FindByHash(ctx, maxExpired.TokenHash)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err), "past MaxTTL")
		_, err = store.FindByHash(ctx, idle.TokenHash)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err), "idle")
		got, err := store.FindByHash(ctx, live.TokenHash)
		require.NoError(t, err)
		assertRecord(t, live, got)
		got, err = store.FindByHash(ctx, never.TokenHash)
		require.NoError(t, err, "a zero ExpiresAt never matches the MaxTTL rule")
		assertRecord(t, never, got)

		// A zero idleBefore (IdleTTL Never) applies the MaxTTL rule only.
		idleKept := newRecord(t, "sweep-idle-kept")
		idleKept.ExpiresAt = time.Time{}
		idleKept.LastUsedAt = now.Add(-1000 * time.Hour)
		expired := newRecord(t, "sweep-max-again")
		expired.ExpiresAt = now.Add(-time.Minute)
		for _, r := range []Record{idleKept, expired} {
			require.NoError(t, store.Insert(ctx, r))
		}

		count, err = store.DeleteExpired(ctx, now, time.Time{})
		require.NoError(t, err)
		assert.Equal(t, int64(1), count)
		_, err = store.FindByHash(ctx, expired.TokenHash)
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
		_, err = store.FindByHash(ctx, idleKept.TokenHash)
		require.NoError(t, err, "no idle rule without idleBefore")
		_, err = store.FindByHash(ctx, live.TokenHash)
		require.NoError(t, err)
	})
}
