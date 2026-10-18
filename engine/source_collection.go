package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
)

type sourceReadSetKey struct{}

type sourceRead struct {
	revision run.Revision
	check    func(context.Context) error
}

type sourceReadSet struct{ reads []sourceRead }

// readPreparedSource records the exact value consumed by preparation. Ordinary
// prompt assembly has no read set and keeps its existing single-read behavior.
func readPreparedSource[T any](ctx context.Context, name string, read func(context.Context) (*T, error)) (*T, error) {
	value, err := read(ctx)
	if err != nil {
		return nil, err
	}
	return recordPreparedSource(ctx, name, value, read)
}

func recordPreparedSource[T any](ctx context.Context, name string, value *T, read func(context.Context) (*T, error)) (*T, error) {
	set, ok := ctx.Value(sourceReadSetKey{}).(*sourceReadSet)
	if !ok {
		return value, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("cortex: source cannot be frozen: %w", cortex.ErrInvalidPrepared)
	}
	var detached *T
	if err := decodePreparedJSON(data, &detached); err != nil {
		return nil, fmt.Errorf("cortex: source cannot be decoded: %w", cortex.ErrInvalidPrepared)
	}
	digest := contentDigest(data)
	set.reads = append(set.reads, sourceRead{
		revision: run.Revision{Name: name, Digest: digest},
		check: func(checkCtx context.Context) error {
			current, readErr := read(checkCtx)
			if readErr != nil {
				return cortex.ErrPreparationConflict
			}
			currentData, encodeErr := json.Marshal(current)
			if encodeErr != nil || contentDigest(currentData) != digest {
				return cortex.ErrPreparationConflict
			}
			return nil
		},
	})
	return detached, nil
}

// validate detects observed source changes, including additions at previously
// absent overlay scopes. Independent reads do not establish a transactional
// snapshot or detect every ABA change; callers must not claim that guarantee.
func (s *sourceReadSet) validate(ctx context.Context) ([]run.Revision, error) {
	revisions := make([]run.Revision, 0, len(s.reads))
	for _, source := range s.reads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := source.check(ctx); err != nil {
			return nil, err
		}
		revisions = append(revisions, source.revision)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return revisions, nil
}

func cloneTemperature(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
