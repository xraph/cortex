package state

import (
	"context"
	"time"

	"github.com/xraph/cortex/optimization/store"
)

// Backend provides coherent exact-scope reads and serialized database updates.
// Update must acquire its database fence before reading, invoke the callback
// once, and atomically persist its mutations or none. It must never retry a
// callback or use a process-local lock as its only concurrency control.
type Backend interface {
	Load(context.Context, string) ([]Record, error)
	Update(context.Context, string, func([]Record) ([]Mutation, error)) error
	Migrate(context.Context) error
	Ping(context.Context) error
	Close() error
}

// NewBackend binds the shared invariants to a database adapter.
func NewBackend(backend Backend) *Store {
	if backend == nil {
		panic("optimization: missing internal storage backend")
	}
	return &Store{backend: backend, now: time.Now}
}

// lockOperation keeps migration and close exclusive. Database operations use
// detached snapshots and database fences, so they can share this lifecycle lock.
// Memory operations still exclusively protect their process-local scope map.
func (s *Store) lockOperation() func() {
	if s.backend != nil {
		s.mu.RLock()
		return s.mu.RUnlock
	}
	s.mu.Lock()
	return s.mu.Unlock
}

func (s *Store) loadScope(ctx context.Context, scope string) (*scopeData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.backend == nil {
		return s.scopes[scope], nil
	}
	records, err := s.backend.Load(ctx, scope)
	if err != nil {
		return nil, err
	}
	return decodeRecords(scope, records)
}

func (s *Store) updateBackend(ctx context.Context, scope string, fn func(store.Tx) error) error {
	return s.backend.Update(ctx, scope, func(records []Record) ([]Mutation, error) {
		original, err := decodeRecords(scope, records)
		if err != nil {
			return nil, err
		}
		data, err := s.applyTransaction(ctx, scope, original, fn)
		if err != nil {
			return nil, err
		}
		after, err := encodeRecords(scope, data)
		if err != nil {
			return nil, err
		}
		return recordChanges(records, after)
	})
}

func (s *Store) applyTransaction(ctx context.Context, scope string, original *scopeData, fn func(store.Tx) error) (*scopeData, error) {
	data := newScopeData()
	if original != nil {
		copied, err := clone(*original)
		if err != nil {
			return nil, err
		}
		data = &copied
	}
	tx := &transaction{ctx: ctx, scope: scope, data: data, original: original, active: true, now: s.now().UTC()}
	defer func() { tx.active = false }()
	if err := fn(tx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
