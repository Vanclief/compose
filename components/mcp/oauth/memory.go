package oauth

import (
	"context"
	"sync"
	"time"

	"github.com/vanclief/ez"
)

// memorySweepInterval is the least time between two sweeps of expired
// records.
const memorySweepInterval = time.Minute

// memoryStore keeps records in a map guarded by a mutex. Expired records are
// dropped when they are read and swept on a write at most once per
// memorySweepInterval.
type memoryStore struct {
	mu        sync.Mutex
	records   map[string]memoryRecord
	lastSweep time.Time
}

type memoryRecord struct {
	value     []byte
	expiresAt time.Time
}

// NewMemoryStore returns a Store kept in process memory, for tests and
// single-process deployments.
func NewMemoryStore() Store {
	return &memoryStore{records: map[string]memoryRecord{}}
}

func memoryKey(kind, id string) string {
	return kind + "\x00" + id
}

func (m *memoryStore) Put(ctx context.Context, kind, id string, value []byte, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if now.Sub(m.lastSweep) >= memorySweepInterval {
		m.lastSweep = now
		for key, record := range m.records {
			if !record.expiresAt.After(now) {
				delete(m.records, key)
			}
		}
	}
	m.records[memoryKey(kind, id)] = memoryRecord{value: append([]byte(nil), value...), expiresAt: expiresAt}
	return nil
}

func (m *memoryStore) Get(ctx context.Context, kind, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookup(kind, id, false)
}

func (m *memoryStore) Take(ctx context.Context, kind, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lookup(kind, id, true)
}

// lookup returns the live record under kind and id, deleting it when remove
// is set or when it has expired. The caller holds the mutex.
func (m *memoryStore) lookup(kind, id string, remove bool) ([]byte, error) {
	key := memoryKey(kind, id)
	record, ok := m.records[key]
	if !ok {
		return nil, ez.New(ez.ENOTFOUND, "oauth: no "+kind+" record", nil)
	}
	if !record.expiresAt.After(time.Now()) {
		delete(m.records, key)
		return nil, ez.New(ez.ENOTFOUND, "oauth: "+kind+" record expired", nil)
	}
	if remove {
		delete(m.records, key)
	}
	return append([]byte(nil), record.value...), nil
}

func (m *memoryStore) Delete(ctx context.Context, kind, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.records, memoryKey(kind, id))
	return nil
}
