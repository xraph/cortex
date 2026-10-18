package engine

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
)

// approvedText cannot be constructed by a provider's streaming callback.
type approvedText string

const publicationToolOverhead = 64

func (e *Engine) configurePublication() error {
	p, err := e.publicationPolicy.Resolve()
	if err != nil {
		return err
	}
	e.publicationPolicy = p
	if e.publicationGate == nil && e.safety != nil {
		e.publicationGate = safety.ScannerGate{Scanner: e.safety}
	}
	return e.requirePublication(p)
}

func (e *Engine) requirePublication(p safety.Policy) error {
	if e.publicationPolicy.Required && p.Mode == safety.ModeDisabled {
		return safety.ErrUnavailable
	}
	if p.Required && e.publicationGate == nil {
		return safety.ErrUnavailable
	}
	if p.Mode == safety.ModeCheckedSegments {
		if _, ok := e.publicationGate.(safety.IncrementalGate); !ok {
			return safety.ErrUnavailable
		}
	}
	return nil
}

func (e *Engine) frozenPublication() safety.Policy {
	p := e.publicationPolicy
	// A snapshot prepared with a scanner must never silently run without one.
	if e.publicationGate != nil && p.Mode != safety.ModeDisabled {
		p.Required = true
	}
	return p
}

func publicationRequest(ctx context.Context, ag *agent.Config, r *run.Run, text, source string, direction safety.Direction) safety.PublicationRequest {
	scope := cortex.ScopeFromContext(ctx)
	req := safety.PublicationRequest{ScanRequest: safety.ScanRequest{Content: text, Direction: direction, AgentID: ag.ID.String(), ProfileName: extractSafetyProfile(ag), AppID: scanAppID(scope), TenantID: scope.Canonical()}, Source: source}
	if r != nil {
		req.RunID = r.ID.String()
		req.Metadata = map[string]any{"prepared_digest": r.Provenance.PreparedDigest, "source": source}
	}
	return req
}

func (e *Engine) checkPublication(ctx context.Context, ag *agent.Config, r *run.Run, p safety.Policy, text, source string, direction safety.Direction) (approvedText, error) {
	p, err := p.Resolve()
	if err != nil {
		return "", err
	}
	if policyErr := e.requirePublication(p); policyErr != nil {
		return "", policyErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return "", contextErr
	}
	limit := p.MaxOutputBytes
	if direction == safety.DirectionInput {
		limit = p.MaxInputBytes
	}
	if source == "tool_result" || source == "tool_error" {
		limit = p.MaxToolOutputBytes
	}
	if len(text) > limit {
		return "", safety.ErrLimit
	}
	decision := safety.PublicationDecision{Text: text, Status: safety.StatusUnscanned}
	if p.Mode != safety.ModeDisabled && e.publicationGate != nil {
		decision, err = e.publicationGate.Check(ctx, publicationRequest(ctx, ag, r, text, source, direction))
		if err != nil {
			return "", closedPublicationError(err)
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		switch decision.Status {
		case safety.StatusApproved, safety.StatusRedacted, safety.StatusFlagged:
		default:
			return "", safety.ErrUnavailable
		}
	}
	if len(decision.Text) > limit {
		return "", safety.ErrLimit
	}
	if r != nil {
		r.Provenance.PublicationStatus = mergePublicationStatus(r.Provenance.PublicationStatus, decision.Status)
	}
	return approvedText(decision.Text), nil
}

func closedPublicationError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, safety.ErrBlocked):
		return safety.ErrBlocked
	case errors.Is(err, safety.ErrLimit):
		return safety.ErrLimit
	default:
		return safety.ErrUnavailable
	}
}

func mergePublicationStatus(old, next safety.PublicationStatus) safety.PublicationStatus {
	rank := map[safety.PublicationStatus]int{"": 0, safety.StatusApproved: 1, safety.StatusFlagged: 2, safety.StatusRedacted: 3, safety.StatusUnscanned: 4}
	if rank[next] > rank[old] {
		return next
	}
	return old
}

func (e *Engine) checkResponse(ctx context.Context, ag *agent.Config, r *run.Run, p safety.Policy, content string, calls []llm.ToolCall) (approvedText, error) {
	total := len(content)
	for _, tc := range calls {
		for _, size := range []int{len(tc.ID), len(tc.Name), len(tc.Arguments), publicationToolOverhead} {
			if size > p.MaxOutputBytes-total {
				return "", safety.ErrLimit
			}
			total += size
		}
	}
	out, err := e.checkPublication(ctx, ag, r, p, content, "model_output", safety.DirectionOutput)
	if err != nil {
		return "", err
	}
	if callErr := e.checkToolCalls(ctx, ag, r, p, calls); callErr != nil {
		return "", callErr
	}
	return out, nil
}

func (e *Engine) checkToolCalls(ctx context.Context, ag *agent.Config, r *run.Run, p safety.Policy, calls []llm.ToolCall) error {
	total := 0
	for _, tc := range calls {
		// Tool names and IDs are public too. Never silently rewrite effect arguments
		// after approval; a redacted command needs a new model proposal.
		for _, size := range []int{len(tc.ID), len(tc.Name), len(tc.Arguments), publicationToolOverhead} {
			if size > p.MaxOutputBytes-total {
				return safety.ErrLimit
			}
			total += size
		}
		text := tc.ID + "\n" + tc.Name + "\n" + tc.Arguments
		out, err := e.checkPublication(ctx, ag, r, p, text, "tool_call", safety.DirectionOutput)
		if err != nil {
			return err
		}
		if string(out) != text {
			return safety.ErrBlocked
		}
	}
	return nil
}

func (e *Engine) checkMessages(ctx context.Context, ag *agent.Config, r *run.Run, p safety.Policy, messages []llm.Message) error {
	total := 0
	for i := range messages {
		if len(messages[i].Content) > p.MaxInputBytes-total {
			return safety.ErrLimit
		}
		total += len(messages[i].Content)
		out, err := e.checkPublication(ctx, ag, r, p, messages[i].Content, "history", safety.DirectionInput)
		if err != nil {
			return err
		}
		messages[i].Content = string(out)
		if callErr := e.checkToolCalls(ctx, ag, r, p, messages[i].ToolCalls); callErr != nil {
			return callErr
		}
	}
	return nil
}

func (e *Engine) stopPublication(ctx context.Context, r *run.Run, st *reactState, err error) error {
	reason := run.StopSafetyBlock
	if errors.Is(err, safety.ErrUnavailable) {
		reason = run.StopPolicyUnavailable
	}
	r.Error = closedPublicationError(err).Error()
	r.Usage = st.usage
	if endErr := e.finalizeRun(ctx, r, run.Outcome{StopReason: reason, StepCount: st.stepIndex, TokensUsed: st.tokensUsed}); endErr != nil {
		return errors.Join(closedPublicationError(err), endErr)
	}
	return closedPublicationError(err)
}

// sendEvent gives a disconnected consumer a cancellation path on every event,
// including the first and terminal events. Cancellation wins when already set.
func sendEvent(ctx context.Context, events chan<- StreamEvent, event StreamEvent) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case events <- event:
		return true
	}
}

func publishToken(ctx context.Context, events chan<- StreamEvent, text approvedText, index int) bool {
	if text == "" {
		return ctx.Err() == nil
	}
	return sendEvent(ctx, events, StreamEvent{Type: EventToken, Data: map[string]any{"content": string(text), "index": index}})
}

func (e *Engine) checkPreparedSources(ctx context.Context, ag *agent.Config, snap *preparedSnapshot) error {
	receipt := &run.Run{Provenance: snap.Provenance}
	total := 0
	for i := range snap.Sections {
		if snap.Sections[i].Source != prompt.SourceKnowledge {
			continue
		}
		if len(snap.Sections[i].Body) > snap.Config.Publication.MaxInputBytes-total {
			return safety.ErrLimit
		}
		total += len(snap.Sections[i].Body)
		out, err := e.checkPublication(ctx, ag, receipt, snap.Config.Publication, snap.Sections[i].Body, "retrieval", safety.DirectionInput)
		if err != nil {
			return err
		}
		snap.Sections[i].Body = string(out)
	}
	snap.Provenance.PublicationStatus = receipt.Provenance.PublicationStatus
	return nil
}

func (e *Engine) checkedToolError(ctx context.Context, subject cortex.Subject, err error) error {
	ag := &agent.Config{ID: subject.AgentID}
	r := &run.Run{ID: subject.RunID}
	p := e.frozenPublication()
	if binding, ok := ctx.Value(publicationContextKey{}).(publicationContext); ok {
		ag = binding.agent
		r = binding.run
		p = binding.policy
	}
	text, scanErr := e.checkToolResult(ctx, ag, r, p, err.Error(), "tool_error")
	if scanErr != nil {
		return closedPublicationError(scanErr)
	}
	if text == "" {
		return errors.New("cortex: tool execution failed")
	}
	if string(text) == err.Error() {
		return err
	}
	return errors.New(string(text))
}

func (e *Engine) checkedProviderError(ctx context.Context, ag *agent.Config, r *run.Run, p safety.Policy, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	text, scanErr := e.checkPublication(ctx, ag, r, p, err.Error(), "provider_error", safety.DirectionOutput)
	if scanErr != nil {
		return closedPublicationError(scanErr)
	}
	if text == "" {
		return errors.New("cortex: provider request failed")
	}
	if string(text) == err.Error() {
		return err
	}
	return errors.New(string(text))
}

func (e *Engine) checkToolResult(ctx context.Context, ag *agent.Config, r *run.Run, p safety.Policy, text, source string) (approvedText, error) {
	incoming, err := e.checkPublication(ctx, ag, r, p, text, source, safety.DirectionInput)
	if err != nil {
		return "", err
	}
	return e.checkPublication(ctx, ag, r, p, string(incoming), source, safety.DirectionOutput)
}

type publicationContextKey struct{}
type publicationContext struct {
	agent  *agent.Config
	run    *run.Run
	policy safety.Policy
}

func withPublicationContext(ctx context.Context, ag *agent.Config, r *run.Run, p safety.Policy) context.Context {
	return context.WithValue(ctx, publicationContextKey{}, publicationContext{agent: ag, run: r, policy: p})
}

func (e *Engine) cancelPublication(ctx context.Context, r *run.Run, steps, tokens int) {
	r.Usage.Steps = steps
	if err := e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopCancelled, StepCount: steps, TokensUsed: tokens}); err != nil {
		e.logger.Error("persist cancelled publication")
	}
}

func (e *Engine) validatePreparedPublication(ctx context.Context, snap *preparedSnapshot) (safety.PublicationStatus, error) {
	copySnapshot := *snap
	copySnapshot.Sections = append([]prompt.Section(nil), snap.Sections...)
	if err := e.checkPreparedSources(ctx, &snap.Agent, &copySnapshot); err != nil {
		return "", err
	}
	for i, section := range snap.Sections {
		if section.Body != copySnapshot.Sections[i].Body {
			return "", safety.ErrBlocked
		}
	}
	return copySnapshot.Provenance.PublicationStatus, nil
}

// Providers may normalize requests or reuse response buffers. Their mutable
// slices must not alias already approved engine state.
func cloneExecutionMessages(messages []llm.Message) []llm.Message {
	out := append([]llm.Message(nil), messages...)
	for i := range out {
		out[i].ToolCalls = append([]llm.ToolCall(nil), messages[i].ToolCalls...)
	}
	return out
}
