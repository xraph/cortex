package state

import (
	"context"
	"strconv"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// ListExperiments pages exact-scope records by creation time and ID.
func (s *Store) ListExperiments(ctx context.Context, query optimization.ListQuery) (optimization.Page[optimization.Experiment], error) {
	switch query.State {
	case "", optimization.StateDraft, optimization.StateQueued, optimization.StateRunning, optimization.StatePaused, optimization.StateReviewRequired, optimization.StateCompleted, optimization.StateCancelled, optimization.StateFailed, optimization.StateBudgetExhausted:
	default:
		return optimization.Page[optimization.Experiment]{}, optimization.ErrInvalidSpec
	}
	if !query.AgentID.IsNil() {
		if _, err := id.ParseWithPrefix(query.AgentID.String(), id.PrefixAgent); err != nil {
			return optimization.Page[optimization.Experiment]{}, optimization.ErrInvalidSpec
		}
	}
	page := optimization.PageQuery{Limit: query.Limit, Cursor: query.Cursor}
	query.Limit, query.Cursor = 0, ""
	return readPage(ctx, s, "experiments", id.Nil, query, page, func(data *scopeData) ([]pageRow[optimization.Experiment], error) {
		return selectRows(data.Experiments, func(value optimization.Experiment) bool {
			return (query.AgentID.IsNil() || value.Spec.AgentID == query.AgentID) && (query.State == "" || value.State == query.State)
		}, func(value optimization.Experiment) time.Time { return value.CreatedAt }), nil
	})
}

// ListEvents returns monotonic audit entries after the supplied sequence.
func (s *Store) ListEvents(ctx context.Context, experimentID id.ID, after int64, limit int) (optimization.Page[optimization.Event], error) {
	page := optimization.Page[optimization.Event]{Items: []optimization.Event{}}
	key, err := contextScope(ctx)
	if err != nil {
		return page, err
	}
	if after < 0 {
		return page, optimization.ErrInvalidSpec
	}
	limit, err = pageLimit(limit)
	if err != nil {
		return page, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return page, optimization.ErrUnavailable
	}
	data, loadErr := s.loadScope(ctx, key)
	if loadErr != nil {
		return page, loadErr
	}
	if data == nil {
		return page, optimization.ErrNotFound
	}
	if _, exists := data.Experiments[experimentID.String()]; !exists {
		return page, optimization.ErrNotFound
	}
	for _, event := range data.Events[experimentID.String()] {
		if event.Sequence <= after {
			continue
		}
		if len(page.Items) == limit {
			page.NextCursor = strconv.FormatInt(page.Items[len(page.Items)-1].Sequence, 10)
			break
		}
		page.Items = append(page.Items, event)
	}
	return clone(page)
}

func pageLimit(limit int) (int, error) {
	if limit == 0 {
		return 100, nil
	}
	if limit < 1 || limit > 500 {
		return 0, optimization.ErrInvalidSpec
	}
	return limit, nil
}
