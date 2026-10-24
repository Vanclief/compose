package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// hookStore runs a function once at a step of a Store call, such as before
// the Swap of a family record or after the Get of a refresh token, so a test
// can run a second request at a precise step of the first one. A before hook
// that returns an error fails the call without running it. An after hook runs
// once the call is done, whatever its result.
type hookStore struct {
	Store
	mu          sync.Mutex
	beforeHooks map[string]func() error
	afterHooks  map[string]func()
}

func newHookStore(store Store) *hookStore {
	return &hookStore{Store: store, beforeHooks: map[string]func() error{}, afterHooks: map[string]func(){}}
}

// before arms fn for the next call of op ("Put", "Get", "Take", "Swap" or
// "Delete") on kind, given as "Swap family".
func (h *hookStore) before(call string, fn func() error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beforeHooks[call] = fn
}

// after arms fn to run after the next call, given as "Get refresh".
func (h *hookStore) after(call string, fn func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.afterHooks[call] = fn
}

// runBefore fires and disarms the before hook of call, if any.
func (h *hookStore) runBefore(call string) error {
	h.mu.Lock()
	fn := h.beforeHooks[call]
	delete(h.beforeHooks, call)
	h.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}

// runAfter fires and disarms the after hook of call, if any.
func (h *hookStore) runAfter(call string) {
	h.mu.Lock()
	fn := h.afterHooks[call]
	delete(h.afterHooks, call)
	h.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (h *hookStore) Put(ctx context.Context, kind, id string, value []byte, expiresAt time.Time) error {
	err := h.runBefore("Put " + kind)
	if err != nil {
		return err
	}
	err = h.Store.Put(ctx, kind, id, value, expiresAt)
	h.runAfter("Put " + kind)
	return err
}

func (h *hookStore) Get(ctx context.Context, kind, id string) ([]byte, error) {
	err := h.runBefore("Get " + kind)
	if err != nil {
		return nil, err
	}
	value, err := h.Store.Get(ctx, kind, id)
	h.runAfter("Get " + kind)
	return value, err
}

func (h *hookStore) Take(ctx context.Context, kind, id string) ([]byte, error) {
	err := h.runBefore("Take " + kind)
	if err != nil {
		return nil, err
	}
	value, err := h.Store.Take(ctx, kind, id)
	h.runAfter("Take " + kind)
	return value, err
}

func (h *hookStore) Swap(ctx context.Context, kind, id string, old, value []byte, expiresAt time.Time) error {
	err := h.runBefore("Swap " + kind)
	if err != nil {
		return err
	}
	err = h.Store.Swap(ctx, kind, id, old, value, expiresAt)
	h.runAfter("Swap " + kind)
	return err
}

func (h *hookStore) Delete(ctx context.Context, kind, id string) error {
	err := h.runBefore("Delete " + kind)
	if err != nil {
		return err
	}
	err = h.Store.Delete(ctx, kind, id)
	h.runAfter("Delete " + kind)
	return err
}

// deadlineStore fails every call whose context is done, as a database driver
// that honours deadlines does, since the memory store ignores them.
type deadlineStore struct {
	Store
}

func (d deadlineStore) Put(ctx context.Context, kind, id string, value []byte, expiresAt time.Time) error {
	err := ctx.Err()
	if err != nil {
		return err
	}
	return d.Store.Put(ctx, kind, id, value, expiresAt)
}

func (d deadlineStore) Get(ctx context.Context, kind, id string) ([]byte, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}
	return d.Store.Get(ctx, kind, id)
}

func (d deadlineStore) Take(ctx context.Context, kind, id string) ([]byte, error) {
	err := ctx.Err()
	if err != nil {
		return nil, err
	}
	return d.Store.Take(ctx, kind, id)
}

func (d deadlineStore) Swap(ctx context.Context, kind, id string, old, value []byte, expiresAt time.Time) error {
	err := ctx.Err()
	if err != nil {
		return err
	}
	return d.Store.Swap(ctx, kind, id, old, value, expiresAt)
}

func (d deadlineStore) Delete(ctx context.Context, kind, id string) error {
	err := ctx.Err()
	if err != nil {
		return err
	}
	return d.Store.Delete(ctx, kind, id)
}

// storeDown is a before hook that fails the Store call.
func storeDown() error {
	return ez.New(ez.EUNAVAILABLE, "store is down", nil)
}

// hookEnv is a test server on a hookStore over a memory store.
type hookEnv struct {
	*testEnv
	memory   *memoryStore
	hooks    *hookStore
	mu       sync.Mutex
	reported []error
}

func newHookEnv(t *testing.T, mutate func(*Config)) *hookEnv {
	t.Helper()
	env := &hookEnv{memory: NewMemoryStore().(*memoryStore)}
	env.hooks = newHookStore(env.memory)
	env.testEnv = newEnv(t, func(c *Config) {
		c.Store = env.hooks
		c.OnError = func(ctx context.Context, err error) {
			env.mu.Lock()
			defer env.mu.Unlock()
			env.reported = append(env.reported, err)
		}
		if mutate != nil {
			mutate(c)
		}
	}, nil)
	return env
}

// refresh runs a refresh_token grant synchronously in the calling goroutine,
// so it can run inside a hook of another request.
func (env *hookEnv) refresh(refreshToken string) response {
	return env.refreshContext(context.Background(), refreshToken)
}

// refreshContext is refresh on a request with context ctx.
func (env *hookEnv) refreshContext(ctx context.Context, refreshToken string) response {
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {"public"}, "refresh_token": {refreshToken}}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	env.s.Handler().ServeHTTP(rec, req)
	return response{status: rec.Code, header: rec.Header(), body: rec.Body.String()}
}

// rotate refreshes refreshToken, requires success and returns the new access
// and refresh tokens.
func (env *hookEnv) rotate(t *testing.T, refreshToken string) (string, string) {
	t.Helper()
	res := env.refresh(refreshToken)
	require.Equal(t, http.StatusOK, res.status, res.body)
	body := res.json(t)
	return body["access_token"].(string), body["refresh_token"].(string)
}

// token returns the record of a stored access or refresh token.
func (env *hookEnv) token(t *testing.T, kind, value string) tokenRecord {
	t.Helper()
	var record tokenRecord
	require.NoError(t, env.s.getRecord(context.Background(), kind, hashID(value), false, &record))
	return record
}

// family returns the family id of a stored refresh token.
func (env *hookEnv) family(t *testing.T, refreshToken string) string {
	t.Helper()
	return env.token(t, KindRefresh, refreshToken).Family
}

// familyRecord returns the live family row under id.
func (env *hookEnv) familyRecord(t *testing.T, id string) familyRecord {
	t.Helper()
	family, _, err := env.s.getFamily(context.Background(), id)
	require.NoError(t, err)
	return family
}

// has reports whether the Store holds a record under kind and id.
func (env *hookEnv) has(kind, id string) bool {
	env.memory.mu.Lock()
	defer env.memory.mu.Unlock()
	_, ok := env.memory.records[memoryKey(kind, id)]
	return ok
}

// rows returns the store ids of the token rows of kind that point at family
// and generation.
func (env *hookEnv) rows(t *testing.T, kind, family string, generation int64) []string {
	t.Helper()
	env.memory.mu.Lock()
	defer env.memory.mu.Unlock()
	var ids []string
	for key, stored := range env.memory.records {
		rowKind, id, _ := strings.Cut(key, "\x00")
		if rowKind != kind {
			continue
		}
		var record tokenRecord
		require.NoError(t, json.Unmarshal(stored.value, &record))
		if record == (tokenRecord{Family: family, Generation: generation}) {
			ids = append(ids, id)
		}
	}
	return ids
}

// expiry returns the Store expiry of the record under kind and id.
func (env *hookEnv) expiry(t *testing.T, kind, id string) time.Time {
	t.Helper()
	env.memory.mu.Lock()
	defer env.memory.mu.Unlock()
	record, ok := env.memory.records[memoryKey(kind, id)]
	require.True(t, ok, "no %s record", kind)
	return record.expiresAt
}

// reuseReports returns the reuse reports sent to OnError.
func (env *hookEnv) reuseReports() []error {
	env.mu.Lock()
	defer env.mu.Unlock()
	var reuses []error
	for _, err := range env.reported {
		if ez.ErrorCode(err) == ez.ENOTAUTHORIZED {
			reuses = append(reuses, err)
		}
	}
	return reuses
}

// assertReuseReported checks that exactly one reuse of family was reported,
// naming the family and the client.
func (env *hookEnv) assertReuseReported(t *testing.T, family string) {
	t.Helper()
	reuses := env.reuseReports()
	require.Len(t, reuses, 1, "the reuse is reported once")
	assert.Contains(t, reuses[0].Error(), "reuse detected")
	assert.Contains(t, reuses[0].Error(), "family "+family)
	assert.Contains(t, reuses[0].Error(), "client public")
}

// assertRefused checks that an access token is refused by Verify, AuthFunc
// and Protect, and that a refresh token is answered invalid_grant.
func (env *hookEnv) assertRefused(t *testing.T, access, refresh string) {
	t.Helper()
	ctx := context.Background()
	_, err := env.s.Verify(ctx, access)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "Verify refuses the access token")
	_, err = env.s.AuthFunc()(ctx, access)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "AuthFunc refuses the access token")
	assert.Equal(t, http.StatusUnauthorized, env.bearer(t, access).status, "Protect refuses the access token")
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, env.refresh(refresh)), "the refresh token is refused")
}

// TestRefreshRotates: a rotation moves the family to the next generation and
// answers a working pair; the presented refresh token no longer rotates, and
// the access token issued before stays valid until it expires.
func TestRefreshRotates(t *testing.T) {
	ctx := context.Background()
	env := newHookEnv(t, nil)
	access0, refresh0 := env.tokens(t)
	id := env.family(t, refresh0)
	before := env.familyRecord(t, id)
	assert.Equal(t, int64(1), before.Generation)

	res := env.refresh(refresh0)
	require.Equal(t, http.StatusOK, res.status, res.body)
	body := res.json(t)
	assert.Equal(t, "mcp", body["scope"], "the requested scope survives the rotation")
	assert.Equal(t, "no-store", res.header.Get("Cache-Control"))
	assert.Equal(t, "no-cache", res.header.Get("Pragma"))
	access1 := body["access_token"].(string)
	refresh1 := body["refresh_token"].(string)

	after := env.familyRecord(t, id)
	assert.Equal(t, int64(2), after.Generation, "the generation increments")
	assert.True(t, before.ExpiresAt.Equal(after.ExpiresAt), "the family keeps its expiry")
	assert.Equal(t, before.Grant, after.Grant)
	assert.Equal(t, before.RequestedScope, after.RequestedScope)
	assert.Equal(t, "public", after.ClientID)
	assert.Equal(t, tokenRecord{Family: id, Generation: 2}, env.token(t, KindRefresh, refresh1))
	assert.Equal(t, tokenRecord{Family: id, Generation: 2}, env.token(t, KindAccess, access1))
	assert.True(t, after.ExpiresAt.Equal(env.expiry(t, KindRefresh, hashID(refresh1))), "the new refresh token expires with the family")

	grant, err := env.s.Verify(ctx, access1)
	require.NoError(t, err)
	assert.Equal(t, issuedGrant, grant)
	assert.Equal(t, http.StatusOK, env.bearer(t, access1).status)
	_, err = env.s.Verify(ctx, access0)
	assert.NoError(t, err, "the access token issued before the rotation lives until it expires")

	_, refresh2 := env.rotate(t, refresh1)
	assert.Equal(t, int64(3), env.familyRecord(t, id).Generation)
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, env.refresh(refresh0)), "a rotated token no longer rotates")
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, env.refresh(refresh2)), "and its replay revoked the family")
}

// TestRefreshReuseRevokesFamily: presenting a rotated refresh token deletes
// its family, so every token of the family is refused, and the reuse is
// reported once. Another connection of the same user is not affected.
func TestRefreshReuseRevokesFamily(t *testing.T) {
	ctx := context.Background()
	env := newHookEnv(t, nil)
	otherAccess, otherRefresh := env.tokens(t)

	access0, refresh0 := env.tokens(t)
	id := env.family(t, refresh0)
	access1, refresh1 := env.rotate(t, refresh0)

	res := env.refresh(refresh0)
	assert.Equal(t, http.StatusBadRequest, res.status, res.body)
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, res))
	assert.False(t, env.has(KindFamily, id), "the family row is deleted")
	env.assertRefused(t, access1, refresh1)
	_, err := env.s.Verify(ctx, access0)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "older access tokens die with the family")
	env.assertReuseReported(t, id)

	assert.Equal(t, "invalid_grant", oauthErrorCode(t, env.refresh(refresh0)), "a second replay finds no family")
	assert.Len(t, env.reuseReports(), 1, "a replay after the revocation is not reported again")

	_, err = env.s.Verify(ctx, otherAccess)
	assert.NoError(t, err)
	env.rotate(t, otherRefresh)
}

// TestRefreshPresentedConcurrently: when two requests present the same
// refresh token at once, one wins the Swap and the other, finding the family
// at the next generation, revokes it, so nothing of the family works
// afterwards.
func TestRefreshPresentedConcurrently(t *testing.T) {
	for _, call := range []string{"Get family", "Put access", "Swap family"} {
		t.Run("second request before "+call, func(t *testing.T) {
			env := newHookEnv(t, nil)
			access, refresh := env.tokens(t)
			id := env.family(t, refresh)

			var second response
			env.hooks.before(call, func() error {
				second = env.refresh(refresh)
				return nil
			})
			first := env.refresh(refresh)

			require.Equal(t, http.StatusOK, second.status, "the second request wins the Swap: "+second.body)
			assert.Equal(t, "invalid_grant", oauthErrorCode(t, first), "the first request loses the Swap")
			assert.False(t, env.has(KindFamily, id))
			body := second.json(t)
			env.assertRefused(t, body["access_token"].(string), body["refresh_token"].(string))
			env.assertRefused(t, access, refresh)
			env.assertReuseReported(t, id)
		})
	}
}

// TestRefreshRevokedDuringRotation: a family deleted between the read and the
// Swap of a rotation fails the Swap; the re-read finds no family, so the
// refresh is invalid_grant and the reuse is reported only once, by the
// request that revoked.
func TestRefreshRevokedDuringRotation(t *testing.T) {
	env := newHookEnv(t, nil)
	_, refresh0 := env.tokens(t)
	id := env.family(t, refresh0)
	access1, refresh1 := env.rotate(t, refresh0)

	var reuse response
	env.hooks.before("Swap family", func() error {
		reuse = env.refresh(refresh0)
		return nil
	})
	outer := env.refresh(refresh1)

	assert.Equal(t, "invalid_grant", oauthErrorCode(t, reuse))
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, outer))
	env.assertRefused(t, access1, refresh1)
	env.assertReuseReported(t, id)
}

// TestRefreshRevocationFails: when the family cannot be deleted on reuse, the
// answer is server_error, not invalid_grant, and the family stays; the retry
// repeats the detection and revokes.
func TestRefreshRevocationFails(t *testing.T) {
	t.Run("on replay", func(t *testing.T) {
		env := newHookEnv(t, nil)
		_, refresh0 := env.tokens(t)
		id := env.family(t, refresh0)
		access1, refresh1 := env.rotate(t, refresh0)

		env.hooks.before("Delete family", storeDown)
		res := env.refresh(refresh0)
		assert.Equal(t, http.StatusInternalServerError, res.status, res.body)
		assert.Equal(t, "server_error", oauthErrorCode(t, res))
		assert.True(t, env.has(KindFamily, id), "the family is still present")
		assert.Empty(t, env.reuseReports(), "a revocation that failed is not reported as done")

		assert.Equal(t, "invalid_grant", oauthErrorCode(t, env.refresh(refresh0)), "the retry revokes")
		assert.False(t, env.has(KindFamily, id))
		env.assertRefused(t, access1, refresh1)
		env.assertReuseReported(t, id)
	})

	t.Run("on a lost Swap", func(t *testing.T) {
		env := newHookEnv(t, nil)
		_, refresh := env.tokens(t)
		id := env.family(t, refresh)

		var second response
		env.hooks.before("Swap family", func() error {
			second = env.refresh(refresh)
			env.hooks.before("Delete family", storeDown)
			return nil
		})
		first := env.refresh(refresh)

		require.Equal(t, http.StatusOK, second.status, second.body)
		assert.Equal(t, "server_error", oauthErrorCode(t, first))
		assert.True(t, env.has(KindFamily, id), "the family is still present")

		assert.Equal(t, "invalid_grant", oauthErrorCode(t, env.refresh(refresh)), "the retry revokes")
		body := second.json(t)
		env.assertRefused(t, body["access_token"].(string), body["refresh_token"].(string))
		env.assertReuseReported(t, id)
	})
}

// TestRefreshFailsBeforeCommit: a Store failure before or at the Swap answers
// server_error and leaves the presented token as it was, so the retry with
// the same token rotates it.
func TestRefreshFailsBeforeCommit(t *testing.T) {
	for _, call := range []string{"Get refresh", "Get family", "Put access", "Put refresh", "Swap family"} {
		t.Run(call+" fails", func(t *testing.T) {
			env := newHookEnv(t, nil)
			_, refresh := env.tokens(t)
			id := env.family(t, refresh)

			env.hooks.before(call, storeDown)
			res := env.refresh(refresh)
			assert.Equal(t, http.StatusInternalServerError, res.status, res.body)
			assert.Equal(t, "server_error", oauthErrorCode(t, res))
			assert.Equal(t, int64(1), env.familyRecord(t, id).Generation, "the family did not move")

			access, _ := env.rotate(t, refresh)
			_, err := env.s.Verify(context.Background(), access)
			assert.NoError(t, err)
			assert.Equal(t, int64(2), env.familyRecord(t, id).Generation)
			assert.Empty(t, env.reuseReports())
		})
	}
}

// TestRefreshClientGoneBeforeSwap: a request whose client is gone once the
// token rows are written commits nothing and answers nothing. The family
// stays at its generation and keeps naming the presented token, so the rows
// written are inert, and the retry with the same token rotates without a
// reuse report. A client gone once the Swap has started still commits.
func TestRefreshClientGoneBeforeSwap(t *testing.T) {
	t.Run("after the token rows", func(t *testing.T) {
		env := newHookEnv(t, nil)
		_, refresh := env.tokens(t)
		id := env.family(t, refresh)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env.hooks.after("Put refresh", cancel)

		res := env.refreshContext(ctx, refresh)
		assert.Empty(t, res.body, "no response, and no token, is written for a client that is gone")
		family := env.familyRecord(t, id)
		assert.Equal(t, int64(1), family.Generation, "the rotation did not commit")
		assert.Equal(t, hashID(refresh), family.Refresh, "the family still names the presented token")
		// The leftovers are inert: the access row is of a later generation
		// than the family's, and the family does not name the refresh row
		// (TestUncommittedRowsAreInert presents such rows).
		require.Len(t, env.rows(t, KindAccess, id, 2), 1, "the access row was written")
		leftover := env.rows(t, KindRefresh, id, 2)
		require.Len(t, leftover, 1, "the refresh row was written")

		access, _ := env.rotate(t, refresh)
		_, err := env.s.Verify(context.Background(), access)
		assert.NoError(t, err)
		family = env.familyRecord(t, id)
		assert.Equal(t, int64(2), family.Generation)
		assert.NotEqual(t, leftover[0], family.Refresh, "the leftover refresh row is never named")
		assert.Empty(t, env.reuseReports())
	})

	t.Run("during the Swap", func(t *testing.T) {
		env := newHookEnv(t, nil)
		_, refresh := env.tokens(t)
		id := env.family(t, refresh)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		env.hooks.before("Swap family", func() error {
			cancel()
			return nil
		})

		res := env.refreshContext(ctx, refresh)
		require.Equal(t, http.StatusOK, res.status, "the started Swap runs to the end: "+res.body)
		assert.Equal(t, int64(2), env.familyRecord(t, id).Generation)
		env.rotate(t, res.json(t)["refresh_token"].(string))
	})
}

// TestRefreshTimeout: a rotation that runs out of RefreshTimeout before the
// Swap answers server_error, and the token still rotates afterwards.
func TestRefreshTimeout(t *testing.T) {
	env := newHookEnv(t, func(c *Config) {
		c.RefreshTimeout = 100 * time.Millisecond
	})
	env.hooks.Store = deadlineStore{env.memory}
	_, refresh := env.tokens(t)

	env.hooks.before("Put access", func() error {
		time.Sleep(150 * time.Millisecond)
		return nil
	})
	res := env.refresh(refresh)
	assert.Equal(t, "server_error", oauthErrorCode(t, res), res.body)

	env.rotate(t, refresh)
	assert.Empty(t, env.reuseReports())
}

// TestRefreshFamilyMissing: a refresh token whose family expired or was
// deleted is invalid_grant, without a reuse report.
func TestRefreshFamilyMissing(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(env *hookEnv, id string){
		"deleted": func(env *hookEnv, id string) {
			require.NoError(t, env.memory.Delete(ctx, KindFamily, id))
		},
		"expired": func(env *hookEnv, id string) {
			raw, err := env.memory.Get(ctx, KindFamily, id)
			require.NoError(t, err)
			require.NoError(t, env.memory.Put(ctx, KindFamily, id, raw, time.Now().Add(-time.Second)))
		},
	}
	for name, remove := range cases {
		t.Run(name, func(t *testing.T) {
			env := newHookEnv(t, nil)
			access, refresh := env.tokens(t)
			remove(env, env.family(t, refresh))

			env.assertRefused(t, access, refresh)
			assert.Empty(t, env.reuseReports())
			env.mu.Lock()
			defer env.mu.Unlock()
			assert.Empty(t, env.reported, "a missing family is not an error")
		})
	}
}

// TestRefreshAfterRestartWithOtherTTLs: a family issued under Never keeps
// sliding by the IdleTTL it was issued with when the server restarts on the
// same Store with other TTLs. It still rotates and detects a replay.
func TestRefreshAfterRestartWithOtherTTLs(t *testing.T) {
	env := newHookEnv(t, func(c *Config) {
		c.RefreshTokenTTL = Never
		c.IdleTTL = 4 * time.Hour
	})
	_, refresh0 := env.tokens(t)
	id := env.family(t, refresh0)
	_, refresh1 := env.rotate(t, refresh0)
	issued := env.expiry(t, KindRefresh, hashID(refresh1))

	restarted := newHookEnv(t, func(c *Config) {
		c.Store = env.hooks
		c.RefreshTokenTTL = time.Hour
		c.AccessTokenTTL = time.Minute
		c.IdleTTL = 2 * time.Hour
	})
	access2, refresh2 := restarted.rotate(t, refresh1)
	family := env.familyRecord(t, id)
	assert.Equal(t, 4*time.Hour, family.Idle, "the family keeps the idle window it was issued with")
	assert.Equal(t, int64(3), family.Generation)
	assert.WithinDuration(t, time.Now().Add(4*time.Hour), family.ExpiresAt, 5*time.Second,
		"the rotation slides the family by the stored window, not the new IdleTTL or RefreshTokenTTL")
	assert.WithinDuration(t, time.Now().Add(4*time.Hour), env.expiry(t, KindRefresh, hashID(refresh2)), 5*time.Second)
	assert.WithinDuration(t, time.Now().Add(time.Minute), env.expiry(t, KindAccess, hashID(access2)), 5*time.Second)
	assert.True(t, issued.Equal(env.expiry(t, KindRefresh, hashID(refresh1))), "the rotated token keeps the expiry it was issued with")

	assert.Equal(t, "invalid_grant", oauthErrorCode(t, restarted.refresh(refresh1)))
	assert.False(t, env.has(KindFamily, id), "the replay revoked the family")
	restarted.assertRefused(t, access2, refresh2)
	restarted.assertReuseReported(t, id)
}

// TestRotatedTokenKeepsItsExpiry: a rotated refresh token is not written
// again. It keeps the expiry it was issued with, until the family expires
// under a finite RefreshTokenTTL and IdleTTL after its issue under Never, and
// is detected as reuse until then; under Never the family itself moves on to
// IdleTTL from the rotation.
func TestRotatedTokenKeepsItsExpiry(t *testing.T) {
	t.Run("never", func(t *testing.T) {
		env := newHookEnv(t, func(c *Config) {
			c.RefreshTokenTTL = Never
			c.IdleTTL = 2 * time.Hour
		})
		_, refresh0 := env.tokens(t)
		id := env.family(t, refresh0)
		issued := env.expiry(t, KindRefresh, hashID(refresh0))
		assert.WithinDuration(t, time.Now().Add(2*time.Hour), issued, 5*time.Second)
		_, refresh1 := env.rotate(t, refresh0)

		assert.True(t, issued.Equal(env.expiry(t, KindRefresh, hashID(refresh0))))
		assert.WithinDuration(t, time.Now().Add(2*time.Hour), env.expiry(t, KindFamily, id), 5*time.Second)
		assert.WithinDuration(t, time.Now().Add(2*time.Hour), env.expiry(t, KindRefresh, hashID(refresh1)), 5*time.Second)
		assert.Equal(t, "invalid_grant", oauthErrorCode(t, env.refresh(refresh0)))
		env.assertReuseReported(t, id)
	})

	t.Run("finite", func(t *testing.T) {
		env := newHookEnv(t, func(c *Config) {
			c.RefreshTokenTTL = 3 * time.Hour
			c.IdleTTL = time.Minute
		})
		_, refresh0 := env.tokens(t)
		id := env.family(t, refresh0)
		env.rotate(t, refresh0)

		family := env.familyRecord(t, id)
		assert.Equal(t, time.Duration(0), family.Idle)
		rotated := env.expiry(t, KindRefresh, hashID(refresh0))
		assert.True(t, family.ExpiresAt.Equal(rotated), "the rotated token expires with the family: %s, %s", family.ExpiresAt, rotated)
		assert.True(t, family.ExpiresAt.Equal(env.expiry(t, KindFamily, id)), "a finite family keeps its expiry")
	})
}

// TestAccessExpiryCappedAtFamily: an access token never outlives its family,
// at the code exchange and at a rotation, and expires_in says so.
func TestAccessExpiryCappedAtFamily(t *testing.T) {
	env := newHookEnv(t, func(c *Config) {
		c.RefreshTokenTTL = time.Hour
		c.AccessTokenTTL = 3 * time.Hour
	})
	verifier, challenge := pkce("a")
	res := env.exchange(t, env.codeFor(t, "public", challenge), verifier)
	require.Equal(t, http.StatusOK, res.status, res.body)
	body := res.json(t)
	access0 := body["access_token"].(string)
	refresh0 := body["refresh_token"].(string)
	assert.InDelta(t, time.Hour.Seconds(), body["expires_in"], 1)
	family := env.familyRecord(t, env.family(t, refresh0))
	assert.WithinDuration(t, family.ExpiresAt, env.expiry(t, KindAccess, hashID(access0)), time.Millisecond)

	res = env.refresh(refresh0)
	require.Equal(t, http.StatusOK, res.status, res.body)
	body = res.json(t)
	assert.InDelta(t, time.Hour.Seconds(), body["expires_in"], 1)
	assert.WithinDuration(t, family.ExpiresAt, env.expiry(t, KindAccess, hashID(body["access_token"].(string))), time.Millisecond)
}

// TestCodeExchangeCommitsWithFamily: the code exchange writes the token rows
// first and the family row last, at generation 1. Token rows without their
// family are inert.
func TestCodeExchangeCommitsWithFamily(t *testing.T) {
	ctx := context.Background()

	t.Run("success", func(t *testing.T) {
		env := newHookEnv(t, nil)
		access, refresh := env.tokens(t)
		id := env.family(t, refresh)
		assert.Equal(t, tokenRecord{Family: id, Generation: 1}, env.token(t, KindAccess, access))
		assert.Equal(t, tokenRecord{Family: id, Generation: 1}, env.token(t, KindRefresh, refresh))
		family := env.familyRecord(t, id)
		assert.Equal(t, familyRecord{ClientID: "public", Grant: issuedGrant, RequestedScope: "mcp", ExpiresAt: family.ExpiresAt, Generation: 1, Refresh: hashID(refresh)}, family)
		assert.WithinDuration(t, time.Now().Add(30*24*time.Hour), family.ExpiresAt, 5*time.Second)
	})

	t.Run("family write fails", func(t *testing.T) {
		env := newHookEnv(t, nil)
		var rows []tokenRecord
		env.hooks.before("Put family", func() error {
			env.memory.mu.Lock()
			defer env.memory.mu.Unlock()
			for key, stored := range env.memory.records {
				kind, _, _ := strings.Cut(key, "\x00")
				if kind == KindAccess || kind == KindRefresh {
					var record tokenRecord
					require.NoError(t, json.Unmarshal(stored.value, &record))
					rows = append(rows, record)
				}
			}
			return storeDown()
		})
		verifier, challenge := pkce("a")
		res := env.exchange(t, env.codeFor(t, "public", challenge), verifier)
		assert.Equal(t, http.StatusInternalServerError, res.status, res.body)
		assert.Equal(t, "server_error", oauthErrorCode(t, res))

		require.Len(t, rows, 2, "both token rows are written before the family")
		for _, row := range rows {
			assert.Equal(t, int64(1), row.Generation)
			assert.False(t, env.has(KindFamily, row.Family), "the family was never stored")
		}
	})

	t.Run("token rows without family are inert", func(t *testing.T) {
		env := newHookEnv(t, nil)
		family := familyRecord{ClientID: "public", Grant: issuedGrant, ExpiresAt: time.Now().Add(time.Hour), Generation: 1}
		pair, err := env.s.putTokens(ctx, randomID(), family)
		require.NoError(t, err)
		env.assertRefused(t, pair.access, pair.refresh)
		assert.Empty(t, env.reuseReports())
	})
}

// TestFamilyUnreadable: a Store failure while reading the family of an access
// token is a server error, never an accepted token.
func TestFamilyUnreadable(t *testing.T) {
	ctx := context.Background()
	env := newHookEnv(t, nil)
	access, _ := env.tokens(t)

	env.hooks.before("Get family", storeDown)
	_, err := env.s.Verify(ctx, access)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))

	env.hooks.before("Get family", storeDown)
	res := env.bearer(t, access)
	assert.Equal(t, http.StatusInternalServerError, res.status, res.body)
	assert.NotContains(t, res.body, "store is down")

	env.hooks.before("Get family", storeDown)
	_, err = env.s.AuthFunc()(ctx, access)
	assert.Equal(t, ez.EINTERNAL, ez.ErrorCode(err))

	_, err = env.s.Verify(ctx, access)
	assert.NoError(t, err, "the token is fine once the Store is back")
}

// TestUncommittedRowsAreInert: the rows a rotation writes before its Swap,
// which a failed rotation leaves behind, are refused while the family is at
// an earlier generation, and the refresh row never rotates, since the family
// does not name it. Once a retry commits the same generation, the leftover
// access row is accepted: it was never issued to a client, so nothing is
// exposed.
func TestUncommittedRowsAreInert(t *testing.T) {
	ctx := context.Background()
	env := newHookEnv(t, nil)
	_, refresh := env.tokens(t)
	id := env.family(t, refresh)
	next := env.familyRecord(t, id)
	next.Generation++
	leftover, err := env.s.putTokens(ctx, id, next)
	require.NoError(t, err)

	_, err = env.s.Verify(ctx, leftover.access)
	assert.Equal(t, ez.ENOTAUTHENTICATED, ez.ErrorCode(err), "an access row of a later generation is refused")

	access, _ := env.rotate(t, refresh)
	_, err = env.s.Verify(ctx, access)
	require.NoError(t, err)
	// The retry committed the leftover's generation, so its access row is
	// accepted now. It was never issued to a client, so nothing is exposed.
	_, err = env.s.Verify(ctx, leftover.access)
	assert.NoError(t, err, "an access row of the committed generation is accepted")
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, env.refresh(leftover.refresh)), "a refresh row the family does not name never rotates")
}

// commitThenFail is a Store whose next Swap runs and then reports a failure,
// as a Swap that commits and then runs out of time does.
type commitThenFail struct {
	Store
	armed bool
}

func (c *commitThenFail) Swap(ctx context.Context, kind, id string, old, value []byte, expiresAt time.Time) error {
	err := c.Store.Swap(ctx, kind, id, old, value, expiresAt)
	if err != nil || !c.armed {
		return err
	}
	c.armed = false
	return ez.New(ez.EUNAVAILABLE, "store timed out after committing", nil)
}

// TestSwapCommitsThenFails: a rotation whose Swap committed before failing is
// answered as a success, so the client does not retry with a rotated token
// and lose the connection; the failure is only reported.
func TestSwapCommitsThenFails(t *testing.T) {
	env := newHookEnv(t, nil)
	store := &commitThenFail{Store: env.memory}
	env.hooks.Store = store
	_, refresh0 := env.tokens(t)
	id := env.family(t, refresh0)

	store.armed = true
	access1, refresh1 := env.rotate(t, refresh0)
	assert.Equal(t, int64(2), env.familyRecord(t, id).Generation)
	assert.Empty(t, env.reuseReports())
	env.mu.Lock()
	reported := len(env.reported)
	env.mu.Unlock()
	assert.Equal(t, 1, reported, "the Swap failure is reported")

	_, err := env.s.Verify(context.Background(), access1)
	assert.NoError(t, err)
	env.rotate(t, refresh1)
	assert.Equal(t, "invalid_grant", oauthErrorCode(t, env.refresh(refresh0)), "the rotated token is dead")
}
