// Package orchestrationstate shares atomic child retention across store backends.
package orchestrationstate

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

// Snapshot pairs a detached execution record with its storage revision.
type Snapshot struct {
	Run      *orchestration.Run
	Revision int64
}

// Driver reads at the caller's exact scope and conditionally replaces a row.
// A false result means no write occurred. Errors may include uncertain writes.
type Driver interface {
	Read(context.Context, id.OrchestrationID) (Snapshot, error)
	CompareAndSwap(context.Context, Snapshot, *orchestration.Run) (bool, error)
}

// Update retains every child observed by any writer and never reopens a team.
func Update(ctx context.Context, driver Driver, record *orchestration.Run) error {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	if record == nil {
		return cortex.ErrInvalidState
	}
	incoming := clone(record)
	updated, err := mutate(ctx, driver, incoming.ID, func(current *orchestration.Run) (*orchestration.Run, error) {
		if incoming.ContinuationGeneration != current.ContinuationGeneration {
			return nil, cortex.ErrOrchestrationRunConflict
		}
		if current.Status == orchestration.StatusWaiting && incoming.Status != orchestration.StatusWaiting && incoming.Status != orchestration.StatusFailed {
			return nil, cortex.ErrOrchestrationRunConflict
		}
		if (current.Status == orchestration.StatusCompleted || current.Status == orchestration.StatusFailed) && incoming.Status != current.Status {
			return nil, cortex.ErrOrchestrationRunClosed
		}
		candidate := clone(incoming)
		candidate.Scope = current.Scope
		children, mergeErr := mergeChildren(candidate.AgentRunIDs, current.AgentRunIDs)
		if mergeErr != nil {
			return nil, mergeErr
		}
		candidate.AgentRunIDs = children
		return candidate, nil
	})
	if err == nil {
		*record = *updated
	}
	return err
}

// Link admits an identity once, while the parent is still running.
func Link(ctx context.Context, driver Driver, team id.OrchestrationID, child id.AgentRunID) error {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	if child.IsNil() || child.Prefix() != id.PrefixAgentRun {
		return cortex.ErrInvalidState
	}
	_, err := mutate(ctx, driver, team, func(current *orchestration.Run) (*orchestration.Run, error) {
		if current.Status != orchestration.StatusRunning {
			return nil, cortex.ErrOrchestrationRunClosed
		}
		if slices.Contains(current.AgentRunIDs, child) {
			return nil, cortex.ErrAlreadyExists
		}
		current.AgentRunIDs = append(current.AgentRunIDs, child)
		return current, nil
	})
	return err
}

func mutate(ctx context.Context, driver Driver, key id.OrchestrationID, change func(*orchestration.Run) (*orchestration.Run, error)) (*orchestration.Run, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if key.IsNil() || key.Prefix() != id.PrefixOrchestration {
		return nil, cortex.ErrInvalidState
	}
	for range 64 {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		before, readErr := driver.Read(ctx, key)
		if readErr != nil {
			return nil, readErr
		}
		if before.Run == nil || before.Run.ID != key || before.Run.Scope.Canonical() != scope.Canonical() {
			return nil, cortex.ErrOrchestrationRunNotFound
		}
		if before.Revision < 0 || before.Revision == math.MaxInt64 {
			return nil, cortex.ErrOrchestrationRunConflict
		}
		candidate, changeErr := change(clone(before.Run))
		if changeErr != nil {
			return nil, changeErr
		}
		candidate.UpdatedAt = time.Now().UTC()
		written, writeErr := driver.CompareAndSwap(ctx, before, candidate)
		if writeErr != nil {
			return nil, writeErr
		}
		if written {
			return candidate, nil
		}
	}
	return nil, cortex.ErrOrchestrationRunConflict
}

func clone(record *orchestration.Run) *orchestration.Run {
	out := *record
	out.Scope.Levels = slices.Clone(record.Scope.Levels)
	out.AgentRunIDs = slices.Clone(record.AgentRunIDs)
	if record.CompletedAt != nil {
		completed := *record.CompletedAt
		out.CompletedAt = &completed
	}
	return &out
}

func mergeChildren(first, retained []id.AgentRunID) ([]id.AgentRunID, error) {
	result := make([]id.AgentRunID, 0, len(first)+len(retained))
	seen := make(map[id.AgentRunID]bool, len(first)+len(retained))
	for _, group := range [][]id.AgentRunID{first, retained} {
		for _, child := range group {
			if child.IsNil() || child.Prefix() != id.PrefixAgentRun {
				return nil, cortex.ErrInvalidState
			}
			if !seen[child] {
				result = append(result, child)
				seen[child] = true
			}
		}
	}
	return result, nil
}

// ClaimContinuation elects one strategy pass from a durable waiting state.
func ClaimContinuation(ctx context.Context, driver Driver, team id.OrchestrationID) (*orchestration.Run, error) {
	return mutate(ctx, driver, team, func(current *orchestration.Run) (*orchestration.Run, error) {
		if current.Status != orchestration.StatusWaiting || current.ContinuationGeneration < 1 || current.ContinuationGeneration == math.MaxInt64 {
			return nil, cortex.ErrOrchestrationRunConflict
		}
		current.Status = orchestration.StatusRunning
		current.ContinuationGeneration++
		current.Error = ""
		current.CompletedAt = nil
		return current, nil
	})
}
