package coordination_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

func completionRequestFixture(t *testing.T) (coordination.TaskCompletionRequest, coordination.Assignment, verification.Binding, planning.Plan, time.Time) {
	r, subject, binding, p, now := reviewFixture(t)
	p.SchemaVersion = 2
	p.Tasks[0].Review = &planning.ReviewRequirement{ReviewerAgent: r.Handoff.Receiver.Agent, ReviewerPrincipal: r.Handoff.Receiver.Principal, PolicyRevision: "1"}
	p, err := planning.Seal(p, planning.Limits{})
	require.NoError(t, err)
	h := subject.Handoff
	h.PlanDigest = p.Digest
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	sub := subject.Submission
	subject, err = coordination.Propose(h, now)
	require.NoError(t, err)
	for _, state := range []coordination.AssignmentState{coordination.AssignmentAccepted, coordination.AssignmentWorking, coordination.AssignmentSubmitted} {
		change := coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: subject.Revision, HandoffDigest: h.Digest, Fence: h.Fence, State: state, Actor: h.Receiver.Principal, Reason: "progress"}
		if state == coordination.AssignmentSubmitted {
			change.Submission = sub
		}
		subject, err = coordination.Advance(subject, change, now)
		require.NoError(t, err)
	}
	h = r.Handoff
	h.PlanDigest = p.Digest
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	req := coordination.TaskCompletionRequest{SchemaVersion: 1, SessionID: id.NewSessionID(), AssignmentID: subject.Handoff.AssignmentID, AssignmentRevision: subject.Revision, AssignmentDigest: subject.Digest, BindingDigest: sub.BindingDigest, Handoff: h, RunID: r.RunID, ReviewID: r.ID, AdjudicationID: id.New(coordination.PrefixTaskAdjudication), AcceptanceID: id.New(coordination.PrefixTaskAcceptance), PreparedDigest: r.PreparedDigest, InputDigest: evidence.Digest([]byte("private review input")), Actor: h.Sender.Principal, Reason: "review", Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: h.TaskID, OwnerRef: h.AssignmentID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: int64(h.Authority.Budget.MaxSteps), Tokens: int64(h.Authority.Budget.MaxTokens), ToolCalls: int64(h.Authority.Budget.MaxToolCalls), FanOut: 1}, Deadline: h.Deadline}}
	return req, subject, binding, p, now
}

func TestTaskCompletionPinsIndependentBoundedReview(t *testing.T) {
	req, subject, binding, p, now := completionRequestFixture(t)
	require.NoError(t, coordination.ValidateTaskCompletion(req, subject, binding, p, now))
	frozen, _, err := req.Freeze()
	require.NoError(t, err)
	req.Handoff.Inputs[0].Version = "changed"
	require.NotEqual(t, req.Handoff.Inputs[0].Version, frozen.Handoff.Inputs[0].Version)
	for _, mode := range []string{"same_principal", "same_prepared", "same_run", "binding", "subject", "input", "budget", "unknown_schema"} {
		t.Run(mode, func(t *testing.T) {
			req, subject, binding, p, now := completionRequestFixture(t)
			switch mode {
			case "same_principal":
				req.Handoff.Receiver.Principal = subject.Handoff.Receiver.Principal
				req.Handoff, _ = coordination.SealHandoff(req.Handoff)
			case "same_prepared":
				req.PreparedDigest = binding.Context.PreparedDigest
			case "same_run":
				req.RunID = binding.RunID
			case "binding":
				req.BindingDigest = evidence.Digest([]byte("different"))
			case "subject":
				req.AssignmentRevision++
			case "input":
				req.Handoff.Inputs = nil
				req.Handoff, _ = coordination.SealHandoff(req.Handoff)
			case "budget":
				req.Reservation.Allocation.Tokens--
			case "unknown_schema":
				req.SchemaVersion = 2
			}
			require.Error(t, coordination.ValidateTaskCompletion(req, subject, binding, p, now))
		})
	}
}
