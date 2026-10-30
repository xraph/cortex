package cortex

import "errors"

var (
	// Store errors.
	ErrNoStore         = errors.New("cortex: no store configured")
	ErrStoreClosed     = errors.New("cortex: store closed")
	ErrMigrationFailed = errors.New("cortex: migration failed")
	// ErrConversationUnavailable prevents execution with an unreadable session history.
	ErrConversationUnavailable = errors.New("cortex: conversation history unavailable")
	// ErrContextUnavailable prevents use of an unavailable or changed context policy.
	ErrContextUnavailable = errors.New("cortex: context policy unavailable")

	// Not found errors.
	ErrAgentNotFound                    = errors.New("cortex: agent not found")
	ErrRunConflict                      = errors.New("cortex: run revision conflict")
	ErrRunNotFound                      = errors.New("cortex: run not found")
	ErrStepNotFound                     = errors.New("cortex: step not found")
	ErrToolCallNotFound                 = errors.New("cortex: tool call not found")
	ErrSkillNotFound                    = errors.New("cortex: skill not found")
	ErrTraitNotFound                    = errors.New("cortex: trait not found")
	ErrBehaviorNotFound                 = errors.New("cortex: behavior not found")
	ErrPersonaNotFound                  = errors.New("cortex: persona not found")
	ErrSessionNotFound                  = errors.New("cortex: session not found")
	ErrCheckpointNotFound               = errors.New("cortex: checkpoint not found")
	ErrOrchestrationNotFound            = errors.New("cortex: orchestration not found")
	ErrOrchestrationRunNotFound         = errors.New("cortex: orchestration run not found")
	ErrOrchestrationRunConflict         = errors.New("cortex: orchestration run revision conflict")
	ErrOrchestrationRunClosed           = errors.New("cortex: orchestration run is closed")
	ErrOrchestrationCoordinatorNotFound = errors.New("cortex: orchestration coordinator not found")
	ErrOrchestrationInvocationNotFound  = errors.New("cortex: orchestration invocation not found")
	ErrWorkingMemoryNotFound            = errors.New("cortex: working memory not found")
	ErrOverlayNotFound                  = errors.New("cortex: prompt overlay not found")

	// Continuation errors.
	ErrOrchestrationWaiting                 = errors.New("cortex: orchestration is waiting for a child")
	ErrOrchestrationContinuationUnavailable = errors.New("cortex: orchestration continuation unavailable")

	// Conflict errors.
	ErrAlreadyExists = errors.New("cortex: resource already exists")
	// ErrPreparationUnavailable means the requested consistency guarantee cannot be provided.
	ErrPreparationUnavailable = errors.New("cortex: atomic preparation unavailable")
	// ErrPreparationConflict requires fresh preparation after an observed source change.
	ErrPreparationConflict = errors.New("cortex: preparation source changed or unavailable")

	// Task call errors.
	ErrTaskRunReconciliationRequired = errors.New("cortex: task run start requires reconciliation")
	ErrTaskCallUnavailable           = errors.New("cortex: task call authority unavailable")
	ErrTaskCallUncertain             = errors.New("cortex: task call outcome uncertain")
	ErrInvalidRunCall                = errors.New("cortex: invalid run call")
	ErrRunCallConflict               = errors.New("cortex: run call conflict")
	ErrRunCallNotFound               = errors.New("cortex: run call not found")
	ErrProviderCallUncertain         = errors.New("cortex: provider call uncertain")
	ErrRunCallsSealed                = errors.New("cortex: run calls sealed")
	ErrRunCallUnavailable            = errors.New("cortex: run call transaction unavailable")

	// Scheduler errors.
	ErrInvalidScheduler                = errors.New("cortex: invalid scheduler configuration or snapshot")
	ErrSchedulerFull                   = errors.New("cortex: scheduler queue full")
	ErrSchedulerStopped                = errors.New("cortex: scheduler stopped")
	ErrSchedulerReconciliationRequired = errors.New("cortex: scheduler admission requires reconciliation")

	// State errors.
	ErrInvalidState     = errors.New("cortex: invalid state transition")
	ErrRunCancelled     = errors.New("cortex: run cancelled")
	ErrInvalidPrepared  = errors.New("cortex: invalid prepared execution")
	ErrNoLLM            = errors.New("cortex: no model provider configured")
	ErrRunIncomplete    = errors.New("cortex: run incomplete")
	ErrRunAlreadyDone   = errors.New("cortex: run already completed")
	ErrBudgetExhausted  = errors.New("cortex: budget exhausted")
	ErrMaxStepsReached  = errors.New("cortex: maximum steps reached")
	ErrMaxTokensReached = errors.New("cortex: maximum tokens reached")
	// ErrEngineStopping refuses a run that would start while Stop drains the
	// engine. Nothing was written for it; retry on another worker, or after
	// Start.
	ErrEngineStopping = errors.New("cortex: engine is stopping")

	// ErrInvalidDelegationPlan rejects a manager plan before worker dispatch.
	ErrInvalidDelegationPlan = errors.New("cortex: invalid delegation plan")

	// Suspension errors.
	//
	// ErrNotSuspended is returned by ClaimSuspension to the losing side of
	// a double resume: the run was not paused (or another caller already
	// claimed it) when the claim ran.
	ErrNotSuspended = errors.New("cortex: run not suspended")
	// ErrSuspensionExpired is returned when a resume targets a suspension
	// past its ExpiresAt. The suspension row still exists for the expiry
	// sweep to find and clean up; it is simply no longer resumable.
	ErrSuspensionExpired = errors.New("cortex: suspension expired")
	// ErrResultsMismatch is returned when the results a caller supplies to
	// resume an external-tool suspension do not correspond to the pending
	// calls it was waiting on.
	ErrResultsMismatch = errors.New("cortex: suspension results do not match pending calls")
	// ErrInvalidContinuation is returned when a suspension carries a
	// continuation the loop cannot run: today that means a zero Config,
	// which a resume would otherwise read as a step budget of nothing.
	//
	// It is its own sentinel rather than ErrMaxStepsReached, which is what
	// a zero budget looks like from inside the loop. That error is a
	// matched-on contract meaning "this run used up its steps", and it
	// sends an operator to raise the budget for a run whose real problem
	// is a continuation nothing could have resumed.
	ErrInvalidContinuation = errors.New("cortex: suspension continuation is unusable")

	// Principal errors.
	//
	// ErrPrincipalRevoked is what a PrincipalCodec wraps when a run's
	// identity is permanently gone: the member was removed, the key
	// revoked. A resume that meets it fails the run with the codec's
	// reason rather than continuing under nobody or under the resumer.
	ErrPrincipalRevoked = errors.New("cortex: principal no longer valid")
	// ErrPrincipalUnavailable means a principal could not be encoded or
	// decoded right now. A run is not created without its principal, and a
	// paused run whose principal cannot be decoded stays paused for the
	// next resume to try again.
	ErrPrincipalUnavailable = errors.New("cortex: principal unavailable")
	// ErrPrincipalCodecRequired is returned by engine.New when a tool
	// authorizer is installed without a principal codec. Without one,
	// every resumed run would reach the authorizer as nobody.
	ErrPrincipalCodecRequired = errors.New("cortex: a tool authorizer needs a principal codec so resumed runs keep their principal")

	// Run continuation errors.
	//
	// ErrRunNotContinuable is returned when a continuation targets a run
	// that is not ready or reclaimable: another replica already continued
	// it, or it never got that far.
	ErrRunNotContinuable = errors.New("cortex: run is not ready to continue")
	// ErrRunLeaseLost is returned to a worker whose lease on a running run
	// is gone: another worker holds it, or the run left running. The
	// worker must stop writing the run.
	ErrRunLeaseLost = errors.New("cortex: run lease lost")
	// ErrRunLeased is returned when a run's lease has not lapsed yet, so
	// the run still belongs to the worker holding it.
	ErrRunLeased = errors.New("cortex: run lease is still held")
	// ErrContinuationUnavailable is returned by the engine calls that make a
	// run ready or continue one when the store does not implement
	// run.ContinuationStore.
	ErrContinuationUnavailable = errors.New("cortex: store cannot hold ready runs")
	// ErrProgressNotFound is returned when a run has no saved progress
	// snapshot at the caller's scope.
	ErrProgressNotFound = errors.New("cortex: run progress not found")
	// ErrRunInterrupted is returned by a run that its worker handed off
	// while shutting down. The run is reclaimable, not ended: another
	// replica continues it from its last saved progress.
	ErrRunInterrupted = errors.New("cortex: run handed off before it finished")
	// ErrReclaimLimit fails a run that was reclaimed more often than
	// engine.WithReclaimLimit allows. A run that takes its worker down every
	// time it runs is stopped here instead of being handed to the next one.
	ErrReclaimLimit = errors.New("cortex: run reclaimed too many times")
	// ErrRunNotRecoverable fails a reclaimed run that left nothing to
	// continue from: no progress snapshot and no pause it was claimed from.
	ErrRunNotRecoverable = errors.New("cortex: run has no saved progress to continue from")
)

// Evidence and verification errors remain independent of execution outcomes.
var (
	ErrInvalidEvidence           = errors.New("cortex: invalid evidence")
	ErrEvidenceDigestMismatch    = errors.New("cortex: evidence digest mismatch")
	ErrEvidenceAccessDenied      = errors.New("cortex: evidence access denied")
	ErrEvidenceTooLarge          = errors.New("cortex: evidence exceeds size limit")
	ErrEvidenceNotFound          = errors.New("cortex: evidence not found")
	ErrEvidenceDeleted           = errors.New("cortex: evidence deleted")
	ErrEvidenceConflict          = errors.New("cortex: immutable evidence conflict")
	ErrInvalidVerification       = errors.New("cortex: invalid verification record")
	ErrVerificationConflict      = errors.New("cortex: immutable verification conflict")
	ErrVerificationNotFound      = errors.New("cortex: verification record not found")
	ErrVerificationJobNotFound   = errors.New("cortex: verification job not found")
	ErrVerificationJobConflict   = errors.New("cortex: verification job ownership conflict")
	ErrVerificationJobExpired    = errors.New("cortex: verification job deadline or attempts exhausted")
	ErrVerificationJobBudget     = errors.New("cortex: verification job budget exhausted")
	ErrVerificationCallUncertain = errors.New("cortex: verification call already dispatched; reconcile its outcome")
	ErrRunUnverified             = errors.New("cortex: run did not pass required verification")
	ErrVerificationUnavailable   = errors.New("cortex: verification worker unavailable")
	ErrResultCommitUnavailable   = errors.New("cortex: atomic verified result commit unavailable")
	ErrResultAccessDenied        = errors.New("cortex: result content access denied")
	ErrSandboxInputsIncomplete   = errors.New("cortex: sandbox input staging is incomplete")
	ErrSandboxInputsConflict     = errors.New("cortex: sandbox input staging identity conflict")
)

// Public replay errors distinguish unsupported requests, gaps and unavailable storage.
var (
	ErrInvalidPublicEvent      = errors.New("cortex: invalid public event")
	ErrPublicEventConflict     = errors.New("cortex: public event identity or sequence conflict")
	ErrPublicEventCursorGap    = errors.New("cortex: public event cursor expired")
	ErrPublicEventAccessDenied = errors.New("cortex: public event replay denied")
	ErrPublicEventUnavailable  = errors.New("cortex: public event replay unavailable")
)

// Effect errors distinguish a conflicting request from an unresolved external action.
var (
	ErrEffectConflict    = errors.New("cortex: effect operation conflicts with retained request")
	ErrEffectNotFound    = errors.New("cortex: effect operation not found")
	ErrEffectUncertain   = errors.New("cortex: effect outcome requires reconciliation")
	ErrEffectUnavailable = errors.New("cortex: effect executor unavailable")
)

// Handoff errors distinguish invalid proposals from unavailable host admission.
var (
	ErrInvalidHandoff     = errors.New("cortex: invalid task handoff")
	ErrHandoffUnavailable = errors.New("cortex: task handoff unavailable")
)

// Assignment errors describe durable progress journal failures.
var (
	ErrInvalidReservation     = errors.New("cortex: invalid budget reservation")
	ErrReservationConflict    = errors.New("cortex: budget reservation conflict")
	ErrReservationNotFound    = errors.New("cortex: budget reservation not found")
	ErrSharedBudgetExceeded   = errors.New("cortex: shared budget exceeded")
	ErrBudgetUnavailable      = errors.New("cortex: shared budget storage unavailable")
	ErrInvalidTaskAcceptance  = errors.New("cortex: invalid task acceptance")
	ErrInvalidTaskUsage       = errors.New("cortex: invalid task usage")
	ErrTaskUsageConflict      = errors.New("cortex: task usage conflict")
	ErrTaskUsageClosed        = errors.New("cortex: task usage admission closed")
	ErrInvalidTaskSettlement  = errors.New("cortex: invalid task usage settlement")
	ErrTaskSettlementConflict = errors.New("cortex: task usage settlement conflict")
	ErrTaskAcceptanceConflict = errors.New("cortex: task acceptance conflict")
	ErrTaskAcceptanceNotFound = errors.New("cortex: task acceptance not found")
	ErrInvalidClaim           = errors.New("cortex: invalid task claim")
	ErrClaimConflict          = errors.New("cortex: task claim conflict")
	ErrClaimNotFound          = errors.New("cortex: task claim not found")
	ErrInvalidReview          = errors.New("cortex: invalid assignment review")
	ErrReviewConflict         = errors.New("cortex: assignment review conflict")
	ErrReviewNotFound         = errors.New("cortex: assignment review not found")
	ErrInvalidAssignment      = errors.New("cortex: invalid assignment")
	ErrAssignmentConflict     = errors.New("cortex: assignment conflict")
	ErrAssignmentNotFound     = errors.New("cortex: assignment not found")
	ErrAssignmentUnavailable  = errors.New("cortex: assignment transaction unavailable")
)
