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
)

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
	// Everything from here on is new to this run. SaveConversation below
	// persists only messages[newMessagesFrom:] -- saving the whole slice
	// (history included) would re-insert the reloaded history as new
	// rows on every run, and LoadConversation's LIMIT 100/no-offset read
	// would then freeze on an ever-older prefix once duplication pushed
	// the row count past the limit, silently blinding the agent to
	// recent turns.
	newMessagesFrom := len(messages)
	messages = append(messages, llm.Message{Role: "user", Content: input})

	var totalTokens int
	var stepIndex int
	var finalOutput string

	// ReAct loop.
	for stepIndex < cfg.MaxSteps {
		stepStart := time.Now().UTC()
		e.extensions.EmitStepStarted(ctx, r.ID, stepIndex)

		// Built once per step and threaded to both authorizer calls
		// below (Visible via resolveTools, Authorize via executeTool)
		// so they judge the same subject.
		subject := cortex.Subject{Scope: scope, AgentID: ag.ID, RunID: r.ID}

		req := &llm.Request{
			Model:       cfg.Model,
			System:      systemPrompt,
			Messages:    messages,
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
				e.failRun(ctx, r, ag.ID, fmt.Errorf("safety: input blocked — %s", scanResult.Decision), now)
				return nil, fmt.Errorf("safety: input blocked by %s profile", scanResult.ProfileUsed)
			}
		}

		resp, err := e.llm.Complete(ctx, req)
		if err != nil {
			e.failRun(ctx, r, ag.ID, err, now)
			return nil, fmt.Errorf("llm complete: %w", err)
		}

		totalTokens += resp.Usage.TotalTokens

		// Record the step.
		stepEnd := time.Now().UTC()
		step := &run.Step{
			Entity:      cortex.NewEntity(),
			ID:          id.NewStepID(),
			RunID:       r.ID,
			Index:       stepIndex,
			Type:        "generation",
			Input:       lastContent(messages),
			Output:      resp.Content,
			TokensUsed:  resp.Usage.TotalTokens,
			StartedAt:   &stepStart,
			CompletedAt: &stepEnd,
		}
		if err := e.store.CreateStep(ctx, step); err != nil {
			e.logger.Error("create step", log.String("error", err.Error()))
		}

		e.extensions.EmitStepCompleted(ctx, r.ID, stepIndex, stepEnd.Sub(stepStart))
		stepIndex++

		// Check for tool calls.
		if len(resp.ToolCalls) > 0 {
			// Append assistant message with tool calls.
			messages = append(messages, llm.Message{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			// Execute each tool call.
			for _, tc := range resp.ToolCalls {
				tcStart := time.Now().UTC()
				e.extensions.EmitToolCalled(ctx, r.ID, tc.Name, tc.Arguments)

				result := e.executeTool(ctx, subject, tc)

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

				e.extensions.EmitToolCompleted(ctx, r.ID, tc.Name, result, tcEnd.Sub(tcStart))

				// Append tool result message.
				messages = append(messages, llm.Message{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
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
				e.failRun(ctx, r, ag.ID, fmt.Errorf("safety: output blocked — %s", scanResult.Decision), now)
				return nil, fmt.Errorf("safety: output blocked by %s profile", scanResult.ProfileUsed)
			} else if scanResult != nil && scanResult.Redacted != "" {
				finalOutput = scanResult.Redacted
			}
		}

		messages = append(messages, llm.Message{Role: "assistant", Content: finalOutput})
		break
	}

	// Save only the messages this run actually added -- not the whole
	// reloaded history alongside them. See newMessagesFrom's comment
	// above for why that distinction matters.
	convMsgs := llmToMemory(messages[newMessagesFrom:])
	if err := e.store.SaveConversation(ctx, ag.ID, sessionID, convMsgs); err != nil {
		e.logger.Error("save conversation", log.String("error", err.Error()))
	}

	// Complete the run.
	completedAt := time.Now().UTC()
	r.State = run.StateCompleted
	r.Output = finalOutput
	r.StepCount = stepIndex
	r.TokensUsed = totalTokens
	r.CompletedAt = &completedAt
	if err := e.store.UpdateRun(ctx, r); err != nil {
		e.logger.Error("update run", log.String("error", err.Error()))
	}

	e.extensions.EmitRunCompleted(ctx, ag.ID, r.ID, r.Output, completedAt.Sub(now))
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
		// See runReAct's identical comment: only messages[newMessagesFrom:]
		// get saved below, not the whole reloaded history.
		newMessagesFrom := len(messages)
		messages = append(messages, llm.Message{Role: "user", Content: input})

		var totalTokens int
		var stepIndex int
		var finalOutput string

		// ReAct loop.
		for stepIndex < cfg.MaxSteps {
			stepStart := time.Now().UTC()
			e.extensions.EmitStepStarted(ctx, r.ID, stepIndex)

			stepID := id.NewStepID()
			events <- StreamEvent{Type: EventStep, Data: map[string]any{
				"step_id": stepID.String(),
				"index":   stepIndex,
				"type":    "generation",
			}}

			// Built once per step and threaded to both authorizer calls
			// below (Visible via resolveTools, Authorize via executeTool)
			// so they judge the same subject.
			subject := cortex.Subject{Scope: scope, AgentID: ag.ID, RunID: r.ID}

			req := &llm.Request{
				Model:       cfg.Model,
				System:      systemPrompt,
				Messages:    messages,
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
					e.failRun(ctx, r, ag.ID, fmt.Errorf("safety: input blocked — %s", scanResult.Decision), now)
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
				e.failRun(ctx, r, ag.ID, err, now)
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
					e.failRun(ctx, r, ag.ID, err, now)
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
				totalTokens += u.TotalTokens
			}
			_ = stream.Close()

			// Record the step.
			stepEnd := time.Now().UTC()
			step := &run.Step{
				Entity:      cortex.NewEntity(),
				ID:          stepID,
				RunID:       r.ID,
				Index:       stepIndex,
				Type:        "generation",
				Input:       lastContent(messages),
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

			e.extensions.EmitStepCompleted(ctx, r.ID, stepIndex, stepEnd.Sub(stepStart))
			stepIndex++

			// Check for tool calls.
			if len(toolCalls) > 0 {
				messages = append(messages, llm.Message{
					Role:      "assistant",
					Content:   contentBuf,
					ToolCalls: toolCalls,
				})

				for _, tc := range toolCalls {
					tcStart := time.Now().UTC()
					e.extensions.EmitToolCalled(ctx, r.ID, tc.Name, tc.Arguments)

					events <- StreamEvent{Type: EventToolCall, Data: map[string]any{
						"tool_name": tc.Name,
						"arguments": tc.Arguments,
						"tool_id":   tc.ID,
					}}

					result := e.executeTool(ctx, subject, tc)

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

					e.extensions.EmitToolCompleted(ctx, r.ID, tc.Name, result, tcEnd.Sub(tcStart))

					messages = append(messages, llm.Message{
						Role:       "tool",
						Content:    result,
						ToolCallID: tc.ID,
					})
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
					e.failRun(ctx, r, ag.ID, fmt.Errorf("safety: output blocked — %s", scanResult.Decision), now)
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

			messages = append(messages, llm.Message{Role: "assistant", Content: finalOutput})
			break
		}

		// Save only the messages this run actually added -- see
		// runReAct's newMessagesFrom comment for why.
		convMsgs := llmToMemory(messages[newMessagesFrom:])
		if err := e.store.SaveConversation(ctx, ag.ID, sessionID, convMsgs); err != nil {
			e.logger.Error("save conversation", log.String("error", err.Error()))
		}

		// Complete the run.
		completedAt := time.Now().UTC()
		r.State = run.StateCompleted
		r.Output = finalOutput
		r.StepCount = stepIndex
		r.TokensUsed = totalTokens
		r.CompletedAt = &completedAt
		if err := e.store.UpdateRun(ctx, r); err != nil {
			e.logger.Error("update run", log.String("error", err.Error()))
		}

		e.extensions.EmitRunCompleted(ctx, ag.ID, r.ID, r.Output, completedAt.Sub(now))

		events <- StreamEvent{Type: EventDone, Data: map[string]any{
			"run_id":      r.ID.String(),
			"output":      finalOutput,
			"tokens_used": totalTokens,
			"duration_ms": completedAt.Sub(now).Milliseconds(),
		}}
	}()

	return nil
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
// filtered to names when non-empty, then to what the authorizer will let
// this subject see. A nil authorizer skips the Visible call and returns
// everything, same as before this seam existed.
func (e *Engine) resolveTools(ctx context.Context, s cortex.Subject, names []string) []llm.Tool {
	tools := e.builtinTools()
	for _, rt := range e.tools {
		tools = append(tools, rt.def)
	}
	if len(names) > 0 {
		tools = filterByName(tools, names)
	}
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

// executeTool executes a tool call and returns the result. The authorizer is
// consulted here even though resolveTools already filtered what the model
// was shown: a model can name a tool it was never shown, so Visible having
// filtered the list is not a substitute for gating the dispatch itself.
func (e *Engine) executeTool(ctx context.Context, s cortex.Subject, tc llm.ToolCall) string {
	if e.authorizer != nil {
		if err := e.authorizer.Authorize(ctx, s, tc); err != nil {
			e.extensions.EmitToolDenied(ctx, s.RunID, tc.Name, err.Error())
			return jsonResult("error", err.Error())
		}
	}

	inv := cortex.Invocation{Subject: s, Call: tc}

	if result, handled := e.executeBuiltinTool(ctx, inv); handled {
		return result
	}
	for _, rt := range e.tools {
		if rt.def.Name == tc.Name {
			out, err := rt.handler(ctx, inv)
			if err != nil {
				e.extensions.EmitToolFailed(ctx, s.RunID, tc.Name, err)
				return jsonResult("error", err.Error())
			}
			return out
		}
	}
	return jsonResult("error", fmt.Sprintf("unknown tool %q", tc.Name))
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
