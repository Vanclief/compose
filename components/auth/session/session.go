// Package session issues and resolves rotating session tokens for
// first-party apps.
//
// A session is one opaque token "<prefix>s_<body>" (see package token). The
// token may arrive in a cookie (web) or in the Authorization header (mobile
// keychain). Resolve is transport-agnostic: it takes the bare token value and
// never looks at the HTTP request. Only the cookie path runs the CSRF rule in
// requests.Authenticate, which AuthenticatedRequest.FromCookie distinguishes.
// When Resolve rotates the token, SetRotated hands the new value back: as a
// fresh cookie when the request came with one, else in the Session-Token
// response header (TokenHeader) for clients that keep the token themselves.
//
// A session expires when unused for IdleTTL or MaxTTL after creation. Resolve
// deletes an expired session it meets; Sweep deletes the rest, and nothing
// calls it automatically. On the first use after RotateAfter the token is
// replaced and the old one becomes the previous token. The previous token
// keeps working for Grace after the rotation while the new token is unused,
// and for Grace after the new token's first use once it is used, so requests
// already in flight still succeed; a rotation due before that waits for Grace
// to end. Presenting the previous token later is treated as theft: the
// session is deleted. A rotation response lost on the wire therefore costs
// the user a login: the client still holds only the previous token, and its
// next request after Grace deletes the session. Resolve rotates when a
// handler starts and the new token reaches the client only with the
// response, so Grace must exceed the slowest authenticated handler of the
// application, and a client that aborts a rotating request before the
// response arrives costs the user a login the same way.
//
// LastUsedAt is written at most once per TouchEvery, so a busy session does
// not update its row on every request; idle expiry can fire up to TouchEvery
// early as a result.
//
// Every write Resolve makes, a delete included, is one conditional statement
// on the revision the request read (WHERE id = ? AND revision = ?), so
// concurrent requests on one session cannot overwrite or delete each other's
// work. A request whose write matches nothing reads the session again and
// re-runs the rules on what is stored now: of many requests racing past
// RotateAfter one rotates and the others find their token as the previous
// one inside Grace, and an expiry or reuse decided on a stale read cannot
// delete a session in use: a previous token presented after Grace of the
// rotation, retried after a concurrent first use of the current token, is
// inside Grace of that first use.
package session

import (
	"context"
	"crypto/rand"
	"net/http"
	"time"

	"github.com/vanclief/compose/components/auth/token"
	"github.com/vanclief/ez"
)

// Never disables the rule of IdleTTL, MaxTTL or RotateAfter. Grace does not
// accept it.
const Never = time.Duration(-1)

// TokenHeader is the response header SetRotated writes the rotated token to
// for clients that do not use cookies.
const TokenHeader = "Session-Token"

// maxAttempts bounds how often Resolve reads a session again after its
// write lost to a concurrent one.
const maxAttempts = 3

const (
	defaultIdleTTL     = 30 * 24 * time.Hour
	defaultMaxTTL      = 90 * 24 * time.Hour
	defaultRotateAfter = time.Hour
	defaultGrace       = 60 * time.Second
	defaultTouchEvery  = time.Minute
	minTouchEvery      = time.Second
)

// Config configures a Manager. A zero duration selects its default.
type Config struct {
	// Prefix is the app prefix; tokens are "<Prefix>s_<body>".
	Prefix string
	// Store persists sessions.
	Store Store
	// IdleTTL expires a session unused for this long. Default 30 days. Never
	// means no idle expiry.
	IdleTTL time.Duration
	// MaxTTL expires a session this long after creation regardless of use.
	// Default 90 days. Never means no max expiry.
	MaxTTL time.Duration
	// RotateAfter replaces the token on the first use after this age.
	// Default 1 hour. Never means never rotate.
	RotateAfter time.Duration
	// Grace keeps the previous token valid this long after the rotation
	// while the new token is unused, and this long after the new token's
	// first use once it is used, so requests already in flight from other
	// tabs still succeed. Default 60s. Must be bounded: Never is rejected.
	// The new token reaches the client only with the response of the request
	// that rotated, so Grace must exceed the slowest authenticated handler of
	// the application; a client that aborts a rotating request before the
	// response arrives costs the user a login.
	Grace time.Duration
	// TouchEvery bounds how often LastUsedAt is written: a request that
	// changes nothing else skips the write while the stored value is younger
	// than this. Default 1 minute. Never is rejected, and so is a value not
	// shorter than IdleTTL, which would expire sessions in active use, or
	// shorter than one second, which would write on every request and make
	// concurrent requests on one session fail on the optimistic lock.
	TouchEvery time.Duration
}

// Session is what a resolved token authenticates.
type Session struct {
	ID         string
	Subject    string
	CreatedAt  time.Time
	LastUsedAt time.Time
	RotatedAt  time.Time
	// FirstUsedAt is when the current token was first presented, zero until
	// then.
	FirstUsedAt time.Time
	// ExpiresAt is CreatedAt plus MaxTTL, zero when MaxTTL is Never.
	ExpiresAt time.Time
}

// Record is the stored row: Session plus TokenHash, PreviousHash and
// Revision. PreviousHash is nil until the first rotation. Revision is the
// optimistic lock: 0 on insert, incremented by every Update.
type Record struct {
	Session
	TokenHash    []byte
	PreviousHash []byte
	Revision     int64
}

// Store persists session records.
type Store interface {
	Insert(ctx context.Context, r Record) error
	// FindByHash returns the record whose current token hash or previous
	// token hash equals hash: the Manager decides whether a previous hash is
	// still accepted. ez.ENOTFOUND when there is none.
	FindByHash(ctx context.Context, hash []byte) (Record, error)
	// Update writes every mutable column of r (token hash, previous hash,
	// first used, last used, rotated and expires at) to the
	// session r.ID whose revision is still r.Revision, and sets its revision
	// to r.Revision+1, in one conditional write (UPDATE ... SET revision =
	// r.Revision+1 WHERE id = ? AND revision = ?). A request whose read is
	// stale therefore cannot overwrite a concurrent write, whether that
	// write rotated the token or only recorded its first use. Returns
	// ez.ENOTFOUND when no row matched: the session changed or is gone.
	Update(ctx context.Context, r Record) error
	// Delete removes the session id whatever its revision; a session that is
	// already gone is not an error. Revoke uses it: an explicit revocation
	// wins over any concurrent use.
	Delete(ctx context.Context, id string) error
	// DeleteRevision removes the session id only while its revision is still
	// revision, in one conditional write (DELETE ... WHERE id = ? AND
	// revision = ?). Resolve uses it to delete an expired or reused
	// session, so a request whose read is stale cannot delete a session that a
	// concurrent request has just used. A custom Store must implement it as
	// one atomic compare-and-delete, never as a read followed by Delete.
	// Returns ez.ENOTFOUND when no row matched: the session changed or is
	// gone.
	DeleteRevision(ctx context.Context, id string, revision int64) error
	DeleteBySubject(ctx context.Context, subject string) error
	// DeleteExpired removes every session whose ExpiresAt is set and not
	// after now and, when idleBefore is not zero, every session whose
	// LastUsedAt is not after idleBefore, whatever its revision. A session
	// with a zero ExpiresAt (MaxTTL Never) never matches the first rule.
	// Sweep uses it. Returns how many sessions it removed.
	DeleteExpired(ctx context.Context, now, idleBefore time.Time) (int64, error)
}

// Manager issues, resolves and revokes sessions.
type Manager struct {
	cfg    Config
	prefix string
	now    func() time.Time
}

// New returns a Manager. Prefix must be such that both Prefix and Prefix+"s"
// pass token.ValidPrefix, Store is required, zero durations take their
// defaults and negative durations other than Never are rejected, as is Grace
// set to Never and a TouchEvery under one second or not shorter than a
// bounded IdleTTL. Returns ez.EINVALID for a bad config.
func New(cfg Config) (*Manager, error) {
	if !token.ValidPrefix(cfg.Prefix) || !token.ValidPrefix(cfg.Prefix+"s") {
		return nil, ez.New(ez.EINVALID, "session: prefix must match ^[a-z0-9]{2,11}$", nil)
	}
	if cfg.Store == nil {
		return nil, ez.New(ez.EINVALID, "session: store is required", nil)
	}
	if cfg.Grace < 0 {
		return nil, ez.New(ez.EINVALID, "session: grace must not be negative", nil)
	}
	if cfg.TouchEvery < 0 {
		return nil, ez.New(ez.EINVALID, "session: touch every must not be negative", nil)
	}
	if invalidDuration(cfg.IdleTTL) || invalidDuration(cfg.MaxTTL) || invalidDuration(cfg.RotateAfter) {
		return nil, ez.New(ez.EINVALID, "session: durations must be positive or Never", nil)
	}
	if cfg.IdleTTL == 0 {
		cfg.IdleTTL = defaultIdleTTL
	}
	if cfg.MaxTTL == 0 {
		cfg.MaxTTL = defaultMaxTTL
	}
	if cfg.RotateAfter == 0 {
		cfg.RotateAfter = defaultRotateAfter
	}
	if cfg.Grace == 0 {
		cfg.Grace = defaultGrace
	}
	if cfg.TouchEvery == 0 {
		cfg.TouchEvery = defaultTouchEvery
	}
	if cfg.TouchEvery < minTouchEvery {
		return nil, ez.New(ez.EINVALID, "session: touch every must be at least one second", nil)
	}
	if cfg.IdleTTL != Never && cfg.TouchEvery >= cfg.IdleTTL {
		return nil, ez.New(ez.EINVALID, "session: touch every must be shorter than idle ttl", nil)
	}
	return &Manager{cfg: cfg, prefix: cfg.Prefix + "s", now: time.Now}, nil
}

func invalidDuration(d time.Duration) bool {
	return d < 0 && d != Never
}

// Issue creates a session and returns the token to set in the cookie or
// hand to the client.
func (m *Manager) Issue(ctx context.Context, subject string) (string, Session, error) {
	if subject == "" {
		return "", Session{}, ez.New(ez.EINVALID, "session: subject is required", nil)
	}
	tok, err := token.Generate(m.prefix)
	if err != nil {
		return "", Session{}, ez.Wrap(err)
	}
	now := m.now()
	record := Record{
		Session: Session{
			ID:         rand.Text(),
			Subject:    subject,
			CreatedAt:  now,
			LastUsedAt: now,
			RotatedAt:  now,
		},
		TokenHash: tok.Hash,
	}
	if m.cfg.MaxTTL != Never {
		record.ExpiresAt = now.Add(m.cfg.MaxTTL)
	}
	err = m.cfg.Store.Insert(ctx, record)
	if err != nil {
		return "", Session{}, ez.Wrap(err)
	}
	return tok.Value, record.Session, nil
}

// Resolve authenticates a token and returns its session. rotated is the new
// token when this call replaced it, empty otherwise; hand it to the client
// with SetRotated. Returns ez.ENOTAUTHENTICATED for a malformed, unknown,
// expired or reused token.
func (m *Manager) Resolve(ctx context.Context, value string) (Session, string, error) {
	if !token.Check(m.prefix, value) {
		return Session{}, "", ez.New(ez.ENOTAUTHENTICATED, "session: token is malformed", nil)
	}
	hash := token.Hash(value)
	for range maxAttempts {
		session, rotated, err := m.resolve(ctx, hash)
		if ez.ErrorCode(err) == ez.ENOTFOUND {
			// A concurrent request wrote the session after this one read it.
			// Read it again and decide on what is stored now.
			continue
		}
		if err != nil {
			return Session{}, "", ez.Wrap(err)
		}
		return session, rotated, nil
	}
	return Session{}, "", ez.New(ez.EINTERNAL, "session: too many concurrent updates", nil)
}

// resolve is one attempt of Resolve: it reads the session of hash, applies
// the expiry, rotation and reuse rules and writes the result. The only
// ez.ENOTFOUND it returns comes from Update or DeleteRevision and means a
// concurrent request wrote or deleted the session first; an unknown hash is
// ez.ENOTAUTHENTICATED.
func (m *Manager) resolve(ctx context.Context, hash []byte) (Session, string, error) {
	record, err := m.find(ctx, hash)
	if err != nil {
		return Session{}, "", ez.Wrap(err)
	}

	now := m.now()
	maxExpired := !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt)
	idleExpired := m.cfg.IdleTTL != Never && !now.Before(record.LastUsedAt.Add(m.cfg.IdleTTL))
	if maxExpired || idleExpired {
		// The delete is conditional on the revision read, so a stale read (a
		// concurrent request just extended LastUsedAt) retries instead of
		// deleting a session that a concurrent request just used.
		err = m.cfg.Store.DeleteRevision(ctx, record.ID, record.Revision)
		if err != nil {
			return Session{}, "", ez.Wrap(err)
		}
		return Session{}, "", ez.New(ez.ENOTAUTHENTICATED, "session: expired", nil)
	}

	rotate := false
	if token.Equal(hash, record.TokenHash) {
		firstUse := record.FirstUsedAt.IsZero()
		if firstUse {
			record.FirstUsedAt = now
		}
		rotate = m.cfg.RotateAfter != Never && now.Sub(record.RotatedAt) >= m.cfg.RotateAfter
		if rotate && record.PreviousHash != nil && now.Before(record.FirstUsedAt.Add(m.cfg.Grace)) {
			// The previous token is inside Grace of the first use: rotating
			// now would drop it while requests carrying it are in flight.
			rotate = false
		}
		if !firstUse && !rotate && now.Sub(record.LastUsedAt) < m.cfg.TouchEvery {
			// Only LastUsedAt would change, and it was written recently.
			return record.Session, "", nil
		}
		if rotate {
			record.PreviousHash = hash
		}
	} else {
		// The previous token. Grace covers requests in flight: from the
		// rotation while the current token is unused, from its first use
		// once it is used. Later is reuse.
		graceFrom := record.FirstUsedAt
		if graceFrom.IsZero() {
			graceFrom = record.RotatedAt
		}
		if now.Before(graceFrom.Add(m.cfg.Grace)) {
			return record.Session, "", nil
		}
		// The delete is conditional on the revision read, so a stale read (a
		// concurrent request just used or rotated the current token) retries
		// and decides on what is stored now instead of deleting a session
		// that a concurrent request just used.
		err = m.cfg.Store.DeleteRevision(ctx, record.ID, record.Revision)
		if err != nil {
			return Session{}, "", ez.Wrap(err)
		}
		return Session{}, "", ez.New(ez.ENOTAUTHENTICATED, "session: token was presented after its grace", nil)
	}

	record.LastUsedAt = now
	rotated := ""
	if rotate {
		tok, err := token.Generate(m.prefix)
		if err != nil {
			return Session{}, "", ez.Wrap(err)
		}
		record.TokenHash = tok.Hash
		record.FirstUsedAt = time.Time{}
		record.RotatedAt = now
		rotated = tok.Value
	}
	err = m.cfg.Store.Update(ctx, record)
	if err != nil {
		return Session{}, "", ez.Wrap(err)
	}
	return record.Session, rotated, nil
}

// Revoke deletes the session value belongs to. Returns ez.ENOTAUTHENTICATED
// for a malformed or unknown token.
func (m *Manager) Revoke(ctx context.Context, value string) error {
	if !token.Check(m.prefix, value) {
		return ez.New(ez.ENOTAUTHENTICATED, "session: token is malformed", nil)
	}
	record, err := m.find(ctx, token.Hash(value))
	if err != nil {
		return ez.Wrap(err)
	}
	err = m.cfg.Store.Delete(ctx, record.ID)
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// RevokeSubject deletes every session of subject.
func (m *Manager) RevokeSubject(ctx context.Context, subject string) error {
	err := m.cfg.Store.DeleteBySubject(ctx, subject)
	if err != nil {
		return ez.Wrap(err)
	}
	return nil
}

// Sweep deletes every session past MaxTTL or unused for IdleTTL and returns
// how many it deleted. Resolve deletes an expired session it meets, but a
// session whose token is never presented again (cookies cleared, device
// lost) stays in the Store until Sweep removes it. Nothing calls Sweep
// automatically, so run it periodically, for example from a goroutine
// started with the application:
//
//	go func() {
//		ticker := time.NewTicker(time.Hour)
//		defer ticker.Stop()
//		for {
//			select {
//			case <-ctx.Done():
//				return
//			case <-ticker.C:
//				_, err := manager.Sweep(ctx)
//				if err != nil {
//					logger.Error("sweeping sessions failed", "error", err)
//				}
//			}
//		}
//	}()
//
// LastUsedAt is written at most once per TouchEvery, so like Resolve, Sweep
// can delete a session up to TouchEvery before its idle expiry.
func (m *Manager) Sweep(ctx context.Context) (int64, error) {
	now := m.now()
	idleBefore := time.Time{}
	if m.cfg.IdleTTL != Never {
		idleBefore = now.Add(-m.cfg.IdleTTL)
	}
	count, err := m.cfg.Store.DeleteExpired(ctx, now, idleBefore)
	if err != nil {
		return 0, ez.Wrap(err)
	}
	return count, nil
}

// Cookie returns an http.Cookie for value: HttpOnly, Secure, SameSite=Lax,
// Path "/", MaxAge from MaxTTL, or from IdleTTL when MaxTTL is Never. When
// both are Never it is a session cookie (MaxAge 0, no Expires).
//
// MaxAge follows MaxTTL rather than IdleTTL because the cookie is only sent
// again when the token rotates: with RotateAfter Never an IdleTTL cookie would
// log an active user out IdleTTL after login. Resolve still enforces IdleTTL,
// so a cookie that outlives it is harmless. The remaining limit: with MaxTTL
// and RotateAfter both Never the cookie expires IdleTTL after login unless the
// application sends it again.
func (m *Manager) Cookie(name, value string) *http.Cookie {
	maxAge := 0
	if m.cfg.MaxTTL != Never {
		maxAge = int(m.cfg.MaxTTL / time.Second)
	} else if m.cfg.IdleTTL != Never {
		maxAge = int(m.cfg.IdleTTL / time.Second)
	}
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// ClearCookie returns the cookie that deletes the one Cookie sets: same
// attributes, empty value, MaxAge -1.
func (m *Manager) ClearCookie(name string) *http.Cookie {
	cookie := m.Cookie(name, "")
	cookie.MaxAge = -1
	return cookie
}

// SetRotated hands a rotated token back to the client: as a Set-Cookie of
// cookieName when the request authenticated with a cookie, else in the
// Session-Token response header for clients that keep the token
// themselves, such as a mobile keychain. It does nothing when rotated is "".
func (m *Manager) SetRotated(w http.ResponseWriter, cookieName, rotated string, fromCookie bool) {
	if rotated == "" {
		return
	}
	if fromCookie {
		http.SetCookie(w, m.Cookie(cookieName, rotated))
		return
	}
	w.Header().Set(TokenHeader, rotated)
}

// find looks a record up by hash, mapping ENOTFOUND to ENOTAUTHENTICATED.
func (m *Manager) find(ctx context.Context, hash []byte) (Record, error) {
	record, err := m.cfg.Store.FindByHash(ctx, hash)
	if ez.ErrorCode(err) == ez.ENOTFOUND {
		return Record{}, ez.New(ez.ENOTAUTHENTICATED, "session: unknown token", err)
	}
	if err != nil {
		return Record{}, ez.Wrap(err)
	}
	return record, nil
}
