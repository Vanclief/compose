package oauth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

func TestMemoryStore(t *testing.T) {
	testStore(t, NewMemoryStore())
}

func TestMemoryStoreSweep(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore().(*memoryStore)
	require.NoError(t, store.Put(ctx, KindCode, "expired", []byte("v"), time.Now().Add(-time.Second)))
	require.NoError(t, store.Put(ctx, KindCode, "live", []byte("v"), time.Now().Add(time.Hour)))
	assert.Len(t, store.records, 2, "the second Put is within a minute of the first sweep")

	store.lastSweep = time.Now().Add(-2 * memorySweepInterval)
	require.NoError(t, store.Put(ctx, KindCode, "other", []byte("v"), time.Now().Add(time.Hour)))
	assert.Len(t, store.records, 2)
	_, ok := store.records[memoryKey(KindCode, "expired")]
	assert.False(t, ok, "a minute later the expired record is swept")
}

// testStore checks the Store contract against an empty store.
func testStore(t *testing.T, store Store) {
	ctx := context.Background()
	later := time.Now().Add(time.Hour)

	t.Run("put and get", func(t *testing.T) {
		value := []byte(`{"a":1}`)
		err := store.Put(ctx, KindClient, "round-trip", value, later)
		require.NoError(t, err)
		value[0] = 'x'

		got, err := store.Get(ctx, KindClient, "round-trip")
		require.NoError(t, err)
		assert.Equal(t, []byte(`{"a":1}`), got)

		got[0] = 'y'
		again, err := store.Get(ctx, KindClient, "round-trip")
		require.NoError(t, err)
		assert.Equal(t, []byte(`{"a":1}`), again)
	})

	t.Run("put empty value", func(t *testing.T) {
		err := store.Put(ctx, KindClient, "empty", nil, later)
		require.NoError(t, err)

		got, err := store.Get(ctx, KindClient, "empty")
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("put overwrites", func(t *testing.T) {
		err := store.Put(ctx, KindAccess, "overwrite", []byte("first"), later)
		require.NoError(t, err)
		err = store.Put(ctx, KindAccess, "overwrite", []byte("second"), later)
		require.NoError(t, err)

		got, err := store.Get(ctx, KindAccess, "overwrite")
		require.NoError(t, err)
		assert.Equal(t, []byte("second"), got)
	})

	t.Run("put extends an expired record", func(t *testing.T) {
		err := store.Put(ctx, KindAccess, "revived", []byte("old"), time.Now().Add(-time.Minute))
		require.NoError(t, err)
		err = store.Put(ctx, KindAccess, "revived", []byte("new"), later)
		require.NoError(t, err)

		got, err := store.Get(ctx, KindAccess, "revived")
		require.NoError(t, err)
		assert.Equal(t, []byte("new"), got)
	})

	t.Run("get missing", func(t *testing.T) {
		_, err := store.Get(ctx, KindCode, "missing")
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	})

	t.Run("expired records are absent", func(t *testing.T) {
		err := store.Put(ctx, KindCode, "expired", []byte("v"), time.Now().Add(-time.Second))
		require.NoError(t, err)

		_, err = store.Get(ctx, KindCode, "expired")
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
		_, err = store.Take(ctx, KindCode, "expired")
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	})

	t.Run("take removes", func(t *testing.T) {
		err := store.Put(ctx, KindCode, "take", []byte("code"), later)
		require.NoError(t, err)

		got, err := store.Take(ctx, KindCode, "take")
		require.NoError(t, err)
		assert.Equal(t, []byte("code"), got)

		_, err = store.Take(ctx, KindCode, "take")
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
		_, err = store.Get(ctx, KindCode, "take")
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	})

	t.Run("delete", func(t *testing.T) {
		err := store.Put(ctx, KindRefresh, "delete", []byte("v"), later)
		require.NoError(t, err)

		err = store.Delete(ctx, KindRefresh, "delete")
		require.NoError(t, err)
		_, err = store.Get(ctx, KindRefresh, "delete")
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))

		err = store.Delete(ctx, KindRefresh, "delete")
		assert.NoError(t, err)
		err = store.Delete(ctx, KindRefresh, "never-stored")
		assert.NoError(t, err)
	})

	t.Run("kinds are isolated", func(t *testing.T) {
		err := store.Put(ctx, KindAccess, "shared", []byte("access"), later)
		require.NoError(t, err)
		err = store.Put(ctx, KindRefresh, "shared", []byte("refresh"), later)
		require.NoError(t, err)

		got, err := store.Get(ctx, KindAccess, "shared")
		require.NoError(t, err)
		assert.Equal(t, []byte("access"), got)

		got, err = store.Take(ctx, KindRefresh, "shared")
		require.NoError(t, err)
		assert.Equal(t, []byte("refresh"), got)

		got, err = store.Get(ctx, KindAccess, "shared")
		require.NoError(t, err)
		assert.Equal(t, []byte("access"), got)

		err = store.Delete(ctx, KindAccess, "shared")
		require.NoError(t, err)
		_, err = store.Get(ctx, KindRequest, "shared")
		assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err))
	})

	t.Run("concurrent takes redeem once", func(t *testing.T) {
		err := store.Put(ctx, KindCode, "race", []byte("code"), later)
		require.NoError(t, err)

		const takers = 20
		errs := make([]error, takers)
		var wg sync.WaitGroup
		for i := 0; i < takers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = store.Take(ctx, KindCode, "race")
			}(i)
		}
		wg.Wait()

		succeeded := 0
		for _, err := range errs {
			if err == nil {
				succeeded++
				continue
			}
			assert.Equal(t, ez.ENOTFOUND, ez.ErrorCode(err), err)
		}
		assert.Equal(t, 1, succeeded)
	})
}
