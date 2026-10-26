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
	"github.com/xraph/cortex/orchestration"
)

// ScopedOrchestrationSource walks an explicit host-owned inventory of exact
// scopes. Authorization is repeated before and after every bounded storage page.
// It includes closed budgets so interrupted parent cleanup is retried.
type ScopedOrchestrationSource struct {
	store     orchestration.BudgetDiscoveryStore
	scopes    []cortex.Scope
	authorize func(context.Context, cortex.Scope) error
}

// NewScopedOrchestrationSource detaches at most 256 distinct exact scopes.
// Authorize checks the current host service identity, not an agent's authority.
func NewScopedOrchestrationSource(store orchestration.BudgetDiscoveryStore, scopes []cortex.Scope, authorize func(context.Context, cortex.Scope) error) (*ScopedOrchestrationSource, error) {
	if hostport.IsNil(store) || authorize == nil {
		return nil, ErrInvalidScheduler
	}
	detached, err := freezeDiscoveryScopes(scopes)
	if err != nil {
		return nil, err
	}
	return &ScopedOrchestrationSource{store: store, scopes: detached, authorize: authorize}, nil
}

// ListOrchestrations reads one scope page and returns a cursor for the next page
// or scope. Empty scopes advance normally. A completed pass restarts on polling.
func (s *ScopedOrchestrationSource) ListOrchestrations(ctx context.Context, request OrchestrationListRequest) (OrchestrationPage, error) {
	if request.Limit < 1 || request.Limit > 256 || len(request.After) > 4096 {
		return OrchestrationPage{}, ErrInvalidScheduler
	}
	if err := ctx.Err(); err != nil {
		return OrchestrationPage{}, err
	}
	index := 0
	var after id.OrchestrationID
	if request.After != "" {
		prefix, key, ok := strings.Cut(request.After, ":")
		parsed, err := strconv.Atoi(prefix)
		if !ok || err != nil || parsed < 0 || parsed >= len(s.scopes) {
			return OrchestrationPage{}, ErrInvalidScheduler
		}
		index = parsed
		if key != "" {
			after, err = id.Parse(key)
			if err != nil || after.Prefix() != id.PrefixOrchestration {
				return OrchestrationPage{}, ErrInvalidScheduler
			}
		}
	}
	scope := s.scopes[index]
	ctx = cortex.WithScope(ctx, cortex.Scope{Levels: slices.Clone(scope.Levels)})
	if err := s.authorize(ctx, cortex.Scope{Levels: slices.Clone(scope.Levels)}); err != nil {
		return OrchestrationPage{}, err
	}
	budgets, err := s.store.ListOrchestrationBudgets(ctx, orchestration.BudgetListRequest{After: after, Limit: request.Limit})
	if err != nil {
		return OrchestrationPage{}, err
	}
	if len(budgets) > request.Limit {
		return OrchestrationPage{}, ErrInvalidScheduler
	}
	page := OrchestrationPage{Teams: make([]ScheduledOrchestration, 0, len(budgets))}
	previous := after.String()
	for _, policy := range budgets {
		if _, _, freezeErr := policy.Freeze(); freezeErr != nil || policy.Scope.Canonical() != scope.Canonical() || policy.ExecutionID.String() <= previous {
			return OrchestrationPage{}, ErrInvalidScheduler
		}
		previous = policy.ExecutionID.String()
		page.Teams = append(page.Teams, ScheduledOrchestration{Scope: cortex.Scope{Levels: slices.Clone(scope.Levels)}, ID: policy.ExecutionID})
	}
	if authErr := s.authorize(ctx, cortex.Scope{Levels: slices.Clone(scope.Levels)}); authErr != nil {
		return OrchestrationPage{}, authErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return OrchestrationPage{}, contextErr
	}
	if len(budgets) == request.Limit {
		page.Next = fmt.Sprintf("%d:%s", index, previous)
	} else if index+1 < len(s.scopes) {
		page.Next = fmt.Sprintf("%d:", index+1)
	}
	return page, nil
}
