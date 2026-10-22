package state

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/optimization"
)

// RecordKey identifies one record without conflating parent and item keys.
type RecordKey struct {
	Kind, Parent, Key string
}

// ID returns the bounded primary lookup key used by durable backends.
func (k RecordKey) ID() string { return digestParts(k.Kind, k.Parent, k.Key) }

func (s *Store) loadResource(ctx context.Context, scope string, key RecordKey) (*scopeData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.backend == nil {
		return s.scopes[scope], nil
	}
	records, err := s.backend.LoadRecord(ctx, scope, key)
	if err != nil {
		return nil, err
	}
	if len(records) > 1 {
		return nil, fmt.Errorf("%w: multiple records for one optimization identity", optimization.ErrUnavailable)
	}
	for _, record := range records {
		if record.ID != key.ID() || record.Kind != key.Kind || record.Parent != key.Parent || record.Key != key.Key {
			return nil, fmt.Errorf("%w: optimization lookup identity mismatch", optimization.ErrUnavailable)
		}
	}
	return decodeRecords(scope, records)
}
