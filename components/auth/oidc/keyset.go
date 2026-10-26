package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/vanclief/ez"
)

// maxDocumentBytes bounds the discovery and JWKS documents read from a
// provider.
const maxDocumentBytes = 1 << 20

const (
	// refreshInterval is the age at which the next call starts a refresh
	// behind it, so a key the provider removed stops verifying. Microsoft
	// asks for a refresh at least every 24 hours.
	refreshInterval = 24 * time.Hour
	// fetchCooldown is the least time between fetches. After one, a token
	// naming a kid the set does not publish is refused without a request
	// and a failure is returned again, so forged kids and an outage cannot
	// drive a fetch per call.
	fetchCooldown = time.Minute
	// staleLimit is how long the last good key set keeps serving while
	// fetches fail. Past it Verify fails with the fetch's error rather than
	// trust keys the provider may have removed.
	staleLimit = 7 * 24 * time.Hour
	// fetchTimeout caps a fetch. A fetch runs detached from the caller
	// that started it, so a caller hanging up neither stops the fetch its
	// waiters share nor skips the cooldown.
	fetchTimeout = 30 * time.Second
)

// keySet discovers, fetches and caches a provider's signing keys by kid.
// The package fetches keys itself so the JWKS "issuer" field Microsoft
// publishes on each key can be enforced.
//
// A fetch starts when a token names a kid the set does not publish or the
// set is older than refreshInterval, at most once per fetchCooldown. One
// fetch runs at a time per provider. A caller whose kid is cached is
// answered at once; the others wait for the fetch, each until its own ctx
// ends, and read its outcome. The last good keys keep serving through
// failed fetches until they are older than staleLimit.
type keySet struct {
	discover     func(ctx context.Context) (jwksURL string, err error)
	client       *http.Client
	now          func() time.Time
	onFetchError func(err error) // nil when the application does not listen

	mu        sync.Mutex
	jwksURL   string                  // set by discovery, cleared by a failed fetch; touched by the fetch goroutine only
	keys      map[string]publishedKey // the last set fetched successfully
	skipped   map[string]error        // kid to why its entry in that set cannot be used
	keysAt    time.Time               // when keys were fetched
	lastFetch time.Time               // when the latest fetch started
	lastErr   error                   // the latest finished fetch's failure, nil when it succeeded
	inflight  chan struct{}           // closed when the running fetch finishes, nil when none runs
}

type publishedKey struct {
	key    jose.JSONWebKey
	issuer string // the JWKS "issuer" field, empty when absent
}

// key returns the signing key for kid. A cached key younger than staleLimit
// is returned at once, with a refresh started behind it when one is due.
// Otherwise the caller waits for a fetch and gets ez.ENOTAUTHENTICATED when
// the key set does not publish kid, the fetch's error when it failed, or
// ez.EUNAVAILABLE when ctx ended while waiting for it.
func (k *keySet) key(ctx context.Context, kid string) (publishedKey, error) {
	k.mu.Lock()
	now := k.now()
	canFetch := k.inflight == nil && now.Sub(k.lastFetch) >= fetchCooldown
	key, ok := k.keys[kid]
	if ok && now.Sub(k.keysAt) < staleLimit {
		if canFetch && now.Sub(k.keysAt) >= refreshInterval {
			k.startFetch(now)
		}
		k.mu.Unlock()

		return key, nil
	}
	if canFetch {
		k.startFetch(now)
	}
	if k.inflight == nil {
		// The latest fetch, inside the cooldown, already answered: it
		// failed, or it did not publish kid.
		key, err := k.answer(kid, now)
		k.mu.Unlock()
		if err != nil {
			return publishedKey{}, ez.Wrap(err)
		}

		return key, nil
	}
	done := k.inflight
	k.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return publishedKey{}, ez.New(ez.EUNAVAILABLE, "oidc: key fetch cancelled", ctx.Err())
	}

	k.mu.Lock()
	key, err := k.answer(kid, k.now())
	k.mu.Unlock()
	if err != nil {
		return publishedKey{}, ez.Wrap(err)
	}

	return key, nil
}

// answer resolves kid from the latest outcome. Call it with mu held.
func (k *keySet) answer(kid string, now time.Time) (publishedKey, error) {
	key, ok := k.keys[kid]
	if ok && now.Sub(k.keysAt) < staleLimit {
		return key, nil
	}
	if k.lastErr != nil {
		return publishedKey{}, ez.Wrap(k.lastErr)
	}
	reason, skipped := k.skipped[kid]
	if skipped {
		return publishedKey{}, ez.New(ez.ENOTAUTHENTICATED, fmt.Sprintf("oidc: the published key for the token's kid %q cannot be used", kid), reason)
	}

	return publishedKey{}, ez.New(ez.ENOTAUTHENTICATED, "oidc: no signing key for the token's kid", nil)
}

// startFetch runs one fetch in its own goroutine and records its outcome.
// Call it with mu held and no fetch in flight.
func (k *keySet) startFetch(now time.Time) {
	done := make(chan struct{})
	k.inflight = done
	k.lastFetch = now
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		keys, skipped, err := k.fetch(ctx)
		if err != nil {
			k.report(err)
		} else {
			k.mu.Lock()
			k.keys = keys
			k.skipped = skipped
			k.keysAt = now
			k.mu.Unlock()
		}

		k.mu.Lock()
		k.lastErr = err
		k.inflight = nil
		k.mu.Unlock()
		close(done)
	}()
}

// report hands a failed fetch to the application, when it listens.
func (k *keySet) report(err error) {
	if k.onFetchError != nil {
		k.onFetchError(err)
	}
}

// fetch runs discovery when the JWKS URL is not known yet, then reads the
// JWKS. It returns the usable keys by kid and, by kid too, why the other
// entries were skipped: a key not meant for signatures, or one go-jose
// cannot read. One odd key does not hide the others, and a token naming it
// is refused with the reason. Entries with no kid are dropped, nothing can
// name them. A document with no usable key at all is a failed fetch, so a
// provider answering an error body with HTTP 200 does not empty the cache.
func (k *keySet) fetch(ctx context.Context) (map[string]publishedKey, map[string]error, error) {
	if k.jwksURL == "" {
		jwksURL, err := k.discover(ctx)
		if err != nil {
			return nil, nil, ez.Wrap(err)
		}
		k.jwksURL = jwksURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.jwksURL, nil)
	if err != nil {
		return nil, nil, ez.New(ez.EINTERNAL, "oidc: key fetch failed", err)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, nil, ez.New(ez.EUNAVAILABLE, "oidc: key fetch failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// The endpoint answered and does not serve the keys there any
			// more: discovery runs again next time. A transport failure
			// or a 5xx says nothing about the URL and keeps it.
			k.jwksURL = ""
		}

		return nil, nil, ez.New(ez.EUNAVAILABLE, fmt.Sprintf("oidc: key fetch answered HTTP %d", resp.StatusCode), nil)
	}
	// A body that stalls or breaks off is the provider not answering, like
	// a failed request; a body that does not parse is a bad document.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocumentBytes))
	if err != nil {
		return nil, nil, ez.New(ez.EUNAVAILABLE, "oidc: key fetch failed", err)
	}
	var document struct {
		Keys []json.RawMessage `json:"keys"`
	}
	err = json.Unmarshal(body, &document)
	if err != nil {
		return nil, nil, ez.New(ez.EINTERNAL, "oidc: key set is malformed", err)
	}

	keys := make(map[string]publishedKey, len(document.Keys))
	skipped := make(map[string]error)
	for _, raw := range document.Keys {
		var meta struct {
			KeyID  string `json:"kid"`
			Use    string `json:"use"`
			Issuer string `json:"issuer"`
		}
		err = json.Unmarshal(raw, &meta)
		if err != nil || meta.KeyID == "" {
			continue
		}
		if meta.Use != "" && meta.Use != "sig" {
			skipped[meta.KeyID] = ez.New(ez.ENOTAUTHENTICATED, fmt.Sprintf("key use is %q", meta.Use), nil)
			continue
		}
		var key jose.JSONWebKey
		err = json.Unmarshal(raw, &key)
		if err != nil {
			skipped[meta.KeyID] = err
			continue
		}
		keys[meta.KeyID] = publishedKey{key: key, issuer: meta.Issuer}
	}
	if len(keys) == 0 {
		return nil, nil, ez.New(ez.EINTERNAL, fmt.Sprintf("oidc: key set has no usable keys (%d skipped)", len(skipped)), nil)
	}

	return keys, skipped, nil
}
