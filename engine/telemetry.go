package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/suspension"

	gometrics "github.com/xraph/go-utils/metrics"
)

// Span names. Each is a verb and an object, with no identifier in it: the
// IDs ride on attributes, where a backend can index them.
const (
	spanRunAgent         = "run agent"
	spanPrepareAgent     = "prepare agent"
	spanRunStep          = "run agent step"
	spanCallTool         = "call tool"
	spanCheckPublication = "check publication"
	spanSuspendRun       = "suspend run"
	spanExpireSuspension = "expire suspension"
	spanRunOrchestration = "run orchestration"
)

// Span attributes. The gen_ai ones follow the OpenTelemetry GenAI
// conventions; everything cortex-specific is namespaced cortex.*. None of
// them carry text a user or a model wrote: no input, output, prompt, tool
// argument or result, no agent, persona or skill name, no scope value.
const (
	attrGenAIOperation      = "gen_ai.operation.name"
	attrGenAIAgentID        = "gen_ai.agent.id"
	attrGenAIConversationID = "gen_ai.conversation.id"
	attrGenAIRequestModel   = "gen_ai.request.model"
	attrGenAIToolName       = "gen_ai.tool.name"
	attrGenAIToolCallID     = "gen_ai.tool.call.id"

	attrRunID           = "cortex.run.id"
	attrRunMode         = "cortex.run.mode"
	attrRunResumed      = "cortex.run.resumed"
	attrRunResumeSource = "cortex.run.resume_source"
	attrRunPurpose      = "cortex.run.purpose"
	attrRunState        = "cortex.run.state"
	attrRunStopReason   = "cortex.run.stop_reason"
	attrRunSteps        = "cortex.run.steps"
	attrRunTokens       = "cortex.run.tokens"
	attrRunResult       = "cortex.run.result"

	attrPrepareAtomic    = "cortex.prepare.atomic"
	attrPrepareSections  = "cortex.prepare.sections"
	attrPrepareKnowledge = "cortex.prepare.knowledge_sections"
	attrPrepareTools     = "cortex.prepare.tools"

	attrStepIndex     = "cortex.step.index"
	attrStepToolCalls = "cortex.step.tool_calls"
	attrStepOutcome   = "cortex.step.outcome"

	attrToolKind    = "cortex.tool.kind"
	attrToolOutcome = "cortex.tool.outcome"

	attrPublicationSource    = "cortex.publication.source"
	attrPublicationDirection = "cortex.publication.direction"
	attrPublicationMode      = "cortex.publication.mode"
	attrPublicationStatus    = "cortex.publication.status"
	attrPublicationChecks    = "cortex.publication.checks"

	attrSuspensionReason  = "cortex.suspension.reason"
	attrSuspensionPending = "cortex.suspension.pending_calls"
	attrSuspensionOutcome = "cortex.suspension.outcome"

	attrOrchestrationID       = "cortex.orchestration.id"
	attrOrchestrationStrategy = "cortex.orchestration.strategy"
	attrOrchestrationRuns     = "cortex.orchestration.runs"
	attrOrchestrationStatus   = "cortex.orchestration.status"
)

// Metric names. A Forge app scrapes each with a forge_ prefix, so
// cortex_runs_finished_total reads as forge_cortex_runs_finished_total.
const (
	metricRunsFinished           = "cortex_runs_finished_total"
	metricRunDuration            = "cortex_run_duration_seconds"
	metricRunsInProgress         = "cortex_runs_in_progress"
	metricStepsExecuted          = "cortex_steps_executed_total"
	metricToolCallsHandled       = "cortex_tool_calls_handled_total"
	metricToolCallDuration       = "cortex_tool_call_duration_seconds"
	metricPublicationChecks      = "cortex_publication_checks_completed_total"
	metricRunsSuspended          = "cortex_runs_suspended_total"
	metricSuspensionsExpired     = "cortex_suspensions_expired_total"
	metricOrchestrationsFinished = "cortex_orchestrations_finished_total"
)

var (
	// runDurationBuckets span one pass of the loop, from an echo that
	// answers in a few milliseconds to a long tool-using run.
	runDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600}
	// toolDurationBuckets span a local lookup to a slow host tool.
	toolDurationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}
)

// Bounded label and attribute values.
const (
	modeSync   = "sync"
	modeStream = "stream"

	stepToolCalls = "tool_calls"
	stepFinal     = "final"
	stepSuspended = "suspended"
	stepStopped   = "stopped"
	stepError     = "error"

	toolKindBuiltin    = "builtin"
	toolKindRegistered = "registered"
	toolKindExternal   = "external"
	toolKindUnknown    = "unknown"

	labelOther   = "other"
	labelUnknown = "unknown"
	resultOK     = "ok"
	resultError  = "error"
)

// engineMetrics holds every instrument the engine records. Each vec is nil
// when the engine has no metrics sink, and a nil vec records nothing, so
// the call sites never check.
type engineMetrics struct {
	runsFinished   *telemetry.CounterVec
	runDuration    *telemetry.HistogramVec
	runsInProgress *telemetry.GaugeVec
	steps          *telemetry.CounterVec
	toolCalls      *telemetry.CounterVec
	toolDuration   *telemetry.HistogramVec
	publication    *telemetry.CounterVec
	suspended      *telemetry.CounterVec
	expired        *telemetry.CounterVec
	orchestrations *telemetry.CounterVec

	// inFlight is the real count behind runsInProgress, one per mode. The
	// gauge is set from it under the lock, so two passes that start and
	// end at once cannot leave it showing a stale value.
	mu       sync.Mutex
	inFlight map[string]int64
}

func newEngineMetrics(m gometrics.Metrics) *engineMetrics {
	return &engineMetrics{
		runsFinished: telemetry.NewCounterVec(m, metricRunsFinished,
			"Agent runs that reached a terminal state, by state, stop reason and purpose.",
			"state", "stop_reason", "purpose"),
		runDuration: telemetry.NewHistogramVec(m, metricRunDuration,
			"Wall time of one pass of the agent loop, from the call that started or resumed the run to its end or its pause.",
			"s", runDurationBuckets, "state", "purpose"),
		runsInProgress: telemetry.NewGaugeVec(m, metricRunsInProgress,
			"Passes of the agent loop running in this process right now, by mode.", "mode"),
		steps: telemetry.NewCounterVec(m, metricStepsExecuted,
			"Steps of the agent loop (one model call each), by how the step ended.", "outcome"),
		toolCalls: telemetry.NewCounterVec(m, metricToolCallsHandled,
			"Tool calls the engine handled, by tool kind and outcome.", "kind", "outcome"),
		toolDuration: telemetry.NewHistogramVec(m, metricToolCallDuration,
			"Wall time of one tool call that ran, by tool kind and outcome. Pending calls are not observed.",
			"s", toolDurationBuckets, "kind", "outcome"),
		publication: telemetry.NewCounterVec(m, metricPublicationChecks,
			"Publication checks, by source, direction and result.", "source", "direction", "result"),
		suspended: telemetry.NewCounterVec(m, metricRunsSuspended,
			"Attempts to pause a run on pending tool calls, by reason and result.", "reason", "result"),
		expired: telemetry.NewCounterVec(m, metricSuspensionsExpired,
			"Paused runs the expiry sweep failed because nobody answered in time, by reason and result.", "reason", "result"),
		orchestrations: telemetry.NewCounterVec(m, metricOrchestrationsFinished,
			"Orchestration runs that finished, by strategy and result.", "strategy", "result"),
		inFlight: map[string]int64{},
	}
}

func (m *engineMetrics) runFinished(r *run.Run) {
	if m == nil || r == nil {
		return
	}
	m.runsFinished.Inc(runStateLabel(r.State), stopReasonLabel(r.StopReason), purposeLabel(r.Provenance.Purpose))
}

func (m *engineMetrics) trackInFlight(mode string, delta int64) {
	if m == nil || m.runsInProgress == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight[mode] += delta
	m.runsInProgress.Set(float64(m.inFlight[mode]), mode)
}

func (m *engineMetrics) stepExecuted(outcome string) {
	if m == nil {
		return
	}
	m.steps.Inc(outcome)
}

func (m *engineMetrics) toolHandled(kind, outcome string, ran bool, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.toolCalls.Inc(kind, outcome)
	if ran {
		m.toolDuration.Observe(elapsed.Seconds(), kind, outcome)
	}
}

func (m *engineMetrics) publicationChecked(source string, direction safety.Direction, result string) {
	if m == nil {
		return
	}
	m.publication.Inc(publicationSourceLabel(source), directionLabel(direction), result)
}

// ──────────────────────────────────────────────────
// Label normalisers. Every one maps anything it does not know to a fixed
// value, so a stored row from a newer version, or a future enum value,
// can never mint a new series.
// ──────────────────────────────────────────────────

func runStateLabel(s run.State) string {
	switch s {
	case run.StateCompleted, run.StateIncomplete, run.StateFailed, run.StateCancelled, run.StatePaused:
		return string(s)
	default:
		return labelOther
	}
}

func stopReasonLabel(s run.StopReason) string {
	switch s {
	case run.StopExecutionError, run.StopInvalidResponse, run.StopProviderUnknown, run.StopFinalAnswer,
		run.StopStepLimit, run.StopToolLimit, run.StopTokenLimit, run.StopCostLimit, run.StopDeadline,
		run.StopCancelled, run.StopSafetyBlock, run.StopPolicyUnavailable, run.StopToolError,
		run.StopPersistenceError, run.StopVerificationFailed:
		return string(s)
	default:
		return labelUnknown
	}
}

func purposeLabel(p run.ExecutionPurpose) string {
	switch p {
	case "", run.PurposeNormal:
		return string(run.PurposeNormal)
	case run.PurposeEvaluation, run.PurposeOptimizationTrial, run.PurposeOptimizationController:
		return string(p)
	default:
		return labelUnknown
	}
}

func suspendReasonLabel(r suspension.SuspendReason) string {
	switch r {
	case suspension.ReasonApproval, suspension.ReasonExternalTool, suspension.ReasonAgentReply:
		return string(r)
	default:
		return labelUnknown
	}
}

func publicationSourceLabel(s string) string {
	switch s {
	case "user_input", "history", "retrieval", "model_output", "model_output_segment", "tool_call",
		"provider_finish", "provider_error", "tool_result", "tool_error":
		return s
	default:
		return labelOther
	}
}

func directionLabel(d safety.Direction) string {
	switch d {
	case safety.DirectionInput, safety.DirectionOutput:
		return string(d)
	default:
		return labelUnknown
	}
}

func strategyLabel(s string) string {
	switch s {
	case orchestration.StrategySequential, orchestration.StrategyParallel, orchestration.StrategyRouter,
		orchestration.StrategyHierarchical, orchestration.StrategyDebate:
		return s
	default:
		return labelOther
	}
}

func resumeSourceLabel(s resumeSource) string {
	switch s {
	case resumeSourceApproval:
		return "approval"
	case resumeSourceAgentReply:
		return "agent_reply"
	default:
		return "public"
	}
}

func toolOutcomeLabel(o toolOutcome) string {
	switch o {
	case outcomeCompleted:
		return "completed"
	case outcomeFailed:
		return "failed"
	case outcomeDenied:
		return "denied"
	case outcomePending:
		return "pending"
	case outcomeUncertain:
		return "uncertain"
	default:
		return labelUnknown
	}
}

// publicationResult is the result label of one publication check: the
// gate's status when it passed, the rejection class when it did not.
func publicationResult(status safety.PublicationStatus, err error) string {
	if err != nil {
		return publicationErrorLabel(err)
	}
	switch status {
	case safety.StatusApproved, safety.StatusRedacted, safety.StatusFlagged, safety.StatusUnscanned:
		return string(status)
	default:
		return labelUnknown
	}
}

func publicationErrorLabel(err error) string {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.Is(err, safety.ErrBlocked):
		return "blocked"
	case errors.Is(err, safety.ErrLimit):
		return "limit"
	default:
		return "unavailable"
	}
}

// ──────────────────────────────────────────────────
// Run spans
// ──────────────────────────────────────────────────

type runTraceKey struct{}

// runTrace is one pass of the loop: a fresh run from start to its end or
// pause, or a resumed run from the claim to its end or next pause. It owns
// the run agent span, the in-flight gauge and the duration observation, and
// it learns which run it is about once the run row exists.
type runTrace struct {
	instr *engineMetrics
	span  trace.Span
	mode  string
	start time.Time

	mu    sync.Mutex
	run   *run.Run
	ended bool
}

// DetachTrace returns ctx for a run that outlives the request or start hook
// that launched it. The run agent span started from it is a new root that
// links to the span ctx carried, rather than a child that keeps running
// after its parent has ended. Values and cancellation are kept.
//
// The dashboard's live runs use it: they go on for minutes after the
// command that started them has returned.
func DetachTrace(ctx context.Context) context.Context { return telemetry.Detach(ctx) }

func (e *Engine) startRun(ctx context.Context, mode string, resumed bool, extra ...attribute.KeyValue) (context.Context, *runTrace) {
	attrs := append([]attribute.KeyValue{
		attribute.String(attrGenAIOperation, "invoke_agent"),
		attribute.String(attrRunMode, mode),
		attribute.Bool(attrRunResumed, resumed),
	}, extra...)
	opts := []trace.SpanStartOption{trace.WithAttributes(attrs...)}
	ctx, link, detached := telemetry.TakeLink(ctx)
	if detached {
		opts = append(opts, trace.WithNewRoot(), trace.WithLinks(trace.Link{SpanContext: link}))
	}
	ctx, span := telemetry.Tracer().Start(ctx, spanRunAgent, opts...)
	rt := &runTrace{instr: e.instr, span: span, mode: mode, start: time.Now()}
	rt.instr.trackInFlight(mode, 1)
	return context.WithValue(ctx, runTraceKey{}, rt), rt
}

// runTraceFrom returns the pass ctx belongs to, or nil, and a nil
// *runTrace ignores every call.
func runTraceFrom(ctx context.Context) *runTrace {
	rt, ok := ctx.Value(runTraceKey{}).(*runTrace)
	if !ok {
		return nil
	}
	return rt
}

// bind records the run this pass is about, as soon as its row exists. The
// pointer is kept rather than copied: the loop writes the terminal state
// into the same struct, so end reads how the run really finished even when
// the call returns a nil run alongside its error.
func (rt *runTrace) bind(r *run.Run, model string) {
	if rt == nil || r == nil {
		return
	}
	rt.mu.Lock()
	rt.run = r
	rt.mu.Unlock()
	attrs := []attribute.KeyValue{
		attribute.String(attrRunID, r.ID.String()),
		attribute.String(attrGenAIAgentID, r.AgentID.String()),
		attribute.String(attrRunPurpose, purposeLabel(r.Provenance.Purpose)),
	}
	if !r.SessionID.IsNil() {
		attrs = append(attrs, attribute.String(attrGenAIConversationID, r.SessionID.String()))
	}
	if model != "" {
		attrs = append(attrs, attribute.String(attrGenAIRequestModel, model))
	}
	rt.span.SetAttributes(attrs...)
}

// end closes the pass. It is safe to call more than once: a streaming
// entry ends it on a synchronous failure and its goroutine on the way out,
// and only the first call counts.
//
// The span is an error only when the run failed, or when the call failed
// before there was a run to speak for it. A run that ended incomplete,
// cancelled or blocked by publication policy, or that paused, did what it
// was told to; its state and stop reason say why.
func (rt *runTrace) end(r *run.Run, err error) {
	if rt == nil {
		return
	}
	rt.mu.Lock()
	if rt.ended {
		rt.mu.Unlock()
		return
	}
	rt.ended = true
	if rt.run == nil {
		rt.run = r
	}
	bound := rt.run
	rt.mu.Unlock()

	if bound != nil {
		attrs := []attribute.KeyValue{
			attribute.String(attrRunState, runStateLabel(bound.State)),
			attribute.Int(attrRunSteps, bound.StepCount),
			attribute.Int(attrRunTokens, bound.TokensUsed),
		}
		if isTerminalRunState(bound.State) {
			attrs = append(attrs, attribute.String(attrRunStopReason, stopReasonLabel(bound.StopReason)))
		}
		rt.span.SetAttributes(attrs...)
		if rt.instr != nil {
			rt.instr.runDuration.Observe(time.Since(rt.start).Seconds(), runStateLabel(bound.State), purposeLabel(bound.Provenance.Purpose))
		}
	}
	switch {
	case errors.Is(err, telemetry.ErrPanic):
		telemetry.Fail(rt.span, err)
	case bound != nil && bound.State == run.StateFailed:
		if err == nil {
			err = errRunFailed
		}
		telemetry.Fail(rt.span, err)
	case bound != nil && (isTerminalRunState(bound.State) || bound.State == run.StatePaused):
	case err == nil, errors.Is(err, context.Canceled):
	default:
		if result, benign := benignRunError(err); benign {
			rt.span.SetAttributes(attribute.String(attrRunResult, result))
			break
		}
		telemetry.Fail(rt.span, err)
	}
	rt.instr.trackInFlight(rt.mode, -1)
	rt.span.End()
}

// benignRunError says whether a call that failed before it had a run to
// speak for did so for an ordinary reason: the agent or run it named does
// not exist, or somebody else claimed the paused run first. Those are
// answers, not faults, so the span carries cortex.run.result instead of an
// error status.
func benignRunError(err error) (string, bool) {
	switch {
	case errors.Is(err, cortex.ErrAgentNotFound), errors.Is(err, cortex.ErrRunNotFound):
		return "not_found", true
	case errors.Is(err, cortex.ErrNotSuspended):
		return "claim_lost", true
	default:
		return "", false
	}
}

// errRunFailed stands in for the error of a run whose loop recorded the
// failure on the row and returned nothing.
var errRunFailed = errors.New("cortex: run failed")

// ──────────────────────────────────────────────────
// Preparation spans
// ──────────────────────────────────────────────────

func (e *Engine) startPrepare(ctx context.Context, agentID string) (context.Context, trace.Span) {
	attrs := []attribute.KeyValue{attribute.Bool(attrPrepareAtomic, e.atomicPreparation)}
	if agentID != "" {
		attrs = append(attrs, attribute.String(attrGenAIAgentID, agentID))
	}
	return telemetry.Tracer().Start(ctx, spanPrepareAgent, trace.WithAttributes(attrs...))
}

func endPrepare(span trace.Span, err error) {
	if err != nil {
		telemetry.Fail(span, err)
	}
	span.End()
}

// annotatePrepared puts the frozen snapshot's counts on the prepare span.
// It is only ever called under one, from the two places a snapshot is
// frozen.
func annotatePrepared(ctx context.Context, snap *preparedSnapshot) {
	if snap == nil {
		return
	}
	knowledge := 0
	for _, s := range snap.Sections {
		if s.Source == prompt.SourceKnowledge {
			knowledge++
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.Int(attrPrepareSections, len(snap.Sections)),
		attribute.Int(attrPrepareKnowledge, knowledge),
		attribute.Int(attrPrepareTools, len(snap.Tools)),
	)
}

// ──────────────────────────────────────────────────
// Step spans
// ──────────────────────────────────────────────────

// stepTrace is one step of the loop: one model call and the tool calls it
// asked for. The model provider's own span (nexus records one per call)
// nests under it, and so does every call tool span.
type stepTrace struct {
	instr     *engineMetrics
	span      trace.Span
	toolCalls int
	done      bool
}

func (e *Engine) startStep(ctx context.Context, index int) (context.Context, *stepTrace) {
	ctx, span := telemetry.Tracer().Start(ctx, spanRunStep, trace.WithAttributes(attribute.Int(attrStepIndex, index)))
	return ctx, &stepTrace{instr: e.instr, span: span}
}

// finish ends the step once; a nil or finished step ignores it, which is
// what lets the loop keep a deferred backstop next to its explicit calls.
//
// A step the caller cancelled, or that ran out its deadline, is stopped
// and not failed, wherever the loop noticed: the provider hands the
// context's error back through the same path as a real failure.
func (s *stepTrace) finish(outcome string, err error) {
	if s == nil || s.done {
		return
	}
	if outcome == stepError && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		outcome = stepStopped
	}
	s.done = true
	s.span.SetAttributes(attribute.String(attrStepOutcome, outcome), attribute.Int(attrStepToolCalls, s.toolCalls))
	if outcome == stepError && err != nil {
		telemetry.Fail(s.span, err)
	}
	s.instr.stepExecuted(outcome)
	s.span.End()
}

// finalOutcome is the step outcome of a response that ended the loop.
func finalOutcome(reason run.StopReason) string {
	if reason == run.StopFinalAnswer {
		return stepFinal
	}
	return stepStopped
}

// ──────────────────────────────────────────────────
// Tool spans
// ──────────────────────────────────────────────────

// toolKind classifies a call the way dispatchTool will resolve it, in the
// same order: builtin, then host-registered, then external, else unknown.
func (e *Engine) toolKind(name string) string {
	switch name {
	case "knowledge_search":
		return toolKindBuiltin
	case toolAgentSend, toolAgentAsk, toolAgentInbox:
		if e.a2a != nil {
			return toolKindBuiltin
		}
	}
	for _, rt := range e.tools {
		if rt.def.Name == name {
			return toolKindRegistered
		}
	}
	for _, ext := range e.externalTools {
		if ext.def.Name == name {
			return toolKindExternal
		}
	}
	return toolKindUnknown
}

type toolFailureKey struct{}

// toolFailure carries a failed call's error from dispatchTool back to the
// span around it, so the span can name the error's type without the
// dispatch signatures growing a return value.
type toolFailure struct{ err error }

func noteToolFailure(ctx context.Context, err error) {
	if f, ok := ctx.Value(toolFailureKey{}).(*toolFailure); ok && f != nil && err != nil {
		f.err = err
	}
}

// traceTool runs call under a call tool span and counts it. A tool name
// reaches the span only when a host or the engine registered it; a name the
// model made up is "unknown", because it is model output. Metric labels
// carry the kind alone: registered names are host-defined and unbounded.
//
// The span and the metrics are closed in a deferred call, so a tool, an
// authorizer or a hook that panics still ends its span (as the class
// "panic") and is still counted before the panic carries on.
func (e *Engine) traceTool(ctx context.Context, tc llm.ToolCall, call func(context.Context) (string, toolOutcome, pendingWait)) (result string, outcome toolOutcome, wait pendingWait) {
	kind := e.toolKind(tc.Name)
	name := tc.Name
	if kind == toolKindUnknown {
		name = labelUnknown
	}
	attrs := []attribute.KeyValue{
		attribute.String(attrGenAIOperation, "execute_tool"),
		attribute.String(attrGenAIToolName, name),
		attribute.String(attrToolKind, kind),
	}
	if callID, ok := toolCallID(tc.ID); ok {
		attrs = append(attrs, attribute.String(attrGenAIToolCallID, callID))
	}
	failure := &toolFailure{}
	ctx = context.WithValue(ctx, toolFailureKey{}, failure)
	ctx, span := telemetry.Tracer().Start(ctx, spanCallTool, trace.WithAttributes(attrs...))
	start := time.Now()
	returned := false
	defer func() {
		label := toolOutcomeLabel(outcome)
		var failErr error
		switch {
		case !returned:
			label, failErr = "failed", telemetry.ErrPanic
		case outcome == outcomeFailed || outcome == outcomeUncertain:
			failErr = failure.err
			if failErr == nil {
				failErr = errToolFailed
			}
		case outcome == outcomeCompleted && failure.err != nil:
			// The tool answered the model with an error payload (a failed
			// retrieval, say) and the loop treats the call as completed.
			// What the model and the hooks see is unchanged; the span and
			// the metric say it failed.
			label, failErr = "failed", failure.err
		}
		span.SetAttributes(attribute.String(attrToolOutcome, label))
		if failErr != nil {
			telemetry.Fail(span, failErr)
		}
		e.instr.toolHandled(kind, label, !returned || outcome != outcomePending, time.Since(start))
		span.End()
	}()

	result, outcome, wait = call(ctx)
	returned = true
	return result, outcome, wait
}

// toolCallID returns the provider's call ID for a span, or false when it
// is not a short opaque token. The ID comes from the model's response, so
// anything long or with characters an ID would not use is dropped.
func toolCallID(id string) (string, bool) {
	if id == "" || len(id) > 64 {
		return "", false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.', c == ':':
		default:
			return "", false
		}
	}
	return id, true
}

// errToolFailed stands in for a failure whose error stayed inside the tool.
var errToolFailed = errors.New("cortex: tool call failed")

// ──────────────────────────────────────────────────
// Publication spans
// ──────────────────────────────────────────────────

type publicationBatchKey struct{}

// startPublicationCheck opens a check publication span around one call to
// the publication gate. Inside a batch (history, retrieved sections) the
// batch span stands for every check in it and this returns no span.
func startPublicationCheck(ctx context.Context, source string, direction safety.Direction, mode safety.Mode) (context.Context, trace.Span) {
	if ctx.Value(publicationBatchKey{}) != nil {
		return ctx, nil
	}
	return telemetry.Tracer().Start(ctx, spanCheckPublication, trace.WithAttributes(
		attribute.String(attrPublicationSource, publicationSourceLabel(source)),
		attribute.String(attrPublicationDirection, directionLabel(direction)),
		attribute.String(attrPublicationMode, string(mode)),
	))
}

// endPublicationCheck records the gate's answer. A rejection is not an
// error: the gate did its job. A gate that could not answer is.
func endPublicationCheck(span trace.Span, status safety.PublicationStatus, err error) {
	if span == nil {
		return
	}
	span.SetAttributes(attribute.String(attrPublicationStatus, publicationResult(status, err)))
	if err != nil && publicationErrorLabel(err) == "unavailable" {
		telemetry.Fail(span, err)
	}
	span.End()
}

// startPublicationBatch opens one span for a run of checks over many items,
// so a hundred history messages cost one span instead of a hundred. It
// opens nothing when no gate will be consulted.
func (e *Engine) startPublicationBatch(ctx context.Context, p safety.Policy, source string, direction safety.Direction, items int) (batchCtx context.Context, done func(error)) {
	resolved, err := p.Resolve()
	if err != nil || resolved.Mode == safety.ModeDisabled || e.publicationGate == nil || items == 0 || ctx.Value(publicationBatchKey{}) != nil {
		return ctx, func(error) {}
	}
	ctx, span := telemetry.Tracer().Start(ctx, spanCheckPublication, trace.WithAttributes(
		attribute.String(attrPublicationSource, publicationSourceLabel(source)),
		attribute.String(attrPublicationDirection, directionLabel(direction)),
		attribute.String(attrPublicationMode, string(resolved.Mode)),
		attribute.Int(attrPublicationChecks, items),
	))
	ctx = context.WithValue(ctx, publicationBatchKey{}, true)
	return ctx, func(err error) {
		if err != nil {
			span.SetAttributes(attribute.String(attrPublicationStatus, publicationErrorLabel(err)))
			if publicationErrorLabel(err) == "unavailable" {
				telemetry.Fail(span, err)
			}
		}
		span.End()
	}
}

// ──────────────────────────────────────────────────
// Suspension and orchestration spans
// ──────────────────────────────────────────────────

func (e *Engine) finishSuspend(span trace.Span, label string, err error) {
	result := resultOK
	if err != nil {
		result = resultError
		telemetry.Fail(span, err)
	}
	span.SetAttributes(attribute.String(attrSuspensionReason, label))
	if e.instr != nil {
		e.instr.suspended.Inc(label, result)
	}
	span.End()
}

func (e *Engine) finishOrchestration(span trace.Span, rec *orchestration.Run, err error) {
	if rec != nil {
		result := resultOK
		if rec.Status != orchestration.StatusCompleted {
			result = resultError
		}
		span.SetAttributes(
			attribute.String(attrOrchestrationID, rec.ID.String()),
			attribute.String(attrOrchestrationStrategy, strategyLabel(rec.Strategy)),
			attribute.Int(attrOrchestrationRuns, len(rec.AgentRunIDs)),
			attribute.String(attrOrchestrationStatus, rec.Status),
		)
		if e.instr != nil {
			e.instr.orchestrations.Inc(strategyLabel(rec.Strategy), result)
		}
	}
	if err != nil {
		telemetry.Fail(span, err)
	}
	span.End()
}
