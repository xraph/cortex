package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// checkpointDecidedByEngine is the DecidedBy the engine stamps on a
// decision no human made, so an audit reader can tell one from an
// operator's.
const checkpointDecidedByEngine = "cortex"

// checkpointStatePending is the state a checkpoint is created in, and the
// one ListPending selects on.
const checkpointStatePending = "pending"

// checkpointSuspensionKey is the metadata key createCheckpoint stamps
// with the id of the suspension the row was opened alongside. Its
// presence is how ResolveCheckpoint knows a paused run is waiting behind
// a checkpoint, so a host that wrote its own row still gets the
// record-only behavior it has always had.
const checkpointSuspensionKey = "suspension_id"

// pendingCall records one tool call the loop could not finish itself.
//
// Arguments travel with it rather than being left in the continuation's
// last assistant message. Whoever resumes an external call has to
// actually execute it, and making them match call ids against the
// message history to find out with what would be a puzzle with exactly
// one consumer.
func pendingCall(tc llm.ToolCall) suspension.PendingCall {
	return suspension.PendingCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
}

// pendingCalls is everything one step could not finish: the calls, and
// the reason each of them is waiting on.
//
// The reasons are kept per call rather than folded together as they
// arrive, because folding is where a step carrying two of them went
// wrong. A suspension holds one reason and every path downstream reads
// it as the reason for ALL of its pending calls: an approved checkpoint
// hands back "execute" for every call in the set, so a plain external
// sibling folded into an approval was force-failed for something it
// never did. Keeping them apart is what lets suspend refuse the step
// instead of quietly corrupting one of its calls.
type pendingCalls struct {
	calls   []suspension.PendingCall
	reasons []suspension.SuspendReason
}

func (p *pendingCalls) add(tc llm.ToolCall, reason suspension.SuspendReason) {
	p.calls = append(p.calls, pendingCall(tc))
	p.reasons = append(p.reasons, reason)
}

func (p *pendingCalls) any() bool { return len(p.calls) > 0 }

// reason is the one reason this step's suspension carries, or the error
// that says why these calls cannot share one.
//
// Refusing here is the whole point. The alternative downstream is worse
// than a failed run: an approval can only be answered through
// ResolveCheckpoint, which grants every pending call at once, so the
// external call in the set is failed with a message about a tool nobody
// asked the engine to run, three steps away from anything that explains
// it.
func (p *pendingCalls) reason() (suspension.SuspendReason, error) {
	if len(p.reasons) == 0 {
		return "", errors.New("suspend was asked for with no pending calls")
	}
	first := p.reasons[0]
	for i, r := range p.reasons {
		if r == first {
			continue
		}
		return "", fmt.Errorf(
			"tool %q pends for %s and tool %q pends for %s in the same step: one suspension carries one reason, and that reason decides how every call under it is answered, so the engine will not pause for both at once. Keep them out of one step by narrowing the agent's tool list, or give the engine a handler for the external tool so only the approval is pending",
			p.calls[0].Name, first, p.calls[i].Name, r)
	}
	return first, nil
}

// suspend stops a run that cannot go on until something outside the
// engine answers, and it is the only place in the loop that writes a
// suspension. Reason is a parameter because the loop does not care why a
// call is pending, only that it is: an external tool today, a call
// escalated for human approval next, both landing here rather than each
// growing its own suspend path.
//
// The suspension is written before the run flips to paused, and the order
// is load-bearing. If the run went paused first and the suspension write
// then failed, the run would be paused with nothing to resume from and no
// way back: ClaimSuspension only ever finds a suspension for a paused
// run. This way round the worst case is a suspension attached to a run
// that is still marked running, which Claim refuses and the caller sees
// as a failed run rather than a wedged one.
//
// Nothing is saved to conversation memory here. The continuation carries
// the messages, and Resume is what eventually persists them: saving now
// would have the resumed run reload its own in-flight messages as
// history and answer them twice.
func (e *Engine) suspend(ctx context.Context, r *run.Run, pend pendingCalls, cont suspension.Continuation) (suspension.SuspendReason, error) {
	reason, err := pend.reason()
	if err != nil {
		return "", err
	}

	s := &suspension.Suspension{
		Entity:    cortex.NewEntity(),
		ID:        id.NewSuspensionID(),
		RunID:     r.ID,
		Scope:     r.Scope,
		Reason:    reason,
		Pending:   pend.calls,
		Cont:      cont,
		ExpiresAt: e.suspensionDeadline(),
	}
	if err = e.store.CreateSuspension(ctx, s); err != nil {
		return "", fmt.Errorf("create suspension: %w", err)
	}

	// The checkpoint goes between the two writes above and below for the
	// same reason the suspension goes before the state flip. A run paused
	// for approval with no checkpoint behind it is a run waiting on a
	// decision nobody can see: ListPending never shows it, so nothing
	// resolves it. Written here, a failed checkpoint write takes the
	// suspension down with it and the run never reaches paused at all.
	cp, err := e.createCheckpoint(ctx, r, s, reason)
	if err != nil {
		if delErr := e.store.DeleteSuspension(ctx, r.ID); delErr != nil {
			e.logger.Error("delete suspension after a failed checkpoint write", log.String("error", delErr.Error()))
		}
		return "", fmt.Errorf("create checkpoint: %w", err)
	}

	r.State = run.StatePaused
	r.StepCount = cont.StepIndex
	r.TokensUsed = cont.TokensUsed
	if err := e.store.UpdateRun(ctx, r); err != nil {
		// The suspension is written and now belongs to nobody: the
		// caller is about to fail the run, and nothing else deletes the
		// row. Best effort, since the store just failed a write, and
		// logged rather than returned: the caller needs the reason the
		// run could not be paused, not the reason the cleanup also
		// failed. If this delete fails too the row outlives its run,
		// and the sweeper picks it up: its claim wants a paused run and
		// this one is about to be failed, so the claim is refused and
		// dropOrphanedSuspension clears the row once the run reaches a
		// terminal state.
		if delErr := e.store.DeleteSuspension(ctx, r.ID); delErr != nil {
			e.logger.Error("delete orphaned suspension", log.String("error", delErr.Error()))
		}
		// Same cleanup for the checkpoint, by the only route the store
		// offers: there is no delete, so it is resolved against itself.
		// Left pending it would sit in every operator's queue forever,
		// asking for a decision on a run that never paused, and
		// approving it would try to resume a run that is about to fail.
		if cp != nil {
			decision := checkpoint.Decision{
				DecidedBy: checkpointDecidedByEngine,
				Reason:    "the run could not be paused, so this checkpoint can never be resolved into a resume",
				DecidedAt: time.Now().UTC(),
			}
			if resErr := e.store.Resolve(ctx, cp.ID, decision); resErr != nil {
				e.logger.Error("resolve orphaned checkpoint", log.String("error", resErr.Error()))
			}
		}
		return "", fmt.Errorf("update run to paused: %w", err)
	}

	// Emitted after the flip, not where the row is written: subscribers
	// hear about a checkpoint only once the run it belongs to is really
	// waiting on it.
	if cp != nil {
		e.extensions.EmitCheckpointCreated(ctx, cp.ID, r.ID, cp.Reason)
	}
	return reason, nil
}

// suspensionDeadline is when the sweeper may fail this run, or nil when
// expiry is switched off. It is stamped at suspend time rather than
// derived at sweep time so a TTL change never moves the deadline of a
// run that is already waiting: whoever was told to answer by Tuesday
// still has until Tuesday.
func (e *Engine) suspensionDeadline() *time.Time {
	if e.suspensionTTL <= 0 {
		return nil
	}
	t := time.Now().UTC().Add(e.suspensionTTL)
	return &t
}

// createCheckpoint opens the human-facing half of an approval
// suspension. The suspension is what the loop resumes from; the
// checkpoint is what somebody actually sees and answers, and it is the
// only one of the two the REST surface and the plugin hooks know about.
//
// Nothing is created for any other reason: an external-tool pause is
// answered by the host that registered the tool, not by a person, and a
// checkpoint queue full of rows no human is meant to decide is a queue
// people stop reading.
func (e *Engine) createCheckpoint(ctx context.Context, r *run.Run, s *suspension.Suspension, reason suspension.SuspendReason) (*checkpoint.Checkpoint, error) {
	if reason != suspension.ReasonApproval {
		return nil, nil
	}

	names := make([]string, 0, len(s.Pending))
	for _, p := range s.Pending {
		names = append(names, p.Name)
	}

	// The step that asked for these calls, not the next one to run.
	// Cont.StepIndex is where the loop resumes, which is one past it.
	stepIndex := s.Cont.StepIndex - 1
	if stepIndex < 0 {
		stepIndex = 0
	}

	cp := &checkpoint.Checkpoint{
		Entity:    cortex.NewEntity(),
		ID:        id.NewCheckpointID(),
		RunID:     r.ID,
		AgentID:   r.AgentID,
		Scope:     r.Scope,
		Reason:    "tool call requires approval: " + strings.Join(names, ", "),
		StepIndex: stepIndex,
		State:     checkpointStatePending,
		Metadata: map[string]any{
			checkpointSuspensionKey: s.ID.String(),
			"tools":                 names,
		},
	}
	if err := e.store.CreateCheckpoint(ctx, cp); err != nil {
		return nil, err
	}
	// No hook fires here. The run is not paused yet, and a flip that
	// then fails resolves this checkpoint through a store call that
	// emits nothing, so a subscriber would be left holding a created
	// event with no resolved event ever to match it. suspend emits once
	// the pause is real.
	return cp, nil
}
