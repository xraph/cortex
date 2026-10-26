package coordination

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/suspension"
)

// ScopedAgentReplySource discovers only run identities in an explicit inventory
// of exact scopes. It checks current host discovery authority around each page.
type ScopedAgentReplySource struct {
	store     suspension.ReplyDiscoveryStore
	scopes    []cortex.Scope
	authorize func(context.Context, cortex.Scope) error
}

// NewScopedAgentReplySource detaches at most 256 distinct scopes. Native stores
// return bounded identity projections, including admitted and expired pauses.
func NewScopedAgentReplySource(store suspension.ReplyDiscoveryStore, scopes []cortex.Scope, authorize func(context.Context, cortex.Scope) error) (*ScopedAgentReplySource, error) {
	if hostport.IsNil(store) || authorize == nil {
		return nil, ErrInvalidScheduler
	}
	detached, err := freezeDiscoveryScopes(scopes)
	if err != nil {
		return nil, err
	}
	return &ScopedAgentReplySource{store: store, scopes: detached, authorize: authorize}, nil
}

// ListAgentReplyRuns reads one bounded exact-scope page. Empty scopes advance
// normally. The service starts another pass after an empty Next cursor.
func (s *ScopedAgentReplySource) ListAgentReplyRuns(ctx context.Context, req AgentReplyListRequest) (AgentReplyPage, error) {
	if req.Limit < 1 || req.Limit > 256 || len(req.After) > 4096 {
		return AgentReplyPage{}, ErrInvalidScheduler
	}
	if err := ctx.Err(); err != nil {
		return AgentReplyPage{}, err
	}
	index := 0
	var after id.AgentRunID
	if req.After != "" {
		prefix, key, ok := strings.Cut(req.After, ":")
		parsed, err := strconv.Atoi(prefix)
		if !ok || err != nil || parsed < 0 || parsed >= len(s.scopes) {
			return AgentReplyPage{}, ErrInvalidScheduler
		}
		index = parsed
		if key != "" {
			after, err = id.ParseAgentRunID(key)
			if err != nil {
				return AgentReplyPage{}, ErrInvalidScheduler
			}
		}
	}
	scope := s.scopes[index]
	ctx = cortex.WithScope(ctx, cortex.Scope{Levels: slices.Clone(scope.Levels)})
	if err := s.authorize(ctx, cortex.Scope{Levels: slices.Clone(scope.Levels)}); err != nil {
		return AgentReplyPage{}, err
	}
	keys, err := s.store.ListAgentReplyRuns(ctx, after, req.Limit)
	if err != nil {
		return AgentReplyPage{}, err
	}
	if len(keys) > req.Limit {
		return AgentReplyPage{}, ErrInvalidScheduler
	}
	page := AgentReplyPage{Runs: make([]ScheduledAgentReply, 0, len(keys))}
	previous := after.String()
	for _, key := range keys {
		if key.IsNil() || key.Prefix() != id.PrefixAgentRun || key.String() <= previous {
			return AgentReplyPage{}, ErrInvalidScheduler
		}
		previous = key.String()
		page.Runs = append(page.Runs, ScheduledAgentReply{Scope: cortex.Scope{Levels: slices.Clone(scope.Levels)}, ID: key})
	}
	if authErr := s.authorize(ctx, cortex.Scope{Levels: slices.Clone(scope.Levels)}); authErr != nil {
		return AgentReplyPage{}, authErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return AgentReplyPage{}, contextErr
	}
	if len(keys) == req.Limit {
		page.Next = fmt.Sprintf("%d:%s", index, previous)
	} else if index+1 < len(s.scopes) {
		page.Next = fmt.Sprintf("%d:", index+1)
	}
	return page, nil
}
