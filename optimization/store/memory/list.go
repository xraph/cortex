package memory

import (
	"context"
	"encoding/base64"
	"slices"
	"strconv"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

type listCursor struct {
	Scope string             `json:"scope"`
	Agent string             `json:"agent"`
	State optimization.State `json:"state"`
	After string             `json:"after"`
}

// ListExperiments pages exact-scope identities in stable ID order.
func (s *Store) ListExperiments(ctx context.Context, query optimization.ListQuery) (optimization.Page[optimization.Experiment], error) {
	page := optimization.Page[optimization.Experiment]{Items: []optimization.Experiment{}}
	key, err := contextScope(ctx)
	if err != nil {
		return page, err
	}
	limit, err := pageLimit(query.Limit)
	if err != nil {
		return page, err
	}
	cursor := listCursor{Scope: key, Agent: query.AgentID.String(), State: query.State}
	if query.Cursor != "" {
		if len(query.Cursor) > 4096 {
			return page, optimization.ErrInvalidSpec
		}
		data, decodeErr := base64.RawURLEncoding.DecodeString(query.Cursor)
		if decodeErr != nil {
			return page, optimization.ErrInvalidSpec
		}
		var previous listCursor
		if decodeErr = canonical.Decode(data, &previous); decodeErr != nil {
			return page, optimization.ErrInvalidSpec
		}
		if previous.Scope != key || previous.Agent != cursor.Agent || previous.State != query.State {
			return page, optimization.ErrInvalidSpec
		}
		cursor.After = previous.After
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return page, optimization.ErrUnavailable
	}
	data := s.scopes[key]
	if data == nil {
		return page, nil
	}
	keys := make([]string, 0, len(data.Experiments))
	for entityID, experiment := range data.Experiments {
		if entityID <= cursor.After || (!query.AgentID.IsNil() && experiment.Spec.AgentID != query.AgentID) || (query.State != "" && experiment.State != query.State) {
			continue
		}
		keys = append(keys, entityID)
	}
	slices.Sort(keys)
	more := len(keys) > limit
	if more {
		keys = keys[:limit]
	}
	for _, entityID := range keys {
		page.Items = append(page.Items, data.Experiments[entityID])
	}
	if more {
		cursor.After = keys[len(keys)-1]
		encoded, encodeErr := canonical.Marshal(cursor)
		if encodeErr != nil {
			return page, encodeErr
		}
		page.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return clone(page)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return page, optimization.ErrUnavailable
	}
	data := s.scopes[key]
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
