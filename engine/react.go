package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/suspension"
)

// reactState is the mutable state one pass of the ReAct loop carries
// across steps. It is deliberately the same set of fields a
// suspension.Continuation stores: a suspension is a serialized reactState
// plus the pending calls, and a resume rehydrates one of these rather
// than reconstructing the loop's local variables by hand.
type reactState struct {
	messages     []llm.Message
	systemPrompt string
	stepIndex    int
	tokensUsed   int

	// newMessagesFrom is where this run's own messages begin. Everything
	// before it is history the run loaded at startup, and saving it again
	// is the v1.8.0 defect that made agents go deaf after about six runs.
	newMessagesFrom int

	// sessionID is the session the run loads from and saves into. A
	// resume restores it rather than re-resolving it, because
	// resolveSession lazily creates a default session under a create
	// race and could land on a different one after the pause.
	sessionID id.SessionID
}

// continuation snapshots the state for a suspension row.
func (s *reactState) continuation() suspension.Continuation {
	return suspension.Continuation{
		Messages:        s.messages,
		SystemPrompt:    s.systemPrompt,
		StepIndex:       s.stepIndex,
		TokensUsed:      s.tokensUsed,
		NewMessagesFrom: s.newMessagesFrom,
		SessionID:       s.sessionID,
	}
}

// stateFromContinuation is continuation's inverse, and the only way a
// resumed loop gets its state.
func stateFromContinuation(cont suspension.Continuation) *reactState {
	return &reactState{
		messages:        cont.Messages,
		systemPrompt:    cont.SystemPrompt,
		stepIndex:       cont.StepIndex,
		tokensUsed:      cont.TokensUsed,
		newMessagesFrom: cont.NewMessagesFrom,
		sessionID:       cont.SessionID,
	}
}

// runReAct executes an agent using the ReAct reasoning loop synchronously.
func (e *Engine) runReAct(ctx context.Context, ag *agent.Config, input string, overrides *RunOverrides) (*run.Run, error) {
	scope := cortex.ScopeFromContext(ctx)

	// Resolved once here and threaded to every conversation call below,
	// rather than re-resolved at each call site: resolveSession lazily
	// creates the agent's default session under a create race, and
	// calling it more than once per run risks two different calls
	// landing on two different freshly created "default" sessions.
	sessionID, err := e.resolveSession(ctx, ag.ID, overrideSessionID(overrides))
	if err != nil {
		return nil, fmt.Errorf("resolve session: %w", err)
	}

	cfg := e.effectiveConfig(ag, overrides)
	systemPrompt, err := e.BuildSystemPrompt(ctx, ag, overrides)
	if err != nil {
		return nil, fmt.Errorf("build system prompt: %w", err)
	}

	now := time.Now().UTC()
	r := &run.Run{
		Entity:     cortex.NewEntity(),
		ID:         id.NewAgentRunID(),
		AgentID:    ag.ID,
		SessionID:  sessionID,
		Scope:      scope,
		State:      run.StateRunning,
		Input:      input,
		StartedAt:  &now,
		PersonaRef: cfg.PersonaRef,
	}
	if err := e.store.CreateRun(ctx, r); err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}

	e.extensions.EmitRunStarted(ctx, ag.ID, r.ID, input)

	// Load conversation history.
	history, _ := e.store.LoadConversation(ctx, ag.ID, sessionID, 100) //nolint:errcheck // best-effort history load
	messages := memoryToLLM(history)
	st := &reactState{
		messages:     messages,
		systemPrompt: systemPrompt,
		sessionID:    sessionID,
		// Everything from here on is new to this run. SaveConversation
		// persists only messages[newMessagesFrom:] -- saving the whole
		// slice (history included) would re-insert the reloaded history
		// as new rows on every run, and LoadConversation's LIMIT
		// 100/no-offset read would then freeze on an ever-older prefix
		// once duplication pushed the row count past the limit, silently
		// blinding the agent to recent turns.
		newMessagesFrom: len(messages),
	}
	st.messages = append(st.messages, llm.Message{Role: "user", Content: input})

	return e.continueReAct(ctx, ag, cfg, r, st, input, now)
}

// continueReAct runs the ReAct loop over a run that already exists and a
// state that is already assembled. A fresh run and a resumed one differ
// only in how that state was built, from a loaded history or from a
// suspension's continuation, so the two share this loop rather than the
// resume path carrying a second copy of it.
//
// startedAt is when the RUN started, not when this pass did, so a
// resumed run reports its full wall-clock duration rather than only the
// stretch after the pause.
func (e *Engine) continueReAct(ctx context.Context, ag *agent.Config, cfg resolvedConfig, r *run.Run, st *reactState, input string, startedAt time.Time) (*run.Run, error) {
	scope := cortex.ScopeFromContext(ctx)

	var finalOutput string

	// ReAct loop.
	for st.stepIndex < cfg.MaxSteps {
		stepStart := time.Now().UTC()
		e.extensions.EmitStepStarted(ctx, r.ID, st.stepIndex)

		// Built once per step and threaded to both authorizer calls
		// below (Visible via resolveTools, Authorize via executeTool)
		// so they judge the same subject.
		subject := cortex.Subject{Scope: scope, Principal: cortex.PrincipalFromContext(ctx), AgentID: ag.ID, RunID: r.ID}

		req := &llm.Request{
			Model:       cfg.Model,
			System:      st.systemPrompt,
			Messages:    st.messages,
			MaxTokens:   cfg.MaxTokens,
			Temperature: cfg.Temperature,
			Tools:       e.resolveTools(ctx, subject, cfg.Tools),
		}

		// Safety: scan input before LLM call.
		if e.safety != nil {
			scanReq := &safety.ScanRequest{
				Content:     input,
				Direction:   safety.DirectionInput,
				AgentID:     ag.ID.String(),
				RunID:       r.ID.String(),
				ProfileName: extractSafetyProfile(ag),
				AppID:       scanAppID(scope),
				TenantID:    scope.Canonical(),
			}
			if scanResult, scanErr := e.safety.ScanInput(ctx, scanReq); scanErr != nil {
				e.logger.Warn("safety scan input error", log.String("error", scanErr.Error()))
			} else if scanResult != nil && scanResult.Blocked {
				e.failRun(ctx, r, ag.ID, fmt.Errorf("safety: input blocked — %s", scanResult.Decision), startedAt)
				return nil, fmt.Errorf("safety: input blocked by %s profile", scanResult.ProfileUsed)
			}
		}

		resp, err := e.llm.Complete(ctx, req)
		if err != nil {
			e.failRun(ctx, r, ag.ID, err, startedAt)
			return nil, fmt.Errorf("llm complete: %w", err)
		}

		st.tokensUsed += resp.Usage.TotalTokens

		// Record the step.
		stepEnd := time.Now().UTC()
		step := &run.Step{
			Entity:      cortex.NewEntity(),
			ID:          id.NewStepID(),
			RunID:       r.ID,
			Index:       st.stepIndex,
			Type:        "generation",
			Input:       lastContent(st.messages),
			Output:      resp.Content,
			TokensUsed:  resp.Usage.TotalTokens,
			StartedAt:   &stepStart,
			CompletedAt: &stepEnd,
		}
		if err := e.store.CreateStep(ctx, step); err != nil {
			e.logger.Error("create step", log.String("error", err.Error()))
		}

		e.extensions.EmitStepCompleted(ctx, r.ID, st.stepIndex, stepEnd.Sub(stepStart))
		st.stepIndex++

		// Check for tool calls.
		if len(resp.ToolCalls) > 0 {
			// Append assistant message with tool calls.
			st.messages = append(st.messages, llm.Message{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			// Execute each tool call. A call that turns out to be
			// pending is collected rather than recorded: the whole step
			// runs first, and one suspension carrying every pending call
			// is written after it.
			var pending []suspension.PendingCall
			// One reason per step, bound where the pending calls are
			// collected and handed to suspend from here. Task 5 sets
			// this from executeTool's classification instead of a
			// literal; nothing downstream, including the persisted row,
			// gets to name a reason of its own.
			reason := suspension.ReasonExternalTool
			for _, tc := range resp.ToolCalls {
				tcStart := time.Now().UTC()
				e.extensions.EmitToolCalled(ctx, r.ID, tc.Name, tc.Arguments)

				result, outcome := e.executeTool(ctx, subject, tc)
				if outcome == outcomePending {
					// No tool call row: nothing ran, and a row with a
					// completion timestamp on it would be a lie the
					// resume has no way to correct (run.Store has no
					// update for tool calls). No terminal plugin event
					// either, and no tool result message: the model must
					// not see an empty result standing in for a call
					// that is still outstanding.
					pending = append(pending, pendingCall(tc))
					continue
				}

				tcEnd := time.Now().UTC()
				toolCall := &run.ToolCall{
					Entity:      cortex.NewEntity(),
					ID:          id.NewToolCallID(),
					StepID:      step.ID,
					RunID:       r.ID,
					ToolName:    tc.Name,
					Arguments:   tc.Arguments,
					Result:      result,
					StartedAt:   &tcStart,
					CompletedAt: &tcEnd,
				}
				if err := e.store.CreateToolCall(ctx, toolCall); err != nil {
					e.logger.Error("create tool call", log.String("error", err.Error()))
				}

				// executeTool already emitted the terminal event for a denial
				// or a failure. Emitting completed here too would report the
				// same call twice, with the second one calling it a success.
				if outcome == outcomeCompleted {
					e.extensions.EmitToolCompleted(ctx, r.ID, tc.Name, result, tcEnd.Sub(tcStart))
				}

				// Append tool result message.
				st.messages = append(st.messages, llm.Message{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
			}

			if len(pending) > 0 {
				cont := st.continuation()
				if err := e.suspend(ctx, r, reason, pending, cont); err != nil {
					e.failRun(ctx, r, ag.ID, err, startedAt)
					return nil, fmt.Errorf("suspend run: %w", err)
				}
				return r, nil
			}
			continue // Continue the ReAct loop.
		}

		// No tool calls — this is the final response.
		finalOutput = resp.Content

		// Safety: scan output before returning.
		if e.safety != nil {
			scanReq := &safety.ScanRequest{
				Content:     finalOutput,
				Direction:   safety.DirectionOutput,
				AgentID:     ag.ID.String(),
				RunID:       r.ID.String(),
				ProfileName: extractSafetyProfile(ag),
				AppID:       scanAppID(scope),
				TenantID:    scope.Canonical(),
			}
			if scanResult, scanErr := e.safety.ScanOutput(ctx, scanReq); scanErr != nil {
				e.logger.Warn("safety scan output error", log.String("error", scanErr.Error()))
			} else if scanResult != nil && scanResult.Blocked {
				e.failRun(ctx, r, ag.ID, fmt.Errorf("safety: output blocked — %s", scanResult.Decision), startedAt)
				return nil, fmt.Errorf("safety: output blocked by %s profile", scanResult.ProfileUsed)
			} else if scanResult != nil && scanResult.Redacted != "" {
				finalOutput = scanResult.Redacted
			}
		}

		st.messages = append(st.messages, llm.Message{Role: "assistant", Content: finalOutput})
		break
	}

	// Save only the messages this run actually added, not the whole
	// reloaded history alongside them. See newMessagesFrom's comment on
	// reactState for why that distinction matters.
	convMsgs := llmToMemory(st.messages[st.newMessagesFrom:])
	if err := e.store.SaveConversation(ctx, ag.ID, st.sessionID, convMsgs); err != nil {
		e.logger.Error("save conversation", log.String("error", err.Error()))
	}

	// Complete the run.
	completedAt := time.Now().UTC()
	r.State = run.StateCompleted
	r.Output = finalOutput
	r.StepCount = st.stepIndex
	r.TokensUsed = st.tokensUsed
	r.CompletedAt = &completedAt
	if err := e.store.UpdateRun(ctx, r); err != nil {
		e.logger.Error("update run", log.String("error", err.Error()))
	}

	e.extensions.EmitRunCompleted(ctx, ag.ID, r.ID, r.Output, completedAt.Sub(startedAt))
	return r, nil
}

// streamReAct executes an agent using the ReAct reasoning loop with streaming.
func (e *Engine) streamReAct(ctx context.Context, ag *agent.Config, input string, overrides *RunOverrides, events chan<- StreamEvent) error {
	scope := cortex.ScopeFromContext(ctx)

	// Resolved once here and threaded to every conversation call below,
	// same as runReAct: resolveSession lazily creates the agent's
	// default session under a create race, and calling it more than
	// once per run risks two different calls landing on two different
	// freshly created "default" sessions.
	sessionID, err := e.resolveSession(ctx, ag.ID, overrideSessionID(overrides))
	if err != nil {
		close(events)
		return fmt.Errorf("resolve session: %w", err)
	}

	cfg := e.effectiveConfig(ag, overrides)
	systemPrompt, err := e.BuildSystemPrompt(ctx, ag, overrides)
	if err != nil {
		close(events)
		return fmt.Errorf("build system prompt: %w", err)
	}

	now := time.Now().UTC()
	r := &run.Run{
		Entity:     cortex.NewEntity(),
		ID:         id.NewAgentRunID(),
		AgentID:    ag.ID,
		SessionID:  sessionID,
		Scope:      scope,
		State:      run.StateRunning,
		Input:      input,
		StartedAt:  &now,
		PersonaRef: cfg.PersonaRef,
	}
	if err := e.store.CreateRun(ctx, r); err != nil {
		close(events)
		return fmt.Errorf("create run: %w", err)
	}

	e.extensions.EmitRunStarted(ctx, ag.ID, r.ID, input)

	go func() {
		defer close(events)

		events <- StreamEvent{Type: EventRunStarted, Data: map[string]any{
			"run_id":   r.ID.String(),
			"agent_id": ag.ID.String(),
		}}

		// Load conversation history.
		history, _ := e.store.LoadConversation(ctx, ag.ID, sessionID, 100) //nolint:errcheck // best-effort history load
		messages := memoryToLLM(history)
		st := &reactState{
			messages:     messages,
			systemPrompt: systemPrompt,
			sessionID:    sessionID,
			// See runReAct's identical comment: only
			// messages[newMessagesFrom:] get saved below, not the whole
			// reloaded history.
			newMessagesFrom: len(messages),
		}
		st.messages = append(st.messages, llm.Message{Role: "user", Content: input})

		e.continueStreamReAct(ctx, ag, cfg, r, st, input, now, events)
	}()

	return nil
}

// continueStreamReAct is continueReAct's streaming twin: same contract,
// same reason for existing, over the streaming loop instead. It does not
// close events, because the goroutine that calls it owns the channel.
func (e *Engine) continueStreamReAct(ctx context.Context, ag *agent.Config, cfg resolvedConfig, r *run.Run, st *reactState, input string, startedAt time.Time, events chan<- StreamEvent) {
	scope := cortex.ScopeFromContext(ctx)

	var finalOutput string

	// ReAct loop.
	for st.stepIndex < cfg.MaxSteps {
		stepStart := time.Now().UTC()
		e.extensions.EmitStepStarted(ctx, r.ID, st.stepIndex)

		stepID := id.NewStepID()
		events <- StreamEvent{Type: EventStep, Data: map[string]any{
			"step_id": stepID.String(),
			"index":   st.stepIndex,
			"type":    "generation",
		}}

		// Built once per step and threaded to both authorizer calls
		// below (Visible via resolveTools, Authorize via executeTool)
		// so they judge the same subject.
		subject := cortex.Subject{Scope: scope, Principal: cortex.PrincipalFromContext(ctx), AgentID: ag.ID, RunID: r.ID}

		req := &llm.Request{
			Model:       cfg.Model,
			System:      st.systemPrompt,
			Messages:    st.messages,
			MaxTokens:   cfg.MaxTokens,
			Temperature: cfg.Temperature,
			Tools:       e.resolveTools(ctx, subject, cfg.Tools),
		}

		// Safety: scan input before LLM call.
		if e.safety != nil {
			scanReq := &safety.ScanRequest{
				Content:     input,
				Direction:   safety.DirectionInput,
				AgentID:     ag.ID.String(),
				RunID:       r.ID.String(),
				ProfileName: extractSafetyProfile(ag),
				AppID:       scanAppID(scope),
				TenantID:    scope.Canonical(),
			}
			if scanResult, scanErr := e.safety.ScanInput(ctx, scanReq); scanErr != nil {
				e.logger.Warn("safety scan input error", log.String("error", scanErr.Error()))
			} else if scanResult != nil && scanResult.Blocked {
				e.failRun(ctx, r, ag.ID, fmt.Errorf("safety: input blocked — %s", scanResult.Decision), startedAt)
				events <- StreamEvent{Type: EventSafetyBlock, Data: map[string]any{
					"direction": "input",
					"decision":  string(scanResult.Decision),
					"profile":   scanResult.ProfileUsed,
				}}
				return
			}
		}

		stream, err := e.llm.CompleteStream(ctx, req)
		if err != nil {
			e.failRun(ctx, r, ag.ID, err, startedAt)
			events <- StreamEvent{Type: EventError, Data: map[string]any{
				"message": err.Error(),
			}}
			return
		}

		// Read all chunks from the stream.
		var contentBuf string
		var toolCalls []llm.ToolCall
		tokenIndex := 0

		for {
			select {
			case <-ctx.Done():
				_ = stream.Close()
				r.State = run.StateCancelled
				completedAt := time.Now().UTC()
				r.CompletedAt = &completedAt
				// ctx is already cancelled here, so a store write using
				// it outright would fail before it starts and the
				// cancel state would never persist, leaving the run
				// stuck at "running". WithoutCancel keeps every context
				// value (including scope) while dropping the
				// cancellation signal for this one terminal write.
				if err := e.store.UpdateRun(context.WithoutCancel(ctx), r); err != nil {
					e.logger.Error("update run on cancel", log.String("error", err.Error()))
				}
				events <- StreamEvent{Type: EventError, Data: map[string]any{"message": "cancelled"}}
				return
			default:
			}

			chunk, err := stream.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				_ = stream.Close()
				e.failRun(ctx, r, ag.ID, err, startedAt)
				events <- StreamEvent{Type: EventError, Data: map[string]any{
					"message": err.Error(),
				}}
				return
			}

			if chunk.Content != "" {
				contentBuf += chunk.Content
				events <- StreamEvent{Type: EventToken, Data: map[string]any{
					"content": chunk.Content,
					"index":   tokenIndex,
				}}
				tokenIndex++
			}

			if len(chunk.ToolCalls) > 0 {
				toolCalls = mergeToolCallDeltas(toolCalls, chunk.ToolCalls)
			}
		}

		// Collect usage from stream.
		if u := stream.Usage(); u != nil {
			st.tokensUsed += u.TotalTokens
		}
		_ = stream.Close()

		// Record the step.
		stepEnd := time.Now().UTC()
		step := &run.Step{
			Entity:      cortex.NewEntity(),
			ID:          stepID,
			RunID:       r.ID,
			Index:       st.stepIndex,
			Type:        "generation",
			Input:       lastContent(st.messages),
			Output:      contentBuf,
			StartedAt:   &stepStart,
			CompletedAt: &stepEnd,
		}
		if u := stream.Usage(); u != nil {
			step.TokensUsed = u.TotalTokens
		}
		if err := e.store.CreateStep(ctx, step); err != nil {
			e.logger.Error("create step", log.String("error", err.Error()))
		}

		e.extensions.EmitStepCompleted(ctx, r.ID, st.stepIndex, stepEnd.Sub(stepStart))
		st.stepIndex++

		// Check for tool calls.
		if len(toolCalls) > 0 {
			st.messages = append(st.messages, llm.Message{
				Role:      "assistant",
				Content:   contentBuf,
				ToolCalls: toolCalls,
			})

			// Same collect-then-suspend shape as the synchronous
			// loop: see its comments for why a pending call gets no
			// row, no terminal event and no result message.
			var pending []suspension.PendingCall
			// One reason per step, same as the synchronous loop: the
			// event below reports what suspend was actually given
			// rather than naming a reason of its own.
			reason := suspension.ReasonExternalTool
			for _, tc := range toolCalls {
				tcStart := time.Now().UTC()
				e.extensions.EmitToolCalled(ctx, r.ID, tc.Name, tc.Arguments)

				events <- StreamEvent{Type: EventToolCall, Data: map[string]any{
					"tool_name": tc.Name,
					"arguments": tc.Arguments,
					"tool_id":   tc.ID,
				}}

				result, outcome := e.executeTool(ctx, subject, tc)
				if outcome == outcomePending {
					pending = append(pending, pendingCall(tc))
					continue
				}

				tcEnd := time.Now().UTC()
				toolCall := &run.ToolCall{
					Entity:      cortex.NewEntity(),
					ID:          id.NewToolCallID(),
					StepID:      step.ID,
					RunID:       r.ID,
					ToolName:    tc.Name,
					Arguments:   tc.Arguments,
					Result:      result,
					StartedAt:   &tcStart,
					CompletedAt: &tcEnd,
				}
				if err := e.store.CreateToolCall(ctx, toolCall); err != nil {
					e.logger.Error("create tool call", log.String("error", err.Error()))
				}

				// Same as the non-streaming loop: one terminal event per
				// call, and executeTool already emitted it for a denial
				// or a failure.
				if outcome == outcomeCompleted {
					e.extensions.EmitToolCompleted(ctx, r.ID, tc.Name, result, tcEnd.Sub(tcStart))
				}

				st.messages = append(st.messages, llm.Message{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
			}

			if len(pending) > 0 {
				cont := st.continuation()
				if err := e.suspend(ctx, r, reason, pending, cont); err != nil {
					e.failRun(ctx, r, ag.ID, err, startedAt)
					events <- StreamEvent{Type: EventError, Data: map[string]any{
						"message": err.Error(),
					}}
					return
				}
				// A streaming caller that never learns the run paused
				// would see the channel close after the tool_call
				// event and read it as a run that just stopped. It
				// needs the pending calls too: it is the one that has
				// to execute them.
				events <- StreamEvent{Type: EventSuspended, Data: map[string]any{
					"run_id":  r.ID.String(),
					"reason":  string(reason),
					"pending": pending,
				}}
				return
			}
			continue // Continue the ReAct loop.
		}

		// No tool calls — this is the final response.
		finalOutput = contentBuf

		// Safety: scan output before returning.
		if e.safety != nil {
			scanReq := &safety.ScanRequest{
				Content:     finalOutput,
				Direction:   safety.DirectionOutput,
				AgentID:     ag.ID.String(),
				RunID:       r.ID.String(),
				ProfileName: extractSafetyProfile(ag),
				AppID:       scanAppID(scope),
				TenantID:    scope.Canonical(),
			}
			if scanResult, scanErr := e.safety.ScanOutput(ctx, scanReq); scanErr != nil {
				e.logger.Warn("safety scan output error", log.String("error", scanErr.Error()))
			} else if scanResult != nil && scanResult.Blocked {
				e.failRun(ctx, r, ag.ID, fmt.Errorf("safety: output blocked — %s", scanResult.Decision), startedAt)
				events <- StreamEvent{Type: EventSafetyBlock, Data: map[string]any{
					"direction": "output",
					"decision":  string(scanResult.Decision),
					"profile":   scanResult.ProfileUsed,
				}}
				return
			} else if scanResult != nil && scanResult.Redacted != "" {
				finalOutput = scanResult.Redacted
			}
		}

		st.messages = append(st.messages, llm.Message{Role: "assistant", Content: finalOutput})
		break
	}

	// Save only the messages this run actually added, same as
	// continueReAct. See newMessagesFrom's comment on reactState.
	convMsgs := llmToMemory(st.messages[st.newMessagesFrom:])
	if err := e.store.SaveConversation(ctx, ag.ID, st.sessionID, convMsgs); err != nil {
		e.logger.Error("save conversation", log.String("error", err.Error()))
	}

	// Complete the run.
	completedAt := time.Now().UTC()
	r.State = run.StateCompleted
	r.Output = finalOutput
	r.StepCount = st.stepIndex
	r.TokensUsed = st.tokensUsed
	r.CompletedAt = &completedAt
	if err := e.store.UpdateRun(ctx, r); err != nil {
		e.logger.Error("update run", log.String("error", err.Error()))
	}

	e.extensions.EmitRunCompleted(ctx, ag.ID, r.ID, r.Output, completedAt.Sub(startedAt))

	events <- StreamEvent{Type: EventDone, Data: map[string]any{
		"run_id":      r.ID.String(),
		"output":      finalOutput,
		"tokens_used": st.tokensUsed,
		"duration_ms": completedAt.Sub(startedAt).Milliseconds(),
	}}
}

// ──────────────────────────────────────────────────
// Helper functions
// ──────────────────────────────────────────────────

// failRun marks a run as failed and emits the RunFailed hook.
func (e *Engine) failRun(ctx context.Context, r *run.Run, agentID id.AgentID, runErr error, _ time.Time) {
	completedAt := time.Now().UTC()
	r.State = run.StateFailed
	r.Error = runErr.Error()
	r.CompletedAt = &completedAt
	// A failure can arrive as an error from stream.Next on a ctx that's
	// already cancelled (as opposed to going through the ctx.Done()
	// branch above, which was already fixed) — a write using ctx as-is
	// would then fail before it starts and the run would stay "running"
	// forever instead of recording StateFailed. WithoutCancel keeps
	// every context value (including scope) while dropping the
	// cancellation signal for this one terminal write, exactly like the
	// cancel branches do.
	if err := e.store.UpdateRun(context.WithoutCancel(ctx), r); err != nil {
		e.logger.Error("update run on failure", log.String("error", err.Error()))
	}
	// Same reasoning as the store write above: a hook subscriber (audit,
	// for instance) that does its own I/O keyed on ctx would otherwise
	// silently drop the failure event on an already-cancelled ctx, even
	// though the store now correctly recorded it.
	e.extensions.EmitRunFailed(context.WithoutCancel(ctx), agentID, r.ID, runErr)
}

// resolveTools converts tool name references to llm.Tool definitions,
// then narrows them to what the authorizer will let this subject see. A
// nil authorizer skips the Visible call and returns everything, same as
// before this seam existed.
//
// names comes from an agent's cfg.Tools, and it filters registered tools
// only. Builtins are always included when the config that creates them is
// present: an agent with Knowledge configured would otherwise lose
// knowledge_search the moment it named a single registered tool, and
// nothing would tell it its knowledge config had gone dead. A host that
// wants a builtin withheld denies it in ToolAuthorizer.Visible, which is
// the actual security boundary. cfg.Tools is not one.
//
// External tools sit on the registered side of that line, not the builtin
// side. A builtin is exempt because it exists as a consequence of engine
// configuration the agent cannot see, so filtering it would kill that
// configuration silently. An external tool is a host registration exactly
// like WithTool, differing only in who executes it, and cfg.Tools is how
// an agent picks among host registrations. An agent that names its tools
// and does not name an external one has said it does not use it, and
// advertising it anyway would suspend runs the agent never asked to have
// suspended.
func (e *Engine) resolveTools(ctx context.Context, s cortex.Subject, names []string) []llm.Tool {
	registered := make([]llm.Tool, 0, len(e.tools)+len(e.externalTools))
	for _, rt := range e.tools {
		registered = append(registered, rt.def)
	}
	registered = append(registered, e.externalTools...)
	if len(names) > 0 {
		registered = filterByName(registered, names)
	}

	tools := append(e.builtinTools(), registered...)
	if e.authorizer != nil {
		tools = e.authorizer.Visible(ctx, s, tools)
	}
	return tools
}

// filterByName keeps only the tools whose name appears in names,
// preserving the order tools were assembled in.
func filterByName(tools []llm.Tool, names []string) []llm.Tool {
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	out := make([]llm.Tool, 0, len(tools))
	for _, t := range tools {
		if keep[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

// toolOutcome says how a tool call ended, so the caller knows whether a
// terminal plugin event is still owed. Exactly one of ToolCompleted,
// ToolFailed and ToolDenied fires per call: executeTool emits the failed
// and denied ones itself, and the ReAct loop emits completed only when it
// gets outcomeCompleted back. Before this existed the loop emitted
// ToolCompleted unconditionally, so a subscriber counting completions
// counted every denial and every failure as a success.
type toolOutcome int

const (
	outcomeCompleted toolOutcome = iota
	outcomeFailed
	outcomeDenied
	// outcomePending means the call is waiting on something outside the
	// engine and has not ended at all yet, so none of the three terminal
	// events is owed for it. The loop collects these across the whole
	// step and suspends the run once, after the step, rather than each
	// site that can produce one suspending on its own: two suspend
	// triggers in one step is how a step suspends twice, or loses the
	// results of the sibling calls that did run.
	outcomePending
)

// executeTool executes a tool call and returns the result plus how it ended.
// The authorizer is consulted here even though resolveTools already filtered
// what the model was shown: a model can name a tool it was never shown, so
// Visible having filtered the list is not a substitute for gating the
// dispatch itself.
//
// The result string is the same in all three terminal outcomes, an error
// payload for a denial or a failure, and it always flows back to the model.
// Only the plugin event differs. outcomePending is the exception: nothing ran,
// so there is no result, and the empty string it returns must not be fed back
// to the model as one.
func (e *Engine) executeTool(ctx context.Context, s cortex.Subject, tc llm.ToolCall) (string, toolOutcome) {
	if e.authorizer != nil {
		if err := e.authorizer.Authorize(ctx, s, tc); err != nil {
			e.extensions.EmitToolDenied(ctx, s.RunID, tc.Name, err.Error())
			return jsonResult("error", err.Error()), outcomeDenied
		}
	}

	inv := cortex.Invocation{Subject: s, Call: tc}

	if result, handled := e.executeBuiltinTool(ctx, inv); handled {
		return result, outcomeCompleted
	}
	for _, rt := range e.tools {
		if rt.def.Name == tc.Name {
			out, err := rt.handler(ctx, inv)
			if err != nil {
				e.extensions.EmitToolFailed(ctx, s.RunID, tc.Name, err)
				return jsonResult("error", err.Error()), outcomeFailed
			}
			return out, outcomeCompleted
		}
	}

	// External tools are matched after the registered ones so a host that
	// registers both under one name keeps the executable registration,
	// same as the first-match-wins rule WithTool already documents.
	// Nothing runs here and no terminal event fires: the call has not
	// completed, failed or been denied, it is waiting on the caller.
	for _, def := range e.externalTools {
		if def.Name == tc.Name {
			return "", outcomePending
		}
	}

	// A name that matches nothing is a failure like any other, and it gets
	// the same terminal event. Reporting it as completed would put a tool
	// that never ran into a subscriber's success count.
	err := fmt.Errorf("unknown tool %q", tc.Name)
	e.extensions.EmitToolFailed(ctx, s.RunID, tc.Name, err)
	return jsonResult("error", err.Error()), outcomeFailed
}

// memoryToLLM converts memory messages to llm messages.
func memoryToLLM(msgs []memory.Message) []llm.Message {
	out := make([]llm.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, llm.Message{
			Role:    m.Role,
			Content: m.Content,
		})
	}
	return out
}

// llmToMemory converts llm messages to memory messages for persistence.
func llmToMemory(msgs []llm.Message) []memory.Message {
	out := make([]memory.Message, 0, len(msgs))
	for _, m := range msgs {
		// Skip tool messages from conversation history — they're stored as ToolCall records.
		if m.Role == "tool" {
			continue
		}
		mm := memory.Message{
			Role:      m.Role,
			Content:   m.Content,
			Timestamp: time.Now().UTC(),
		}
		if len(m.ToolCalls) > 0 {
			tcs := make([]any, len(m.ToolCalls))
			for i, tc := range m.ToolCalls {
				tcs[i] = map[string]string{
					"id":        tc.ID,
					"name":      tc.Name,
					"arguments": tc.Arguments,
				}
			}
			mm.ToolCalls = tcs
		}
		out = append(out, mm)
	}
	return out
}

// lastContent returns the content of the last message.
func lastContent(msgs []llm.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1].Content
}

// extractSafetyProfile extracts the shield safety profile name from agent guardrails.
func extractSafetyProfile(ag *agent.Config) string {
	if ag.Guardrails == nil {
		return ""
	}
	if profile, ok := ag.Guardrails["shield_profile"].(string); ok {
		return profile
	}
	return ""
}

// scanAppID derives the app dimension for a safety.ScanRequest from scope.
// safety.ScanRequest.AppID and TenantID are distinct dimensions to a host
// scanner (see shield.WithApp vs shield.WithTenant), so a host that still
// wants per-app scanner policies must declare an "app" scope level — its
// value is used verbatim here. A host without one gets the same canonical
// string as TenantID, which is what every host got before this scope
// conversion existed.
func scanAppID(scope cortex.Scope) string {
	if v, ok := scope.Get("app"); ok {
		return v
	}
	return scope.Canonical()
}

// mergeToolCallDeltas accumulates streaming tool call deltas into complete tool calls.
func mergeToolCallDeltas(existing, deltas []llm.ToolCall) []llm.ToolCall {
	for _, d := range deltas {
		found := false
		for i := range existing {
			if existing[i].ID == d.ID && d.ID != "" {
				// Append arguments to existing tool call.
				existing[i].Arguments += d.Arguments
				if d.Name != "" {
					existing[i].Name = d.Name
				}
				found = true
				break
			}
		}
		if !found {
			existing = append(existing, d)
		}
	}
	return existing
}
