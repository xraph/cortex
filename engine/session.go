package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/session"
)

// resolveSession picks the session a run belongs to. An explicit override
// wins. Otherwise the agent's default session for this scope is used, and
// created if it does not exist yet.
//
// The default is a real row rather than an empty-string session id,
// because an empty identifier that silently means "the shared one" is the
// shape that leaked conversation history across tenants before scope
// existed. A lazy insert costs one write on an agent's first unsessioned
// run and leaves no sentinel to misread.
func (e *Engine) resolveSession(ctx context.Context, agentID id.AgentID, override id.SessionID) (id.SessionID, error) {
	if !override.IsNil() {
		return override, nil
	}

	if e.store == nil {
		return id.SessionID{}, fmt.Errorf("resolve default session: %w", cortex.ErrNoStore)
	}

	existing, err := e.store.ListSessions(ctx, &session.ListFilter{AgentID: agentID, Limit: 1, DefaultOnly: true})
	if err != nil {
		return id.SessionID{}, fmt.Errorf("resolve default session: %w", err)
	}
	if len(existing) == 1 {
		return existing[0].ID, nil
	}

	s := &session.Session{ID: id.NewSessionID(), AgentID: agentID, IsDefault: true, Title: "Default"}
	if err := e.store.CreateSession(ctx, s); err != nil {
		// A concurrent run may have won the race. The partial unique
		// index on (agent_id, scope_canon) WHERE is_default makes that a
		// unique violation rather than a second default, so re-read
		// instead of failing the run.
		if errors.Is(err, cortex.ErrAlreadyExists) {
			again, reErr := e.store.ListSessions(ctx, &session.ListFilter{AgentID: agentID, Limit: 1, DefaultOnly: true})
			if reErr == nil && len(again) == 1 {
				return again[0].ID, nil
			}
		}
		return id.SessionID{}, fmt.Errorf("create default session: %w", err)
	}
	return s.ID, nil
}
