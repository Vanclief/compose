package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/auth/token"
	"github.com/vanclief/ez"
)

// testClock is the time a Manager built by newManager sees. Tests move it
// between calls, never while goroutines read it.
type testClock struct {
	now time.Time
}

func (c *testClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
}

// newManager returns a Manager over a memory store with prefix "app" and a
// clock the test controls. cfg's Prefix and Store are filled in when empty.
func newManager(t *testing.T, cfg Config) (*Manager, *testClock) {
	if cfg.Prefix == "" {
		cfg.Prefix = "app"
	}
	if cfg.Store == nil {
		cfg.Store = NewMemoryStore()
	}
	m, err := New(cfg)
	require.NoError(t, err)
	clock := &testClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	m.now = func() time.Time {
		return clock.now
	}
	return m, clock
}

func assertUnauthenticated(t *testing.T, err error, message string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err))
	if message != "" {
		assert.Equal(t, message, ez.ErrorMessage(err))
	}
}

// panicStore panics on any call, proving the Manager did not reach it.
type panicStore struct {
	Store
}

func TestNew(t *testing.T) {
	store := NewMemoryStore()

	m, err := New(Config{Prefix: "app", Store: store})
	require.NoError(t, err)
	assert.Equal(t, 30*24*time.Hour, m.cfg.IdleTTL)
	assert.Equal(t, 90*24*time.Hour, m.cfg.MaxTTL)
	assert.Equal(t, time.Hour, m.cfg.RotateAfter)
	assert.Equal(t, 60*time.Second, m.cfg.Grace)
	assert.Equal(t, time.Minute, m.cfg.TouchEvery)

	_, err = New(Config{Prefix: "app", Store: store, IdleTTL: Never, MaxTTL: Never, RotateAfter: Never})
	require.NoError(t, err)

	_, err = New(Config{Prefix: "app", Store: store, TouchEvery: time.Second})
	require.NoError(t, err)

	bad := map[string]Config{
		"bad prefix":            {Prefix: "APP", Store: store},
		"prefix too long":       {Prefix: "abcdefghijkl", Store: store},
		"no store":              {Prefix: "app"},
		"grace never":           {Prefix: "app", Store: store, Grace: Never},
		"negative grace":        {Prefix: "app", Store: store, Grace: -time.Second},
		"negative idle ttl":     {Prefix: "app", Store: store, IdleTTL: -time.Second},
		"negative max ttl":      {Prefix: "app", Store: store, MaxTTL: -time.Hour},
		"negative rotate after": {Prefix: "app", Store: store, RotateAfter: -2},
		"touch every never":     {Prefix: "app", Store: store, TouchEvery: Never},
		"touch every too short": {Prefix: "app", Store: store, TouchEvery: time.Second - 1},
		"idle ttl under touch":  {Prefix: "app", Store: store, IdleTTL: 30 * time.Second},
		"idle ttl equals touch": {Prefix: "app", Store: store, IdleTTL: time.Minute, TouchEvery: time.Minute},
	}
	for name, cfg := range bad {
		_, err := New(cfg)
		assert.Equal(t, ez.EINVALID, ez.ErrorCode(err), name)
	}
}

func TestIssueAndResolve(t *testing.T) {
	ctx := context.Background()
	m, clock := newManager(t, Config{})

	value, issued, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(value, "apps_"))
	assert.Equal(t, "user-1", issued.Subject)
	assert.Equal(t, clock.now.Add(90*24*time.Hour), issued.ExpiresAt)
	assert.True(t, issued.FirstUsedAt.IsZero())

	clock.advance(time.Minute)
	got, rotated, err := m.Resolve(ctx, value)
	require.NoError(t, err)
	assert.Empty(t, rotated)
	assert.Equal(t, issued.ID, got.ID)
	assert.Equal(t, "user-1", got.Subject)
	assert.Equal(t, clock.now, got.LastUsedAt)
	assert.Equal(t, clock.now, got.FirstUsedAt)

	_, _, err = m.Issue(ctx, "")
	assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
}

func TestResolveMalformedNeverTouchesStore(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t, Config{Store: panicStore{}})

	other, err := token.Generate("appat")
	require.NoError(t, err)
	for _, value := range []string{"", "garbage", "apps_short", other.Value, "apps_" + strings.Repeat("!", 43)} {
		_, _, err := m.Resolve(ctx, value)
		assertUnauthenticated(t, err, "session: token is malformed")
		err = m.Revoke(ctx, value)
		assertUnauthenticated(t, err, "session: token is malformed")
	}
}

func TestResolveUnknownToken(t *testing.T) {
	m, _ := newManager(t, Config{})
	unknown, err := token.Generate("apps")
	require.NoError(t, err)

	_, _, err = m.Resolve(context.Background(), unknown.Value)
	assertUnauthenticated(t, err, "session: unknown token")
}

func TestIdleExpiry(t *testing.T) {
	ctx := context.Background()
	m, clock := newManager(t, Config{IdleTTL: time.Hour, RotateAfter: Never})
	value, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)

	clock.advance(59 * time.Minute)
	_, _, err = m.Resolve(ctx, value)
	require.NoError(t, err, "use resets the idle timer")

	clock.advance(59 * time.Minute)
	_, _, err = m.Resolve(ctx, value)
	require.NoError(t, err)

	clock.advance(time.Hour)
	_, _, err = m.Resolve(ctx, value)
	assertUnauthenticated(t, err, "session: expired")

	_, _, err = m.Resolve(ctx, value)
	assertUnauthenticated(t, err, "session: unknown token")
}

func TestMaxExpiryDespiteUse(t *testing.T) {
	ctx := context.Background()
	m, clock := newManager(t, Config{})
	value, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)

	for day := 10; day < 90; day += 10 {
		clock.advance(10 * 24 * time.Hour)
		_, rotated, err := m.Resolve(ctx, value)
		require.NoError(t, err, "day %d", day)
		if rotated != "" {
			value = rotated
		}
	}
	clock.advance(10 * 24 * time.Hour)
	_, _, err = m.Resolve(ctx, value)
	assertUnauthenticated(t, err, "session: expired")
}

func TestRotation(t *testing.T) {
	ctx := context.Background()
	m, clock := newManager(t, Config{})
	old, issued, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)

	clock.advance(time.Hour)
	got, fresh, err := m.Resolve(ctx, old)
	require.NoError(t, err)
	assert.True(t, token.Check("apps", fresh), "rotated token has the apps_ prefix")
	assert.NotEqual(t, old, fresh)
	assert.Equal(t, issued.ID, got.ID)
	assert.Equal(t, clock.now, got.RotatedAt)
	assert.True(t, got.FirstUsedAt.IsZero(), "the new token is not used yet")

	clock.advance(10 * time.Second)
	_, rotated, err := m.Resolve(ctx, old)
	require.NoError(t, err, "a request in flight with the old token succeeds")
	assert.Empty(t, rotated, "and does not rotate again")

	clock.advance(10 * time.Second)
	got, rotated, err = m.Resolve(ctx, fresh)
	require.NoError(t, err)
	assert.Empty(t, rotated)
	assert.Equal(t, clock.now, got.FirstUsedAt)

	clock.advance(59 * time.Second)
	_, rotated, err = m.Resolve(ctx, old)
	require.NoError(t, err, "old token inside Grace after the new token's first use")
	assert.Empty(t, rotated)

	clock.advance(time.Hour - 2*time.Minute)
	_, rotated, err = m.Resolve(ctx, fresh)
	require.NoError(t, err)
	assert.Empty(t, rotated, "no rotation before RotateAfter passes again")

	clock.advance(2 * time.Minute)
	_, rotated, err = m.Resolve(ctx, fresh)
	require.NoError(t, err)
	assert.True(t, token.Check("apps", rotated), "rotates again after RotateAfter")
}

func TestRotationWaitsForPreviousGrace(t *testing.T) {
	ctx := context.Background()
	m, clock := newManager(t, Config{})
	old, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)

	clock.advance(time.Hour)
	_, fresh, err := m.Resolve(ctx, old)
	require.NoError(t, err)
	require.NotEmpty(t, fresh)

	// The new token is first used just before RotateAfter passes again.
	clock.advance(time.Hour - 30*time.Second)
	_, rotated, err := m.Resolve(ctx, fresh)
	require.NoError(t, err)
	assert.Empty(t, rotated)

	clock.advance(30 * time.Second)
	_, rotated, err = m.Resolve(ctx, fresh)
	require.NoError(t, err)
	assert.Empty(t, rotated, "no rotation while the previous token is inside Grace")

	_, _, err = m.Resolve(ctx, old)
	require.NoError(t, err, "the previous token is still accepted")

	clock.advance(30 * time.Second)
	_, rotated, err = m.Resolve(ctx, fresh)
	require.NoError(t, err)
	assert.True(t, token.Check("apps", rotated), "rotates once Grace has ended")
}

func TestReuseAfterGraceRevokesSession(t *testing.T) {
	ctx := context.Background()
	m, clock := newManager(t, Config{})
	old, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)

	clock.advance(time.Hour)
	_, fresh, err := m.Resolve(ctx, old)
	require.NoError(t, err)
	require.NotEmpty(t, fresh)

	_, _, err = m.Resolve(ctx, fresh)
	require.NoError(t, err)

	clock.advance(time.Minute)
	_, _, err = m.Resolve(ctx, old)
	assertUnauthenticated(t, err, "session: token was presented after its grace")

	_, _, err = m.Resolve(ctx, fresh)
	assertUnauthenticated(t, err, "session: unknown token")
}

// TestPreviousTokenAfterGraceOfRotationRevokesSession: while the new token
// is unused, the previous token is accepted only inside Grace of the
// rotation. Later the session is deleted, the new token included, so a
// rotation response lost on the wire costs the user a login.
func TestPreviousTokenAfterGraceOfRotationRevokesSession(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	m, clock := newManager(t, Config{Store: store})
	old, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)

	clock.advance(time.Hour)
	_, current, err := m.Resolve(ctx, old)
	require.NoError(t, err)
	require.NotEmpty(t, current)

	clock.advance(59 * time.Second)
	_, rotated, err := m.Resolve(ctx, old)
	require.NoError(t, err, "a request in flight inside Grace of the rotation succeeds")
	assert.Empty(t, rotated)

	clock.advance(time.Second)
	_, rotated, err = m.Resolve(ctx, old)
	assertUnauthenticated(t, err, "session: token was presented after its grace")
	assert.Empty(t, rotated, "the previous token never gets a replacement")

	_, err = store.FindByHash(ctx, token.Hash(current))
	assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err), "the session row is gone")

	_, _, err = m.Resolve(ctx, current)
	assertUnauthenticated(t, err, "session: unknown token")
}

func TestTouchEveryCoalescesWrites(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	m, clock := newManager(t, Config{Store: store, TouchEvery: time.Minute, RotateAfter: Never})
	value, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)
	hash := token.Hash(value)

	// The first use writes FirstUsedAt and LastUsedAt.
	clock.advance(10 * time.Second)
	_, _, err = m.Resolve(ctx, value)
	require.NoError(t, err)
	stored, err := store.FindByHash(ctx, hash)
	require.NoError(t, err)
	firstUse := clock.now
	assert.True(t, stored.LastUsedAt.Equal(firstUse))

	// A use inside TouchEvery is served from the row without writing it.
	clock.advance(30 * time.Second)
	got, rotated, err := m.Resolve(ctx, value)
	require.NoError(t, err)
	assert.Empty(t, rotated)
	assert.True(t, got.LastUsedAt.Equal(firstUse))
	stored, err = store.FindByHash(ctx, hash)
	require.NoError(t, err)
	assert.True(t, stored.LastUsedAt.Equal(firstUse), "no write inside TouchEvery")

	// Once TouchEvery has passed the row is written again.
	clock.advance(31 * time.Second)
	got, _, err = m.Resolve(ctx, value)
	require.NoError(t, err)
	assert.True(t, got.LastUsedAt.Equal(clock.now))
	stored, err = store.FindByHash(ctx, hash)
	require.NoError(t, err)
	assert.True(t, stored.LastUsedAt.Equal(clock.now))
}

func TestConcurrentResolveRotatesOnce(t *testing.T) {
	ctx := context.Background()
	m, clock := newManager(t, Config{})
	testConcurrentResolve(t, ctx, m, clock)
}

// testConcurrentResolve resolves one token from 20 goroutines after
// RotateAfter and checks that exactly one of them rotated.
func testConcurrentResolve(t *testing.T, ctx context.Context, m *Manager, clock *testClock) {
	old, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)
	clock.advance(time.Hour)

	const workers = 20
	var wg sync.WaitGroup
	rotations := make([]string, workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, rotations[i], errs[i] = m.Resolve(ctx, old)
		}()
	}
	wg.Wait()

	var fresh []string
	for i := 0; i < workers; i++ {
		require.NoError(t, errs[i])
		if rotations[i] != "" {
			fresh = append(fresh, rotations[i])
		}
	}
	require.Len(t, fresh, 1)

	_, rotated, err := m.Resolve(ctx, fresh[0])
	require.NoError(t, err)
	assert.Empty(t, rotated)
}

// hookStore runs beforeUpdate once, on the next Update, and
// beforeDeleteRevision once, on the next DeleteRevision, before the write, so
// a test can run another request between a read and its write.
type hookStore struct {
	Store
	beforeUpdate         func(Record)
	beforeDeleteRevision func(id string, revision int64)
}

func (s *hookStore) Update(ctx context.Context, r Record) error {
	hook := s.beforeUpdate
	s.beforeUpdate = nil
	if hook != nil {
		hook(r)
	}
	return s.Store.Update(ctx, r)
}

func (s *hookStore) DeleteRevision(ctx context.Context, id string, revision int64) error {
	hook := s.beforeDeleteRevision
	s.beforeDeleteRevision = nil
	if hook != nil {
		hook(id, revision)
	}
	return s.Store.DeleteRevision(ctx, id, revision)
}

// TestPreviousTokenAfterGraceYieldsToConcurrentFirstUse: a previous-token
// request that read the session before the current token's first use, after
// Grace of the rotation, deletes only the revision it read. A concurrent first
// use between the read and the delete makes the delete miss, and the retry
// decides on the first use.
func TestPreviousTokenAfterGraceYieldsToConcurrentFirstUse(t *testing.T) {
	ctx := context.Background()

	// setup rotates a session and lets Grace pass without the new token being
	// used, so the previous token reads as presented after its grace.
	setup := func(t *testing.T) (*Manager, *testClock, *hookStore, string, string) {
		store := &hookStore{Store: NewMemoryStore()}
		m, clock := newManager(t, Config{Store: store})
		old, _, err := m.Issue(ctx, "user-1")
		require.NoError(t, err)
		clock.advance(time.Hour)
		_, current, err := m.Resolve(ctx, old)
		require.NoError(t, err)
		require.NotEmpty(t, current)
		clock.advance(2 * time.Minute)
		return m, clock, store, old, current
	}

	t.Run("inside grace of the first use", func(t *testing.T) {
		m, _, store, old, current := setup(t)
		hooked := false
		store.beforeDeleteRevision = func(id string, revision int64) {
			hooked = true
			stored, err := store.FindByHash(ctx, token.Hash(current))
			require.NoError(t, err)
			require.Equal(t, stored.Revision, revision, "the previous token decided to delete on the stored revision")
			require.True(t, stored.FirstUsedAt.IsZero(), "before the current token's first use")
			// The concurrent first use lands between the read and the delete.
			_, rotated, err := m.Resolve(ctx, current)
			require.NoError(t, err)
			assert.Empty(t, rotated)
		}

		_, rotated, err := m.Resolve(ctx, old)
		require.NoError(t, err, "the retry finds the previous token inside Grace of the first use")
		assert.Empty(t, rotated, "and does not rotate")
		assert.True(t, hooked, "the previous token took the conditional delete")

		stored, err := store.FindByHash(ctx, token.Hash(current))
		require.NoError(t, err, "the session was not deleted")
		assert.Equal(t, token.Hash(current), stored.TokenHash, "the used token is still current")
		assert.False(t, stored.FirstUsedAt.IsZero())

		_, rotated, err = m.Resolve(ctx, current)
		require.NoError(t, err)
		assert.Empty(t, rotated)
	})

	t.Run("after grace of the first use", func(t *testing.T) {
		m, clock, store, old, current := setup(t)
		store.beforeDeleteRevision = func(id string, revision int64) {
			_, _, err := m.Resolve(ctx, current)
			require.NoError(t, err)
			clock.advance(time.Minute)
		}

		_, rotated, err := m.Resolve(ctx, old)
		assertUnauthenticated(t, err, "session: token was presented after its grace")
		assert.Empty(t, rotated, "the previous token never gets a replacement")

		_, _, err = m.Resolve(ctx, current)
		assertUnauthenticated(t, err, "session: unknown token")
	})
}

// TestIdleExpiryYieldsToConcurrentTouch: a request that read the session
// before a concurrent request extended LastUsedAt, and so found it idle
// expired, deletes only the revision it read, retries on the stale read and
// keeps the session.
func TestIdleExpiryYieldsToConcurrentTouch(t *testing.T) {
	ctx := context.Background()
	store := &hookStore{Store: NewMemoryStore()}
	m, clock := newManager(t, Config{Store: store, IdleTTL: time.Hour, RotateAfter: Never})
	value, issued, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)
	clock.advance(61 * time.Minute)

	hooked := false
	store.beforeDeleteRevision = func(id string, revision int64) {
		hooked = true
		fresh, err := store.FindByHash(ctx, token.Hash(value))
		require.NoError(t, err)
		require.Equal(t, fresh.Revision, revision, "the request read the session before the touch")
		require.Equal(t, issued.LastUsedAt, fresh.LastUsedAt)
		// The concurrent touch landed just before the idle deadline.
		fresh.LastUsedAt = issued.LastUsedAt.Add(59 * time.Minute)
		err = store.Store.Update(ctx, fresh)
		require.NoError(t, err)
	}

	session, rotated, err := m.Resolve(ctx, value)
	require.NoError(t, err, "the retry sees the concurrent touch")
	assert.Equal(t, issued.ID, session.ID)
	assert.Empty(t, rotated)
	assert.True(t, hooked, "the expiry took the conditional delete")

	stored, err := store.FindByHash(ctx, token.Hash(value))
	require.NoError(t, err, "the session was not deleted")
	assert.Equal(t, issued.ID, stored.ID)
}

// TestReuseYieldsToConcurrentUse: a reuse of the previous token deletes only
// the revision it read, so a concurrent use of the current token between the
// read and the delete makes the request retry and decide on what is stored
// now.
func TestReuseYieldsToConcurrentUse(t *testing.T) {
	ctx := context.Background()

	// setup rotates a session, uses the new token and lets the given time
	// pass, so the previous token reads as reused.
	setup := func(t *testing.T, wait time.Duration) (*Manager, *hookStore, string, string) {
		store := &hookStore{Store: NewMemoryStore()}
		m, clock := newManager(t, Config{Store: store})
		old, _, err := m.Issue(ctx, "user-1")
		require.NoError(t, err)
		clock.advance(time.Hour)
		_, current, err := m.Resolve(ctx, old)
		require.NoError(t, err)
		require.NotEmpty(t, current)
		clock.advance(10 * time.Second)
		_, _, err = m.Resolve(ctx, current)
		require.NoError(t, err)
		clock.advance(wait)
		return m, store, old, current
	}

	t.Run("a concurrent rotation drops the previous token", func(t *testing.T) {
		m, store, old, current := setup(t, time.Hour)
		next := ""
		store.beforeDeleteRevision = func(id string, revision int64) {
			_, rotated, err := m.Resolve(ctx, current)
			require.NoError(t, err)
			require.NotEmpty(t, rotated, "the current token rotates past RotateAfter")
			next = rotated
		}

		_, _, err := m.Resolve(ctx, old)
		assertUnauthenticated(t, err, "session: unknown token")

		got, rotated, err := m.Resolve(ctx, next)
		require.NoError(t, err, "the session the concurrent request rotated survives")
		assert.Empty(t, rotated)
		assert.Equal(t, "user-1", got.Subject)
	})

	t.Run("a concurrent touch is re-read before the delete", func(t *testing.T) {
		m, store, old, current := setup(t, 2*time.Minute)
		var stale, retried int64 = -1, -1
		store.beforeDeleteRevision = func(id string, revision int64) {
			stale = revision
			_, rotated, err := m.Resolve(ctx, current)
			require.NoError(t, err)
			require.Empty(t, rotated, "the current token is only touched")
			store.beforeDeleteRevision = func(id string, revision int64) {
				retried = revision
			}
		}

		_, _, err := m.Resolve(ctx, old)
		assertUnauthenticated(t, err, "session: token was presented after its grace")
		assert.Equal(t, stale+1, retried, "the retry deleted the revision the touch wrote")

		_, _, err = m.Resolve(ctx, current)
		assertUnauthenticated(t, err, "session: unknown token")
	})
}

// conflictStore reports every Update as lost to a concurrent write.
type conflictStore struct {
	Store
	updates int
}

func (s *conflictStore) Update(ctx context.Context, r Record) error {
	s.updates++
	return ez.New(ez.ENOTFOUND, "session: no session to update", nil)
}

func TestResolveGivesUpAfterRepeatedConflicts(t *testing.T) {
	ctx := context.Background()
	store := &conflictStore{Store: NewMemoryStore()}
	m, _ := newManager(t, Config{Store: store})
	value, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)

	_, rotated, err := m.Resolve(ctx, value)
	require.Error(t, err)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
	assert.Equal(t, "session: too many concurrent updates", ez.ErrorMessage(err))
	assert.Empty(t, rotated)
	assert.Equal(t, 3, store.updates)
}

func TestRevoke(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t, Config{})
	first, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)
	second, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)
	third, _, err := m.Issue(ctx, "user-1")
	require.NoError(t, err)
	other, _, err := m.Issue(ctx, "user-2")
	require.NoError(t, err)

	err = m.Revoke(ctx, first)
	require.NoError(t, err)
	_, _, err = m.Resolve(ctx, first)
	assertUnauthenticated(t, err, "")
	err = m.Revoke(ctx, first)
	assertUnauthenticated(t, err, "session: unknown token")

	_, _, err = m.Resolve(ctx, second)
	require.NoError(t, err)

	err = m.RevokeSubject(ctx, "user-1")
	require.NoError(t, err)
	_, _, err = m.Resolve(ctx, second)
	assertUnauthenticated(t, err, "")
	_, _, err = m.Resolve(ctx, third)
	assertUnauthenticated(t, err, "")

	_, _, err = m.Resolve(ctx, other)
	require.NoError(t, err, "other subjects keep their sessions")
}

// sweepErrorStore fails every DeleteExpired.
type sweepErrorStore struct {
	Store
}

func (s *sweepErrorStore) DeleteExpired(ctx context.Context, now, idleBefore time.Time) (int64, error) {
	return 0, ez.New(ez.EINTERNAL, "session: sweeping expired sessions failed", nil)
}

func TestSweep(t *testing.T) {
	ctx := context.Background()

	t.Run("idle and max expired sessions go", func(t *testing.T) {
		m, clock := newManager(t, Config{IdleTTL: 2 * time.Hour, MaxTTL: 10 * time.Hour, RotateAfter: Never})
		maxed, _, err := m.Issue(ctx, "user-1")
		require.NoError(t, err)
		idle, _, err := m.Issue(ctx, "user-2")
		require.NoError(t, err)

		// maxed is used every hour, so only MaxTTL expires it.
		for range 9 {
			clock.advance(time.Hour)
			_, _, err = m.Resolve(ctx, maxed)
			require.NoError(t, err)
		}
		live, _, err := m.Issue(ctx, "user-3")
		require.NoError(t, err)
		clock.advance(time.Hour)

		count, err := m.Sweep(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(2), count)
		_, _, err = m.Resolve(ctx, maxed)
		assertUnauthenticated(t, err, "session: unknown token")
		_, _, err = m.Resolve(ctx, idle)
		assertUnauthenticated(t, err, "session: unknown token")
		_, _, err = m.Resolve(ctx, live)
		require.NoError(t, err, "a session used within IdleTTL and MaxTTL stays")

		count, err = m.Sweep(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(0), count)
	})

	t.Run("idle ttl never keeps idle sessions", func(t *testing.T) {
		m, clock := newManager(t, Config{IdleTTL: Never, MaxTTL: 100 * time.Hour, RotateAfter: Never})
		maxed, _, err := m.Issue(ctx, "user-1")
		require.NoError(t, err)
		clock.advance(50 * time.Hour)
		idle, _, err := m.Issue(ctx, "user-2")
		require.NoError(t, err)
		clock.advance(50 * time.Hour)

		count, err := m.Sweep(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1), count)
		_, _, err = m.Resolve(ctx, maxed)
		assertUnauthenticated(t, err, "session: unknown token")
		_, _, err = m.Resolve(ctx, idle)
		require.NoError(t, err, "unused for 50 hours but IdleTTL is Never")
	})

	t.Run("store error", func(t *testing.T) {
		m, _ := newManager(t, Config{Store: &sweepErrorStore{Store: NewMemoryStore()}})
		count, err := m.Sweep(ctx)
		require.Error(t, err)
		assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))
		assert.Equal(t, int64(0), count)
	})
}

func TestNever(t *testing.T) {
	ctx := context.Background()

	t.Run("idle ttl", func(t *testing.T) {
		m, clock := newManager(t, Config{IdleTTL: Never, RotateAfter: Never})
		value, _, err := m.Issue(ctx, "user-1")
		require.NoError(t, err)
		clock.advance(80 * 24 * time.Hour)
		_, _, err = m.Resolve(ctx, value)
		require.NoError(t, err, "no idle expiry")
		clock.advance(10 * 24 * time.Hour)
		_, _, err = m.Resolve(ctx, value)
		assertUnauthenticated(t, err, "session: expired")
	})

	t.Run("max ttl", func(t *testing.T) {
		m, clock := newManager(t, Config{MaxTTL: Never, RotateAfter: Never})
		value, issued, err := m.Issue(ctx, "user-1")
		require.NoError(t, err)
		assert.True(t, issued.ExpiresAt.IsZero())
		for i := 0; i < 20; i++ {
			clock.advance(20 * 24 * time.Hour)
			_, _, err = m.Resolve(ctx, value)
			require.NoError(t, err, "no max expiry")
		}
		clock.advance(30 * 24 * time.Hour)
		_, _, err = m.Resolve(ctx, value)
		assertUnauthenticated(t, err, "session: expired")
	})

	t.Run("rotate after", func(t *testing.T) {
		m, clock := newManager(t, Config{RotateAfter: Never})
		value, _, err := m.Issue(ctx, "user-1")
		require.NoError(t, err)
		for i := 0; i < 5; i++ {
			clock.advance(10 * 24 * time.Hour)
			_, rotated, err := m.Resolve(ctx, value)
			require.NoError(t, err)
			assert.Empty(t, rotated, "never rotates")
		}
	})
}

func TestCookie(t *testing.T) {
	m, _ := newManager(t, Config{})

	cookie := m.Cookie("sid", "apps_x")
	assert.Equal(t, "sid", cookie.Name)
	assert.Equal(t, "apps_x", cookie.Value)
	assert.Equal(t, "/", cookie.Path)
	assert.Equal(t, 90*24*60*60, cookie.MaxAge, "MaxAge follows MaxTTL")
	assert.True(t, cookie.HttpOnly)
	assert.True(t, cookie.Secure)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)

	clear := m.ClearCookie("sid")
	assert.Equal(t, "sid", clear.Name)
	assert.Empty(t, clear.Value)
	assert.Equal(t, -1, clear.MaxAge)
	assert.Equal(t, "/", clear.Path)
	assert.True(t, clear.HttpOnly)
	assert.True(t, clear.Secure)
	assert.Equal(t, http.SameSiteLaxMode, clear.SameSite)

}

func TestCookieMaxAge(t *testing.T) {
	cases := map[string]struct {
		idleTTL time.Duration
		maxTTL  time.Duration
		maxAge  int
	}{
		"max ttl over idle ttl":          {idleTTL: time.Hour, maxTTL: 2 * time.Hour, maxAge: 2 * 60 * 60},
		"max ttl when idle ttl is never": {idleTTL: Never, maxTTL: 2 * time.Hour, maxAge: 2 * 60 * 60},
		"idle ttl when max ttl is never": {idleTTL: time.Hour, maxTTL: Never, maxAge: 60 * 60},
		"session cookie when both never": {idleTTL: Never, maxTTL: Never, maxAge: 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m, _ := newManager(t, Config{IdleTTL: c.idleTTL, MaxTTL: c.maxTTL})
			cookie := m.Cookie("sid", "v")
			assert.Equal(t, c.maxAge, cookie.MaxAge)
			assert.True(t, cookie.Expires.IsZero())
			if c.maxAge == 0 {
				assert.NotContains(t, cookie.String(), "Max-Age")
				assert.NotContains(t, cookie.String(), "Expires")
			}
		})
	}
}

func TestSetRotated(t *testing.T) {
	m, _ := newManager(t, Config{})

	w := httptest.NewRecorder()
	m.SetRotated(w, "sid", "apps_new", true)
	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	assert.Equal(t, "sid", cookies[0].Name)
	assert.Equal(t, "apps_new", cookies[0].Value)
	assert.True(t, cookies[0].HttpOnly)
	assert.Empty(t, w.Header().Get(TokenHeader))

	w = httptest.NewRecorder()
	m.SetRotated(w, "sid", "apps_new", false)
	assert.Equal(t, "apps_new", w.Header().Get("Session-Token"))
	assert.Empty(t, w.Header().Values("Set-Cookie"))

	w = httptest.NewRecorder()
	m.SetRotated(w, "sid", "", true)
	m.SetRotated(w, "sid", "", false)
	assert.Empty(t, w.Header())
}
