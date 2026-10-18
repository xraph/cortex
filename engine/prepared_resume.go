package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/suspension"
)

func decodeContinuation(cont suspension.Continuation) (*PreparedAgent, *preparedSnapshot, error) {
	if cont.SchemaVersion == 0 {
		if cont.Config.Publication != (safety.Policy{}) || cont.Config.AllToolsRestricted || cont.Config.Budget.MaxSteps != 0 || len(cont.Prepared) > 0 || cont.Provenance.PreparedDigest != "" || cont.Config.Budget.MaxTokens != 0 || cont.Config.Budget.MaxToolCalls != 0 || !cont.Config.Budget.Deadline.IsZero() {
			return nil, nil, fmt.Errorf("cortex: legacy continuation contains unsupported policy: %w", cortex.ErrInvalidContinuation)
		}
		return nil, nil, nil
	}
	if cont.SchemaVersion != preparedSchemaVersion {
		return nil, nil, fmt.Errorf("cortex: continuation schema %d: %w", cont.SchemaVersion, cortex.ErrInvalidContinuation)
	}
	var prepared PreparedAgent
	if err := json.Unmarshal(cont.Prepared, &prepared); err != nil {
		return nil, nil, fmt.Errorf("cortex: continuation snapshot: %w: %w", cortex.ErrInvalidContinuation, err)
	}
	if prepared.Digest() != cont.Provenance.PreparedDigest {
		return nil, nil, fmt.Errorf("cortex: continuation identity mismatch: %w", cortex.ErrInvalidContinuation)
	}
	snap, err := prepared.snapshot()
	return &prepared, snap, err
}

func validateFrozenContinuation(cont suspension.Continuation, snap *preparedSnapshot, r *run.Run) error {
	if snap == nil {
		if r.Provenance.PreparedDigest != "" {
			return fmt.Errorf("cortex: prepared run cannot resume legacy continuation: %w", cortex.ErrInvalidContinuation)
		}
		return nil
	}
	if !cont.Config.Budget.Equal(r.Budget) {
		return fmt.Errorf("cortex: continuation changed durable budget: %w", cortex.ErrInvalidContinuation)
	}
	cfg := configFromContinuation(cont.Config)
	frozen := snap.Config
	// A caller deadline may narrow a frozen deadline when the run starts.
	if !frozen.Budget.Deadline.IsZero() && (cfg.Budget.Deadline.IsZero() || cfg.Budget.Deadline.After(frozen.Budget.Deadline)) {
		return cortex.ErrInvalidContinuation
	}
	cfg.Budget.Deadline = frozen.Budget.Deadline
	if !reflect.DeepEqual(cfg, frozen) || cont.SystemPrompt != prompt.Assemble(snap.Sections) || r.AgentID != snap.Agent.ID || r.Scope.Canonical() != snap.Scope.Canonical() {
		return fmt.Errorf("cortex: continuation changed frozen inputs: %w", cortex.ErrInvalidContinuation)
	}
	if !reflect.DeepEqual(r.Provenance, cont.Provenance) {
		return cortex.ErrInvalidContinuation
	}
	if cont.Usage != r.Usage || cont.StepIndex != r.StepCount || cont.TokensUsed != r.TokensUsed {
		return fmt.Errorf("cortex: continuation differs from durable accounting: %w", cortex.ErrInvalidContinuation)
	}
	if cont.Usage.Steps != cont.StepIndex || cont.Usage.Tokens != cont.TokensUsed || cont.Usage.Steps < 0 || cont.Usage.Tokens < 0 || cont.Usage.ToolCalls < 0 || cont.Usage.ReportedTokens < 0 || cont.Usage.ReportedTokens > cont.Usage.Tokens {
		return fmt.Errorf("cortex: continuation accounting mismatch: %w", cortex.ErrInvalidContinuation)
	}
	return nil
}

func (e *Engine) authorizeResumedCalls(ctx context.Context, r *run.Run, susp *suspension.Suspension, results []ToolResult, st *reactState) error {
	subject := cortex.Subject{Scope: cortex.ScopeFromContext(ctx), Principal: cortex.PrincipalFromContext(ctx), AgentID: r.AgentID, RunID: r.ID}
	byID := make(map[string]ToolResult, len(results))
	for _, result := range results {
		byID[result.ToolCallID] = result
	}
	for _, pending := range susp.Pending {
		if !byID[pending.ID].Execute {
			continue
		}
		if !susp.Cont.Config.Budget.Deadline.IsZero() && !time.Now().Before(susp.Cont.Config.Budget.Deadline) {
			return context.DeadlineExceeded
		}
		if st.preparedSnapshot != nil && len(filterByName(st.preparedSnapshot.Tools, []string{pending.Name})) == 0 {
			return fmt.Errorf("cortex: resumed tool is outside prepared allowlist: %w", cortex.ErrInvalidContinuation)
		}
		if err := e.authorizeResumedTool(ctx, subject, llm.ToolCall{ID: pending.ID, Name: pending.Name, Arguments: pending.Arguments}, susp.Reason == suspension.ReasonApproval); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) authorizeResumedTool(ctx context.Context, subject cortex.Subject, call llm.ToolCall, approved bool) error {
	if e.authorizer == nil {
		return nil
	}
	err := e.authorizer.Authorize(ctx, subject, call)
	// Only a claimed approval checkpoint can satisfy a fresh escalation.
	if err != nil && (!approved || !errors.Is(err, cortex.ErrRequiresApproval)) {
		return fmt.Errorf("cortex: resumed tool authorization: %w", err)
	}
	return nil
}
