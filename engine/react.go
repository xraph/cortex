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
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

// reactState is the mutable state one pass of the ReAct loop carries
// across steps. It is deliberately the same set of fields a
// suspension.Continuation stores: a suspension is a serialized reactState
// plus the pending calls, and a resume rehydrates one of these rather
// than reconstructing the loop's local variables by hand.
type reactState struct {
	prepared         *PreparedAgent
	preparedSnapshot *preparedSnapshot
	provenance       run.ExecutionProvenance
	usage            execution.Usage
	messages         []llm.Message
	toolResults      []verification.ToolResult
	systemPrompt     string
	stepIndex        int
	tokensUsed       int

	// newMessagesFrom is where this run's own messages begin. Everything
	// before it is history the run loaded at startup, and saving it again
	// is the v1.8.0 defect that made agents go deaf after about six runs.
	newMessagesFrom int

	// sessionID is the session the run loads from and saves into. A
	// resume restores it rather than re-resolving it, because
	// resolveSession lazily creates a default session under a create
	// race and could land on a different one after the pause.
	sessionID id.SessionID

	// stepRows are step rows a reclaimed run's previous worker already
	// wrote, by index. The loop reuses one rather than writing a second
	// row for the same step.
	stepRows map[int]id.StepID
}

// continuation snapshots the state for a suspension row. cfg travels
// with it because a resume must not re-merge the agent's config: the
// run's own overrides live nowhere else, and rebuilding without them
// widens the tool list and resets the step budget.
func (s *reactState) continuation(cfg resolvedConfig) suspension.Continuation {
	schema := 0
	publication := safety.Policy{}
	if s.prepared != nil {
		publication = cfg.Publication
		schema = preparedSchemaVersion
		if s.preparedSnapshot != nil {
			schema = s.preparedSnapshot.SchemaVersion
		}
	}
	return suspension.Continuation{
		SchemaVersion: schema, Prepared: preparedBytes(s.prepared), Provenance: s.provenance, Usage: s.usage,
		Messages:        s.messages,
		ToolResults:     s.toolResults,
		SystemPrompt:    s.systemPrompt,
		StepIndex:       s.stepIndex,
		TokensUsed:      s.tokensUsed,
		NewMessagesFrom: s.newMessagesFrom,
		SessionID:       s.sessionID,
		Config: suspension.RunConfig{
			Context:     cfg.Context,
			Publication: publication, Budget: cfg.Budget, AllToolsRestricted: cfg.AllToolsRestricted,
			Model:           cfg.Model,
			Temperature:     cfg.Temperature,
			MaxSteps:        cfg.MaxSteps,
			MaxTokens:       cfg.MaxTokens,
			ReasoningLoop:   cfg.ReasoningLoop,
			Tools:           cfg.Tools,
			ToolsRestricted: cfg.ToolsRestricted,
			PersonaRef:      cfg.PersonaRef,
		},
	}
}

// configFromContinuation is the inverse, and the only config a resumed
// run ever runs on.
func configFromContinuation(c suspension.RunConfig) resolvedConfig {
	return resolvedConfig{
		Context:     c.Context,
		Publication: c.Publication, Budget: c.Budget, AllToolsRestricted: c.AllToolsRestricted,
		Model:           c.Model,
		Temperature:     c.Temperature,
		MaxSteps:        c.MaxSteps,
		MaxTokens:       c.MaxTokens,
		ReasoningLoop:   c.ReasoningLoop,
		Tools:           c.Tools,
		ToolsRestricted: c.ToolsRestricted,
		PersonaRef:      c.PersonaRef,
	}
}

// stateFromContinuation is continuation's inverse, and the only way a
// resumed loop gets its state.
func stateFromContinuation(cont suspension.Continuation) *reactState {
	return &reactState{
		provenance: cont.Provenance, usage: cont.Usage,
		messages:        cont.Messages,
		toolResults:     cont.ToolResults,
		systemPrompt:    cont.SystemPrompt,
		stepIndex:       cont.StepIndex,
		tokensUsed:      cont.TokensUsed,
		newMessagesFrom: cont.NewMessagesFrom,
		sessionID:       cont.SessionID,
	}
}

// runReAct executes an agent using the ReAct reasoning loop synchronously.
func (e *Engine) runReAct(ctx context.Context, ag *agent.Config, input string, overrides *RunOverrides) (*run.Run, error) {
	p, err := e.prepareAgent(ctx, ag, overrides)
	if err != nil {
		return nil, err
	}
	return e.runPrepared(ctx, p, input, preparedOptions(overrides))
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
func (e *Engine) continueReAct(ctx context.Context, ag *agent.Config, cfg resolvedConfig, r *run.Run, st *reactState, startedAt time.Time) (*run.Run, error) {
	if cfg.Publication.Mode == "" {
		cfg.Publication = e.frozenPublication()
	}
	ctx, cancelRun := runDeadline(ctx, cfg)
	defer cancelRun()
	defer releaseLease(ctx, r.ID)
	ctx = withPublicationContext(ctx, ag, r, cfg.Publication)
	ctx = withOperationalRuntime(ctx, r, cfg, st)
	scope := cortex.ScopeFromContext(ctx)

	var finalOutput string
	var lastFinishReason string
	stopReason := run.StopStepLimit

	// Each step runs under its own run agent step span. ctx is pointed at
	// the step's span for the body of the loop and put back afterwards, so
	// the model call and every tool call nest under their step, and the
	// writes after the loop belong to the run. stepSpan is finished
	// explicitly at every exit; the deferred call only catches a path that
	// forgot.
	runCtx := ctx
	var stepSpan *stepTrace
	defer func() {
		if p := recover(); p != nil {
			stepSpan.finish(stepError, telemetry.ErrPanic)
			panic(p)
		}
		stepSpan.finish(stepError, nil)
	}()

	// ReAct loop.
	for st.stepIndex < cfg.MaxSteps {
		ctx = runCtx
		// A leased run stops here, before its next model call, when the
		// engine drains or the run is interrupted.
		if e.handOffDue(ctx, r.ID) {
			fresh := progressCont(r, st, cfg)
			return r, e.leaveRun(ctx, r, &fresh)
		}
		if ctx.Err() != nil {
			stopReason = run.StopCancelled
			break
		}
		if limit := cfg.Budget.Check(st.usage, time.Now()); limit != "" {
			stopReason = run.StopReason(limit)
			break
		}

		if stopped, reflectionErr := e.reflectionBoundary(ctx, r, st); stopped {
			if reflectionErr != nil {
				e.logWarn("reflection boundary", reflectionErr, log.String("run_id", r.ID.String()))
			}
			stopReason = run.StopPolicyUnavailable
			break
		}

		stepStart := time.Now().UTC()
		ctx, stepSpan = e.startStep(runCtx, st.stepIndex)
		ctx = withTaskCallIndex(ctx, st.stepIndex)
		e.extensions.EmitStepStarted(ctx, r.ID, st.stepIndex)

		// Built once per step and threaded to both authorizer calls
		// below (Visible via resolveTools, Authorize via executeTool)
		// so they judge the same subject.
		subject := cortex.Subject{Scope: scope, Principal: cortex.PrincipalFromContext(ctx), AgentID: ag.ID, RunID: r.ID}

		req := &llm.Request{
			Model:       cfg.Model,
			System:      st.systemPrompt,
			Messages:    cloneExecutionMessages(st.messages),
			MaxTokens:   cfg.Budget.OutputLimit(cfg.MaxTokens, st.usage),
			Temperature: cloneTemperature(cfg.Temperature),
			Tools:       e.executionTools(ctx, subject, cfg, st),
		}

		if interruption(ctx, r.ID) != nil {
			stepSpan.finish(stepStopped, nil)
			fresh := progressCont(r, st, cfg)
			return r, e.leaveRun(ctx, r, &fresh)
		}
		if ctx.Err() != nil {
			stopReason = run.StopCancelled
			stepSpan.finish(stepStopped, nil)
			break
		}
		if contextErr := e.admitContextRequest(ctx, cfg.Context, req); contextErr != nil {
			terminalErr := e.stopContextAdmission(ctx, r, st, contextErr)
			stepSpan.finish(stepStopped, nil)
			return r, terminalErr
		}
		resp, err := e.completeTaskCall(ctx, r, req)
		if err != nil && interruption(ctx, r.ID) != nil {
			// The model call ended because the run was interrupted, not
			// because the provider failed: nothing about it is the run's
			// outcome.
			stepSpan.finish(stepStopped, nil)
			fresh := progressCont(r, st, cfg)
			return r, e.leaveRun(ctx, r, &fresh)
		}
		if err != nil {
			err = e.checkedProviderError(ctx, ag, r, cfg.Publication, err)
			reported := 0
			if resp != nil {
				reported = resp.Usage.TotalTokens
			}
			st.failedAttempt(reported, cfg, r)
			e.failRun(ctx, r, ag.ID, err, startedAt)
			stepSpan.finish(stepError, err)
			return nil, fmt.Errorf("llm complete: %w", err)
		}

		if resp == nil {
			st.failedAttempt(0, cfg, r)
			endErr := e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopInvalidResponse, StepCount: st.stepIndex, TokensUsed: st.tokensUsed})
			stepSpan.finish(stepStopped, nil)
			return r, endErr
		}
		st.chargeTokens(resp.Usage.TotalTokens, cfg, r)
		stepSpan.toolCalls = len(resp.ToolCalls)
		if r.Provenance.VerificationRequired && len(st.preparedSnapshot.Tools) == 0 && len(resp.ToolCalls) > 0 {
			st.stepIndex++
			st.usage.Steps = st.stepIndex
			r.Usage = st.usage
			stopReason = run.StopInvalidResponse
			stepSpan.finish(stepStopped, nil)
			break
		}
		responseReason := responseStopReason(resp.FinishReason, resp.Content, len(resp.ToolCalls))
		checked, checkErr := e.checkResponse(ctx, ag, r, cfg.Publication, resp.Content, resp.ToolCalls)
		if checkErr == nil {
			checkedFinish, finishErr := e.checkPublication(ctx, ag, r, cfg.Publication, resp.FinishReason, "provider_finish", safety.DirectionOutput)
			lastFinishReason = string(checkedFinish)
			checkErr = finishErr
		}
		if checkErr != nil {
			st.stepIndex++
			st.usage.Steps = st.stepIndex
			stopErr := e.stopPublication(ctx, r, st, checkErr)
			stepSpan.finish(stepStopped, nil)
			return r, stopErr
		}
		// The provider may retain its response; keep approved content separate.
		responseCopy := *resp
		responseCopy.Content = string(checked)
		responseCopy.ToolCalls = append([]llm.ToolCall(nil), resp.ToolCalls...)
		resp = &responseCopy

		// Record the step.
		stepEnd := time.Now().UTC()
		step := &run.Step{
			Entity:      cortex.NewEntity(),
			ID:          id.NewStepID(),
			RunID:       r.ID,
			Index:       st.stepIndex,
			Type:        "generation",
			Input:       publicGenerationText(r, lastContent(st.messages)),
			Output:      publicGenerationText(r, resp.Content),
			TokensUsed:  resp.Usage.TotalTokens,
			StartedAt:   &stepStart,
			CompletedAt: &stepEnd,
		}
		if existing, reused := st.stepRows[step.Index]; reused {
			step.ID = existing
		} else if err := e.store.CreateStep(ctx, step); err != nil {
			e.logError("create step", err, log.String("run_id", r.ID.String()))
		}

		e.extensions.EmitStepCompleted(ctx, r.ID, st.stepIndex, stepEnd.Sub(stepStart))
		st.stepIndex++
		st.usage.Steps = st.stepIndex
		r.Usage = st.usage

		// Check for tool calls.
		if cfg.Budget.MaxTokens > 0 && st.tokensUsed > cfg.Budget.MaxTokens {
			stopReason = run.StopTokenLimit
			stepSpan.finish(stepStopped, nil)
			break
		}
		if len(resp.ToolCalls) > 0 && responseReason == run.StopFinalAnswer {
			if limit := st.admitTools(len(resp.ToolCalls), cfg); limit != "" {
				stopReason = run.StopReason(limit)
				stepSpan.finish(stepStopped, nil)
				break
			}
			// Append assistant message with tool calls.
			st.messages = append(st.messages, llm.Message{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})
			e.saveBoundary(ctx, r.ID, progressCont(r, st, cfg))

			// Execute each tool call. A call that turns out to be
			// pending is collected rather than recorded: the whole step
			// runs first, and one suspension carrying every pending call
			// is written after it.
			// One reason per step, taken from what executeTool
			// classified rather than named here: nothing downstream,
			// including the persisted row, gets to invent a reason of
			// its own. A step whose calls disagree is refused at
			// suspend rather than folded into one of them.
			var pending pendingCalls
			stepCtx := pending.collecting(ctx, r.ID)
			for _, tc := range resp.ToolCalls {
				// A leased run hands off before each dispatch too: the
				// calls already done are in its progress, the rest are not
				// started.
				if e.handOffDue(ctx, r.ID) {
					stepSpan.finish(stepStopped, nil)
					return r, e.leaveRun(ctx, r, nil)
				}
				tcStart := time.Now().UTC()
				result, outcome, wait := e.executePreparedTool(e.announceCall(stepCtx, r.ID, tc), subject, tc, st)
				if interruption(ctx, r.ID) != nil {
					// A result that arrives after the interruption is
					// discarded, so a call that is not retry-safe stays in
					// Started and is treated as uncertain.
					stepSpan.finish(stepStopped, nil)
					return r, e.leaveRun(ctx, r, nil)
				}
				if outcome == outcomeUncertain {
					terminalErr := e.finalizeUncertainEffect(ctx, r, st)
					stepSpan.finish(stepStopped, cortex.ErrEffectUncertain)
					return r, errors.Join(cortex.ErrEffectUncertain, terminalErr)
				}
				if outcome == outcomePending {
					// No tool call row: nothing ran, and a row with a
					// completion timestamp on it would be a lie the
					// resume has no way to correct (run.Store has no
					// update for tool calls). No terminal plugin event
					// either, and no tool result message: the model must
					// not see an empty result standing in for a call
					// that is still outstanding.
					pending.add(tc, wait.reason, wait.ttl, wait.note)
					continue
				}

				approvedResult, resultErr := e.checkToolResult(ctx, ag, r, cfg.Publication, result, "tool_result")
				if resultErr != nil {
					stopErr := e.stopPublication(ctx, r, st, resultErr)
					stepSpan.finish(stepStopped, nil)
					return r, stopErr
				}
				result = string(approvedResult)
				tcEnd := time.Now().UTC()
				toolCall := &run.ToolCall{
					Entity:      cortex.NewEntity(),
					ID:          id.NewToolCallID(),
					StepID:      step.ID,
					RunID:       r.ID,
					ToolName:    publicToolName(ctx, tc.Name),
					Arguments:   publicGenerationText(r, tc.Arguments),
					Result:      publicGenerationText(r, result),
					Error:       publicToolFailure(ctx, outcome),
					StartedAt:   &tcStart,
					CompletedAt: &tcEnd,
				}
				if err := e.store.CreateToolCall(ctx, toolCall); err != nil {
					e.logError("create tool call", err, log.String("run_id", r.ID.String()))
				}

				// executeTool already emitted the terminal event for a denial
				// or a failure. Emitting completed here too would report the
				// same call twice, with the second one calling it a success.
				if outcome == outcomeCompleted {
					e.emitToolCompleted(ctx, r.ID, tc.Name, result, tcEnd.Sub(tcStart))
				}

				// Append tool result message.
				st.recordToolResult(tc.ID, outcome, verification.ToolLocal)
				st.messages = append(st.messages, llm.Message{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
				e.noteDone(ctx, r.ID, tc.ID, result, "", outcome)
			}

			if pending.any() {
				st.provenance = r.Provenance
				cont := st.continuation(cfg)
				if _, err := e.suspend(ctx, r, pending, cont); err != nil {
					e.failRun(ctx, r, ag.ID, err, startedAt)
					stepSpan.finish(stepError, err)
					return nil, fmt.Errorf("suspend run: %w", err)
				}
				stepSpan.finish(stepSuspended, nil)
				return r, nil
			}
			stepSpan.finish(stepToolCalls, nil)
			continue // Continue the ReAct loop.
		}

		// No tool calls — this is the final response.
		finalOutput = resp.Content
		stopReason = responseReason

		st.messages = append(st.messages, llm.Message{Role: "assistant", Content: finalOutput})
		stepSpan.finish(finalOutcome(responseReason), nil)
		break
	}
	ctx = runCtx
	if interruption(ctx, r.ID) != nil {
		// Interrupted between the last boundary and the end: nothing of
		// this pass is the run's outcome, and the conversation is not
		// saved, because the worker that continues the run saves it.
		return r, e.leaveRun(ctx, r, nil)
	}

	r.Usage = st.usage
	e.saveConversation(ctx, ag.ID, st)

	// Complete the run.
	completedAt := time.Now().UTC()
	if err := e.finalizeRun(ctx, r, run.Outcome{StopReason: stopReason, FinishReason: lastFinishReason, Output: finalOutput, StepCount: st.stepIndex, TokensUsed: st.tokensUsed, CompletedAt: completedAt, Transcript: st.executionTranscript(r)}); err != nil {
		return r, err
	}
	return r, nil
}

// streamReAct executes an agent using the ReAct reasoning loop with streaming.
func (e *Engine) streamReAct(ctx context.Context, ag *agent.Config, input string, overrides *RunOverrides, events chan<- StreamEvent, done func()) error {
	p, err := e.prepareAgent(ctx, ag, overrides)
	if err != nil {
		close(events)
		return err
	}
	return e.streamPrepared(ctx, p, input, preparedOptions(overrides), events, done)
}

// continueStreamReAct is continueReAct's streaming twin: same contract,
// same reason for existing, over the streaming loop instead. It does not
// close events, because the goroutine that calls it owns the channel.
func (e *Engine) continueStreamReAct(ctx context.Context, ag *agent.Config, cfg resolvedConfig, r *run.Run, st *reactState, startedAt time.Time, events chan<- StreamEvent) {
	var taskStream llm.Stream
	defer func() {
		if taskStream != nil {
			_ = taskStream.Close()
		}
	}()
	if cfg.Publication.Mode == "" {
		cfg.Publication = e.frozenPublication()
	}
	ctx, cancelRun := runDeadline(ctx, cfg)
	defer cancelRun()
	defer releaseLease(ctx, r.ID)
	ctx = withPublicationContext(ctx, ag, r, cfg.Publication)
	ctx = withOperationalRuntime(ctx, r, cfg, st)
	scope := cortex.ScopeFromContext(ctx)

	var finalOutput string
	var lastFinishReason string
	stopReason := run.StopStepLimit

	// Same step span discipline as continueReAct: see its comment.
	runCtx := ctx
	var stepSpan *stepTrace
	defer func() {
		if p := recover(); p != nil {
			stepSpan.finish(stepError, telemetry.ErrPanic)
			panic(p)
		}
		stepSpan.finish(stepError, nil)
	}()

	// ReAct loop.
	for st.stepIndex < cfg.MaxSteps {
		ctx = runCtx
		// Same boundary as the synchronous loop.
		if e.handOffDue(ctx, r.ID) {
			fresh := progressCont(r, st, cfg)
			sendLeftEvent(ctx, events, r, e.leaveRun(ctx, r, &fresh))
			return
		}
		if ctx.Err() != nil {
			stopReason = run.StopCancelled
			break
		}
		if limit := cfg.Budget.Check(st.usage, time.Now()); limit != "" {
			stopReason = run.StopReason(limit)
			break
		}

		if stopped, reflectionErr := e.reflectionBoundary(ctx, r, st); stopped {
			if reflectionErr != nil {
				e.logWarn("reflection boundary", reflectionErr, log.String("run_id", r.ID.String()))
			}
			stopReason = run.StopPolicyUnavailable
			break
		}

		stepStart := time.Now().UTC()
		ctx, stepSpan = e.startStep(runCtx, st.stepIndex)
		ctx = withTaskCallIndex(ctx, st.stepIndex)
		e.extensions.EmitStepStarted(ctx, r.ID, st.stepIndex)

		stepID := id.NewStepID()
		if existing, reused := st.stepRows[st.stepIndex]; reused {
			stepID = existing
		}
		if !sendEvent(ctx, events, StreamEvent{Type: EventStep, Data: map[string]any{
			"step_id": stepID.String(), "index": st.stepIndex, "type": "generation",
		}}) {
			e.cancelPublication(ctx, r, st.stepIndex, st.tokensUsed)
			stepSpan.finish(stepStopped, nil)
			return
		}

		// Built once per step and threaded to both authorizer calls
		// below (Visible via resolveTools, Authorize via executeTool)
		// so they judge the same subject.
		subject := cortex.Subject{Scope: scope, Principal: cortex.PrincipalFromContext(ctx), AgentID: ag.ID, RunID: r.ID}

		req := &llm.Request{
			Model:       cfg.Model,
			System:      st.systemPrompt,
			Messages:    cloneExecutionMessages(st.messages),
			MaxTokens:   cfg.Budget.OutputLimit(cfg.MaxTokens, st.usage),
			Temperature: cloneTemperature(cfg.Temperature),
			Tools:       e.executionTools(ctx, subject, cfg, st),
		}

		if interruption(ctx, r.ID) != nil {
			stepSpan.finish(stepStopped, nil)
			fresh := progressCont(r, st, cfg)
			sendLeftEvent(ctx, events, r, e.leaveRun(ctx, r, &fresh))
			return
		}
		if ctx.Err() != nil {
			e.cancelPublication(ctx, r, st.stepIndex, st.tokensUsed)
			stepSpan.finish(stepStopped, nil)
			return
		}
		if contextErr := e.admitContextRequest(ctx, cfg.Context, req); contextErr != nil {
			terminalErr := e.stopContextAdmission(ctx, r, st, contextErr)
			stepSpan.finish(stepStopped, nil)
			sendEvent(ctx, events, runErrorEvent(r, terminalErr))
			return
		}
		stream, err := e.streamTaskCall(ctx, r, req)
		if err != nil && interruption(ctx, r.ID) != nil {
			stepSpan.finish(stepStopped, nil)
			fresh := progressCont(r, st, cfg)
			sendLeftEvent(ctx, events, r, e.leaveRun(ctx, r, &fresh))
			return
		}
		if err != nil {
			err = e.checkedProviderError(ctx, ag, r, cfg.Publication, err)
			st.failedAttempt(0, cfg, r)
			e.failRun(ctx, r, ag.ID, err, startedAt)
			stepSpan.finish(stepError, err)
			sendEvent(ctx, events, runErrorEvent(r, err))
			return
		}

		if stream == nil {
			st.failedAttempt(0, cfg, r)
			if err := e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopInvalidResponse, StepCount: st.stepIndex, TokensUsed: st.tokensUsed}); err != nil {
				sendEvent(ctx, events, runErrorEvent(r, err))
			} else {
				sendEvent(ctx, events, outcomeEvent(r, time.Since(startedAt)))
			}
			stepSpan.finish(stepStopped, nil)
			return
		}

		if r.Provenance.TaskClaimID != "" {
			taskStream = stream
		}
		segments, segmentErr := e.newSegmentPublication(ctx, ag, r, cfg.Publication)
		if segmentErr != nil {
			_ = stream.Close()
			st.failedAttempt(streamReportedTokens(stream), cfg, r)
			terminalErr := e.stopPublication(ctx, r, st, segmentErr)
			stepSpan.finish(stepStopped, nil)
			sendEvent(ctx, events, StreamEvent{Type: EventSafetyBlock, Data: map[string]any{"message": terminalErr.Error()}})
			return
		}
		// Read all chunks from the stream.
		var contentBuf string
		var finishReason string
		var toolCalls []llm.ToolCall
		totalBytes := 0

		for {
			select {
			case <-ctx.Done():
				_ = stream.Close()
				if interruption(ctx, r.ID) != nil {
					stepSpan.finish(stepStopped, nil)
					fresh := progressCont(r, st, cfg)
					sendLeftEvent(ctx, events, r, e.leaveRun(ctx, r, &fresh))
					return
				}
				st.failedAttempt(streamReportedTokens(stream), cfg, r)
				terminalErr := e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopCancelled, StepCount: st.stepIndex, TokensUsed: st.tokensUsed})
				failure := ctx.Err()
				if terminalErr != nil {
					failure = terminalErr
				}
				stepSpan.finish(stepStopped, nil)
				sendEvent(ctx, events, runErrorEvent(r, failure))

				return
			default:
			}

			chunk, err := stream.Next(ctx)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil && interruption(ctx, r.ID) != nil {
				_ = stream.Close()
				stepSpan.finish(stepStopped, nil)
				fresh := progressCont(r, st, cfg)
				sendLeftEvent(ctx, events, r, e.leaveRun(ctx, r, &fresh))
				return
			}
			if err != nil {
				err = e.checkedProviderError(ctx, ag, r, cfg.Publication, err)
				_ = stream.Close()
				st.failedAttempt(streamReportedTokens(stream), cfg, r)
				e.failRun(ctx, r, ag.ID, err, startedAt)
				stepSpan.finish(stepError, err)
				sendEvent(ctx, events, runErrorEvent(r, err))
				return
			}

			if chunk == nil {
				finishReason = "invalid_response"
				break
			}
			if chunk.FinishReason != "" {
				finishReason = chunk.FinishReason
			}

			chunkBytes := len(chunk.Content) + len(chunk.FinishReason) + 1
			for _, delta := range chunk.ToolCalls {
				chunkBytes += len(delta.ID) + len(delta.Name) + len(delta.Arguments) + publicationToolOverhead
			}
			if chunkBytes > cfg.Publication.MaxOutputBytes-totalBytes {
				_ = stream.Close()
				st.failedAttempt(streamReportedTokens(stream), cfg, r)
				terminalErr := e.stopPublication(ctx, r, st, safety.ErrLimit)
				stepSpan.finish(stepStopped, nil)
				sendEvent(ctx, events, StreamEvent{Type: EventSafetyBlock, Data: map[string]any{"message": terminalErr.Error()}})
				return
			}
			totalBytes += chunkBytes
			if chunk.Content != "" {
				contentBuf += chunk.Content
				if segments != nil {
					if checkErr := segments.add(ctx, r, cfg.Publication, chunk.Content, false, events); checkErr != nil {
						_ = stream.Close()
						st.failedAttempt(streamReportedTokens(stream), cfg, r)
						terminalErr := e.stopPublication(ctx, r, st, checkErr)
						stepSpan.finish(stepStopped, nil)
						sendEvent(ctx, events, StreamEvent{Type: EventSafetyBlock, Data: map[string]any{"message": terminalErr.Error()}})
						return
					}
				}
			}

			if len(chunk.ToolCalls) > 0 {
				toolCalls = mergeToolCallDeltas(toolCalls, chunk.ToolCalls)
			}
		}

		rawFinishReason := finishReason
		if finishReason == "" {
			// EOF ends transport, not the model's answer. Retain the raw
			// missing marker while classifying the interrupted response.
			finishReason = "provider_unknown"
			if contentBuf == "" && len(toolCalls) == 0 {
				finishReason = "invalid_response"
			}
		}

		// Collect usage from stream.
		if u := stream.Usage(); u != nil {
			st.chargeTokens(u.TotalTokens, cfg, r)
		} else {
			st.chargeTokens(0, cfg, r)
		}
		_ = stream.Close()
		stepSpan.toolCalls = len(toolCalls)
		if r.Provenance.VerificationRequired && len(st.preparedSnapshot.Tools) == 0 && len(toolCalls) > 0 {
			st.stepIndex++
			st.usage.Steps = st.stepIndex
			r.Usage = st.usage
			stopReason = run.StopInvalidResponse
			stepSpan.finish(stepStopped, nil)
			break
		}
		responseReason := responseStopReason(finishReason, contentBuf, len(toolCalls))
		var checked approvedText
		var checkErr error
		if segments != nil {
			checkErr = segments.add(ctx, r, cfg.Publication, "", true, events)
			if checkErr == nil {
				checkErr = e.checkToolCalls(ctx, ag, r, cfg.Publication, toolCalls)
			}
			checked = approvedText(segments.approved.String())
		} else {
			checked, checkErr = e.checkResponse(ctx, ag, r, cfg.Publication, contentBuf, toolCalls)
		}
		if checkErr == nil {
			checkedFinish, finishErr := e.checkPublication(ctx, ag, r, cfg.Publication, rawFinishReason, "provider_finish", safety.DirectionOutput)
			lastFinishReason = string(checkedFinish)
			checkErr = finishErr
		}
		if checkErr != nil {
			st.stepIndex++
			st.usage.Steps = st.stepIndex
			terminalErr := e.stopPublication(ctx, r, st, checkErr)
			stepSpan.finish(stepStopped, nil)
			sendEvent(ctx, events, StreamEvent{Type: EventSafetyBlock, Data: map[string]any{"message": terminalErr.Error()}})
			return
		}
		contentBuf = string(checked)
		if !r.Provenance.VerificationRequired && segments == nil && !publishToken(ctx, events, checked, 0) {
			e.cancelPublication(ctx, r, st.stepIndex+1, st.tokensUsed)
			stepSpan.finish(stepStopped, nil)
			return
		}

		// Record the step.
		stepEnd := time.Now().UTC()
		step := &run.Step{
			Entity:      cortex.NewEntity(),
			ID:          stepID,
			RunID:       r.ID,
			Index:       st.stepIndex,
			Type:        "generation",
			Input:       publicGenerationText(r, lastContent(st.messages)),
			Output:      publicGenerationText(r, contentBuf),
			StartedAt:   &stepStart,
			CompletedAt: &stepEnd,
		}
		if u := stream.Usage(); u != nil {
			step.TokensUsed = u.TotalTokens
		}
		if existing, reused := st.stepRows[step.Index]; reused {
			step.ID = existing
		} else if err := e.store.CreateStep(ctx, step); err != nil {
			e.logError("create step", err, log.String("run_id", r.ID.String()))
		}

		e.extensions.EmitStepCompleted(ctx, r.ID, st.stepIndex, stepEnd.Sub(stepStart))
		st.stepIndex++
		st.usage.Steps = st.stepIndex
		r.Usage = st.usage

		// Check for tool calls.
		if cfg.Budget.MaxTokens > 0 && st.tokensUsed > cfg.Budget.MaxTokens {
			stopReason = run.StopTokenLimit
			stepSpan.finish(stepStopped, nil)
			break
		}
		if len(toolCalls) > 0 && responseReason == run.StopFinalAnswer {
			if limit := st.admitTools(len(toolCalls), cfg); limit != "" {
				stopReason = run.StopReason(limit)
				stepSpan.finish(stepStopped, nil)
				break
			}
			st.messages = append(st.messages, llm.Message{
				Role:      "assistant",
				Content:   contentBuf,
				ToolCalls: toolCalls,
			})
			e.saveBoundary(ctx, r.ID, progressCont(r, st, cfg))

			// Same collect-then-suspend shape as the synchronous
			// loop: see its comments for why a pending call gets no
			// row, no terminal event and no result message.
			// One reason per step, same as the synchronous loop: the
			// event below reports the reason suspend settled on rather
			// than naming one of its own, and a step whose calls
			// disagree never gets that far.
			var pending pendingCalls
			stepCtx := pending.collecting(ctx, r.ID)
			for _, tc := range toolCalls {
				// Same hand-off boundary as the synchronous loop.
				if e.handOffDue(ctx, r.ID) {
					stepSpan.finish(stepStopped, nil)
					sendLeftEvent(ctx, events, r, e.leaveRun(ctx, r, nil))
					return
				}
				tcStart := time.Now().UTC()
				callCtx := e.announceCall(stepCtx, r.ID, tc, func() {
					sendEvent(ctx, events, StreamEvent{Type: EventToolCall, Data: map[string]any{
						"tool_name": publicToolName(ctx, tc.Name),
						"arguments": publicGenerationText(r, tc.Arguments),
						"tool_id":   publicGenerationText(r, tc.ID),
					}})
				})

				result, outcome, wait := e.executePreparedTool(callCtx, subject, tc, st)
				if interruption(ctx, r.ID) != nil {
					// Discarded, as in the synchronous loop.
					stepSpan.finish(stepStopped, nil)
					sendLeftEvent(ctx, events, r, e.leaveRun(ctx, r, nil))
					return
				}
				if outcome == outcomeUncertain {
					terminalErr := e.finalizeUncertainEffect(ctx, r, st)
					stepSpan.finish(stepStopped, cortex.ErrEffectUncertain)
					if terminalErr != nil || r.State == run.StateCompleted {
						sendEvent(ctx, events, runErrorEvent(r, cortex.ErrEffectUncertain))
					} else {
						sendEvent(ctx, events, outcomeEvent(r, time.Since(startedAt)))
					}
					return
				}
				if outcome == outcomePending {
					pending.add(tc, wait.reason, wait.ttl, wait.note)
					continue
				}

				approvedResult, resultErr := e.checkToolResult(ctx, ag, r, cfg.Publication, result, "tool_result")
				if resultErr != nil {
					terminalErr := e.stopPublication(ctx, r, st, resultErr)
					stepSpan.finish(stepStopped, nil)
					sendEvent(ctx, events, StreamEvent{Type: EventSafetyBlock, Data: map[string]any{"message": terminalErr.Error()}})
					return
				}
				result = string(approvedResult)
				tcEnd := time.Now().UTC()
				toolCall := &run.ToolCall{
					Entity:      cortex.NewEntity(),
					ID:          id.NewToolCallID(),
					StepID:      step.ID,
					RunID:       r.ID,
					ToolName:    publicToolName(ctx, tc.Name),
					Arguments:   publicGenerationText(r, tc.Arguments),
					Result:      publicGenerationText(r, result),
					Error:       publicToolFailure(ctx, outcome),
					StartedAt:   &tcStart,
					CompletedAt: &tcEnd,
				}
				if err := e.store.CreateToolCall(ctx, toolCall); err != nil {
					e.logError("create tool call", err, log.String("run_id", r.ID.String()))
				}

				// Same as the non-streaming loop: one terminal event per
				// call, and executeTool already emitted it for a denial
				// or a failure.
				if outcome == outcomeCompleted {
					e.emitToolCompleted(ctx, r.ID, tc.Name, result, tcEnd.Sub(tcStart))
				}

				st.recordToolResult(tc.ID, outcome, verification.ToolLocal)
				st.messages = append(st.messages, llm.Message{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
				e.noteDone(ctx, r.ID, tc.ID, result, "", outcome)
			}

			if pending.any() {
				st.provenance = r.Provenance
				cont := st.continuation(cfg)
				reason, err := e.suspend(ctx, r, pending, cont)
				if err != nil {
					e.failRun(ctx, r, ag.ID, err, startedAt)
					stepSpan.finish(stepError, err)
					sendEvent(ctx, events, runErrorEvent(r, err))
					return
				}
				// A streaming caller that never learns the run paused
				// would see the channel close after the tool_call
				// event and read it as a run that just stopped. It
				// needs the pending calls too: it is the one that has
				// to execute them.
				stepSpan.finish(stepSuspended, nil)
				sendEvent(ctx, events, StreamEvent{Type: EventSuspended, Data: map[string]any{
					"run_id":  r.ID.String(),
					"reason":  string(reason),
					"pending": publicPendingCalls(r, pending.calls),
				}})
				return
			}
			stepSpan.finish(stepToolCalls, nil)
			continue // Continue the ReAct loop.
		}

		// No tool calls — this is the final response.
		finalOutput = contentBuf
		stopReason = responseReason

		st.messages = append(st.messages, llm.Message{Role: "assistant", Content: finalOutput})
		stepSpan.finish(finalOutcome(responseReason), nil)
		break
	}
	ctx = runCtx
	if interruption(ctx, r.ID) != nil {
		sendLeftEvent(ctx, events, r, e.leaveRun(ctx, r, nil))
		return
	}

	r.Usage = st.usage
	e.saveConversation(ctx, ag.ID, st)

	// Complete the run.
	completedAt := time.Now().UTC()
	if err := e.finalizeRun(ctx, r, run.Outcome{StopReason: stopReason, FinishReason: lastFinishReason, Output: finalOutput, StepCount: st.stepIndex, TokensUsed: st.tokensUsed, CompletedAt: completedAt, Transcript: st.executionTranscript(r)}); err != nil {
		sendEvent(ctx, events, runErrorEvent(r, err))
		return
	}

	sendEvent(ctx, events, outcomeEvent(r, completedAt.Sub(startedAt)))
}

// sendLeftEvent tells a stream's consumer how a pass that left its run
// ended: the run's own outcome when it turned out to have ended already
// (err is nil), otherwise the hand-off or the lost lease.
func sendLeftEvent(ctx context.Context, events chan<- StreamEvent, r *run.Run, err error) {
	if err == nil {
		var duration time.Duration
		if r.StartedAt != nil && r.CompletedAt != nil {
			duration = r.CompletedAt.Sub(*r.StartedAt)
		}
		sendEvent(ctx, events, outcomeEvent(r, duration))
		return
	}
	sendEvent(ctx, events, runErrorEvent(r, err))
}

// ──────────────────────────────────────────────────
// Helper functions
// ──────────────────────────────────────────────────

// saveConversation persists only the messages this run actually added,
// not the whole reloaded history alongside them. See newMessagesFrom's
// comment on reactState for why that distinction matters.
//
// A state with no session is skipped rather than written. Nothing in this
// version produces one, since both loops resolve a session before the run
// starts and a continuation carries it across a pause, but an empty
// session id is the sentinel that meant "the shared one" before scope
// existed, and writing conversation rows under it is how history leaked
// between tenants. A future writer that forgets to carry the session
// should lose a save and say so, not quietly write into the shared bucket.
func (e *Engine) saveConversation(ctx context.Context, agentID id.AgentID, st *reactState) {
	if st.preparedSnapshot != nil && st.preparedSnapshot.Config.Verification != nil {
		return
	}
	if st.sessionID.IsNil() {
		e.logWarn("skipping conversation save: the run carries no session", nil,
			log.String("agent_id", agentID.String()))
		return
	}
	convMsgs := llmToMemory(st.messages[st.newMessagesFrom:])
	if err := e.recordConversationSources(ctx, st, convMsgs); err != nil {
		e.logError("record conversation sources", err, log.String("agent_id", agentID.String()))
		return
	}
	if err := e.store.SaveConversation(ctx, agentID, st.sessionID, convMsgs); err != nil {
		e.logError("save conversation", err, log.String("agent_id", agentID.String()))
	}
}

// failRun marks a run as failed and emits the RunFailed hook. The store
// write is best effort here: the loop's callers have nothing to do with
// the failure but log it. A caller with cleanup waiting on the state
// actually landing calls persistFailure instead.
func (e *Engine) failRun(ctx context.Context, r *run.Run, agentID id.AgentID, runErr error, _ time.Time) {
	e.reflectionErrorBoundary(ctx, r)
	if err := e.persistFailure(ctx, r, agentID, runErr); err != nil {
		e.logError("update run on failure", err, log.String("run_id", r.ID.String()))
	}
}

// persistFailure is failRun with the store error handed back, for the one
// caller that has to know whether the run really moved: a rejected
// checkpoint drops the suspension afterwards, and dropping it while the
// run is still paused would leave a run nothing can ever resume.
//
// The RunFailed hook fires after persistence, so observers never treat an
// uncommitted failure as a durable outcome.
func (e *Engine) persistFailure(ctx context.Context, r *run.Run, agentID id.AgentID, runErr error) error {
	if r.Provenance.VerificationRequired {
		runErr = closedRequiredError(runErr)
	}
	// Same lease check as finalizeRun: a run this worker no longer owns,
	// or one it was told to hand off, is not failed here.
	if leave, err := e.settleLease(ctx, r); leave {
		return err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	completedAt := time.Now().UTC()
	// A dashboard cancellation persists before it stops the provider stream.
	// Next can then return an error, but that error must not replace the
	// operator's terminal cancellation or emit a misleading failure hook.
	if saved, err := e.store.GetRun(writeCtx, r.ID); err == nil && saved.State == run.StateCancelled {
		r.State = run.StateCancelled
		r.StopReason = run.StopCancelled
		r.Error = saved.Error
		r.CompletedAt = &completedAt
		return e.persistRun(writeCtx, r)
	}
	r.State = run.StateFailed
	r.StopReason = run.StopExecutionError
	if ctx.Err() != nil {
		r.State = run.StateCancelled
		r.StopReason = run.StopCancelled
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			r.State = run.StateIncomplete
			r.StopReason = run.StopDeadline
		}
	}
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
	writeErr := e.persistRun(writeCtx, r)
	// Same reasoning as the store write above: a hook subscriber (audit,
	// for instance) that does its own I/O keyed on ctx would otherwise
	// silently drop the failure event on an already-cancelled ctx, even
	// though the store now correctly recorded it.
	if writeErr != nil {
		return fmt.Errorf("update run on failure: %w", writeErr)
	}
	e.emitTerminalOutcome(writeCtx, r)
	if r.State == run.StateFailed {
		e.extensions.EmitRunFailed(writeCtx, agentID, r.ID, runErr)
	}
	return nil
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
// resolveTools picks the tools a run may call. An empty names list means
// every registered tool, UNLESS restricted is set, which says the list
// is an explicit allowlist that happens to be empty. That distinction
// exists because an overlay can withdraw the last tool an agent had, and
// without it a removal that emptied the list would read as a grant of
// everything registered.
func (e *Engine) resolveTools(ctx context.Context, s cortex.Subject, names []string, restricted bool) []llm.Tool {
	tools := e.toolDefinitions(names, restricted)
	if e.authorizer != nil {
		tools = e.authorizer.Visible(ctx, s, tools)
	}
	return tools
}

func (e *Engine) toolDefinitions(names []string, restricted bool) []llm.Tool {
	registered := make([]llm.Tool, 0, len(e.tools)+len(e.externalTools))
	for _, rt := range e.tools {
		registered = append(registered, rt.def)
	}
	registered = append(registered, e.externalToolDefs()...)
	if restricted || len(names) > 0 {
		registered = filterByName(registered, names)
	}

	return append(e.builtinTools(), registered...)
}

// registeredToolNames is every tool an agent could name, used to
// materialize the implicit "all of them" list before an overlay's
// removals are applied. Without materializing, a removal against an
// agent that never named its tools would subtract from an empty list
// and silently do nothing.
func (e *Engine) registeredToolNames() []string {
	out := make([]string, 0, len(e.tools)+len(e.externalTools))
	for _, rt := range e.tools {
		out = append(out, rt.def.Name)
	}
	for _, ext := range e.externalTools {
		out = append(out, ext.def.Name)
	}

	return out
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
	// outcomeUncertain stops the run before another effect or model step.
	outcomeUncertain
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
func (e *Engine) executeTool(ctx context.Context, s cortex.Subject, tc llm.ToolCall) (string, toolOutcome, pendingWait) {
	if e.authorizer != nil {
		if err := e.authorizer.Authorize(ctx, s, tc); err != nil {
			// ErrRequiresApproval is the authorizer's third answer and it
			// is not a denial: it says a human has to decide first. So
			// the call is classified pending, exactly like an external
			// one, and nothing here suspends anything. The loop collects
			// it with the rest of the step and suspends once, with
			// approval as the reason. Nothing goes back to the model
			// either: a denial's error text would tell it the call was
			// refused when it is only waiting.
			announced(ctx)
			if errors.Is(err, cortex.ErrRequiresApproval) {
				return e.approvalWait(ctx, s, tc, err)
			}
			e.emitToolDenied(ctx, s.RunID, tc.Name, e.checkedToolError(ctx, s, err).Error())
			return jsonResult("error", err.Error()), outcomeDenied, pendingWait{}
		}
	}

	result, outcome, failErr := e.dispatchTool(ctx, s, tc)
	// An interrupted run reports nothing about the call: it stays started,
	// and the worker that continues the run settles it.
	if (outcome == outcomeFailed || outcome == outcomeUncertain) && interruption(ctx, s.RunID) == nil {
		announced(ctx)
		noteToolFailure(ctx, failErr)
		e.emitToolFailed(ctx, s.RunID, tc.Name, e.checkedToolError(ctx, s, failErr))
	}
	if outcome == outcomePending {
		wait := pendingWait{reason: pendingReason(tc.Name)}
		if wait.reason == suspension.ReasonExternalTool {
			wait.ttl = e.externalTTL(tc.Name)
		}
		return result, outcome, wait
	}
	return result, outcome, pendingWait{}
}

// approvalWait classifies a call the authorizer escalated. The sentinel
// alone gives no TTL and no note; ApprovalRequired, found anywhere in the
// chain, gives both.
//
// A negative TTL is a denial, not a pause. It reads as a deadline that
// has already passed, and pausing on it would open a checkpoint nobody
// can answer before the sweeper fails the run. Refusing the call instead
// tells the model, on this step, and fires ToolDenied for the operator.
func (e *Engine) approvalWait(ctx context.Context, s cortex.Subject, tc llm.ToolCall, err error) (string, toolOutcome, pendingWait) {
	var approval cortex.ApprovalRequired
	errors.As(err, &approval)
	if approval.TTL < 0 {
		denial := fmt.Errorf("approval TTL is negative (%s), so the call is denied rather than paused past its own deadline", approval.TTL)
		e.emitToolDenied(ctx, s.RunID, tc.Name, e.checkedToolError(ctx, s, denial).Error())
		return jsonResult("error", denial.Error()), outcomeDenied, pendingWait{}
	}
	return "", outcomePending, pendingWait{reason: suspension.ReasonApproval, ttl: approval.TTL, note: approval.Reason}
}

// pendingReason says what a pending call is waiting on. External tools
// wait on the caller; agent_ask waits on a peer agent, and telling a host
// to go execute that one would be asking it to forge the peer's reply.
func pendingReason(toolName string) suspension.SuspendReason {
	if toolName == toolAgentAsk {
		return suspension.ReasonAgentReply
	}
	return suspension.ReasonExternalTool
}

// dispatchTool runs a call that has already cleared authorization, and it
// is where the actual tool lives: builtin, registered, or external and
// therefore not runnable here at all.
//
// It emits no plugin event and returns the failure error instead, because
// it has two callers with different books to close. executeTool reports a
// failure as it happens; a resume dispatching a call a human just
// approved reports it with the timings of the whole pause, and two
// emitters for one call is how a subscriber ends up counting it twice.
func (e *Engine) dispatchAuthorizedTool(ctx context.Context, s cortex.Subject, tc llm.ToolCall) (string, toolOutcome, error) {
	if err := ctx.Err(); err != nil {
		return jsonResult("error", err.Error()), outcomeFailed, err
	}
	inv := cortex.Invocation{Subject: s, Call: tc}

	// Progress point 3: a call this engine runs, and that is not
	// retry-safe, is recorded as started before its handler gets it. A
	// lost lease stops it here, undispatched.
	if isBuiltinTool(tc.Name) || e.hasRegisteredTool(tc.Name) {
		if err := e.noteStarted(ctx, s.RunID, tc); err != nil {
			return jsonResult("error", err.Error()), outcomeFailed, err
		}
		announced(ctx)
	}

	if result, outcome, handled := e.executeBuiltinTool(ctx, inv); handled {
		if outcome == outcomeFailed {
			return result, outcome, fmt.Errorf("builtin tool %q failed", tc.Name)
		}
		return result, outcome, nil
	}
	for _, rt := range e.tools {
		if rt.def.Name == tc.Name {
			out, err := rt.handler(ctx, inv)
			if errors.Is(err, cortex.ErrEffectUncertain) {
				return "", outcomeUncertain, cortex.ErrEffectUncertain
			}
			if err != nil {
				// A run the handler started was handed off or lost its
				// lease: that interrupts this run, it is not the call's
				// result.
				interruptedByTool(ctx, s.RunID, err)
				return jsonResult("error", err.Error()), outcomeFailed, err
			}
			return out, outcomeCompleted, nil
		}
	}

	// External tools are matched after the registered ones so a host that
	// registers both under one name keeps the executable registration,
	// same as the first-match-wins rule WithTool already documents.
	// Nothing runs here: the call has not completed, failed or been
	// denied, it is waiting on the caller.
	for _, ext := range e.externalTools {
		if ext.def.Name == tc.Name {
			return "", outcomePending, nil
		}
	}

	// A name that matches nothing is a failure like any other, and it gets
	// the same terminal event from whoever called us. Reporting it as
	// completed would put a tool that never ran into a subscriber's
	// success count.
	err := fmt.Errorf("unknown tool %q", tc.Name)
	return jsonResult("error", err.Error()), outcomeFailed, err
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

func (s *reactState) recordToolResult(callID string, outcome toolOutcome, source verification.ToolSource) {
	if !s.provenance.VerificationRequired {
		return
	}
	var status verification.ToolOutcome
	switch outcome {
	case outcomeCompleted:
		status = verification.ToolCompleted
	case outcomeFailed:
		status = verification.ToolFailed
	case outcomeDenied:
		status = verification.ToolDenied
	default:
		return
	}
	s.toolResults = append(s.toolResults, verification.ToolResult{CallID: callID, Outcome: status, Source: source})
}

func (s *reactState) executionTranscript(r *run.Run) *verification.ExecutionTranscript {
	if !r.Provenance.VerificationRequired || s.newMessagesFrom < 0 || s.newMessagesFrom >= len(s.messages) {
		return nil
	}
	return &verification.ExecutionTranscript{SchemaVersion: 1, RunID: r.ID, InputsDigest: r.Provenance.ExecutionInputsDigest, Messages: cloneExecutionMessages(s.messages[s.newMessagesFrom+1:]), Tools: append([]verification.ToolResult(nil), s.toolResults...), Usage: s.usage}
}
