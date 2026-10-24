package session

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/vanclief/ez"
)

// memoryStore keeps records in a map keyed by session id, guarded by a mutex.
type memoryStore struct {
	mu      sync.Mutex
	records map[string]Record
}

// NewMemoryStore returns a Store kept in process memory, for tests and
// single-process deployments. Expired sessions stay until Resolve meets them
// or Sweep deletes them.
func NewMemoryStore() Store {
	return &memoryStore{records: map[string]Record{}}
}

// clone copies the hash slices so callers cannot mutate stored records.
func clone(r Record) Record {
	r.TokenHash = bytes.Clone(r.TokenHash)
	r.PreviousHash = bytes.Clone(r.PreviousHash)
	return r
}

func (m *memoryStore) Insert(ctx context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.records[r.ID]
	if exists {
		return ez.New(ez.ECONFLICT, "session: id already exists", nil)
	}
	m.records[r.ID] = clone(r)
	return nil
}

func (m *memoryStore) FindByHash(ctx context.Context, hash []byte) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.records {
		if bytes.Equal(r.TokenHash, hash) || (r.PreviousHash != nil && bytes.Equal(r.PreviousHash, hash)) {
			return clone(r), nil
		}
	}
	return Record{}, ez.New(ez.ENOTFOUND, "session: no session for token", nil)
}

func (m *memoryStore) Update(ctx context.Context, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.records[r.ID]
	if !ok || stored.Revision != r.Revision {
		return ez.New(ez.ENOTFOUND, "session: no session to update", nil)
	}
	r.Subject = stored.Subject
	r.CreatedAt = stored.CreatedAt
	r.Revision++
	m.records[r.ID] = clone(r)
	return nil
}

func (m *memoryStore) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, id)
	return nil
}

func (m *memoryStore) DeleteRevision(ctx context.Context, id string, revision int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.records[id]
	if !ok || stored.Revision != revision {
		return ez.New(ez.ENOTFOUND, "session: no session to delete", nil)
	}
	delete(m.records, id)
	return nil
}

func (m *memoryStore) DeleteBySubject(ctx context.Context, subject string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, r := range m.records {
		if r.Subject == subject {
			delete(m.records, id)
		}
	}
	return nil
}

func (m *memoryStore) DeleteExpired(ctx context.Context, now, idleBefore time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := int64(0)
	for id, r := range m.records {
		maxExpired := !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(now)
		idleExpired := !idleBefore.IsZero() && !r.LastUsedAt.After(idleBefore)
		if maxExpired || idleExpired {
			delete(m.records, id)
			count++
		}
	}
	return count, nil
}
