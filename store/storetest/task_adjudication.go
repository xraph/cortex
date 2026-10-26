package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func taskAdjudicationFixture(t *testing.T, s store.Store) (context.Context, coordination.TaskAcceptanceRequest, coordination.Review, coordination.TaskAdjudicationRequest) {
	config := taskContractConfig(t)
	var reviewerAgent id.AgentID
	old := config.Plan
	config.Plan = func(p *planning.Plan) {
		if old != nil {
			old(p)
		}
		reviewerAgent = mustCreateAgent(t, s, cortex.WithScope(context.Background(), p.Scope), "")
		p.SchemaVersion = 2
		p.Tasks[0].Review = &planning.ReviewRequirement{ReviewerAgent: reviewerAgent.String(), ReviewerPrincipal: "reviewer-1", PolicyRevision: "review/1"}
	}
	ctx, req := taskAcceptanceFixture(t, s, config)
	journal := resultCapability[coordination.AssignmentStore](t, s)
	subject, err := journal.GetAssignment(ctx, req.AssignmentID)
	require.NoError(t, err)
	h := subject.Handoff
	h.ID = id.New(coordination.PrefixHandoff)
	h.AssignmentID = id.New(coordination.PrefixAssignment)
	h.Purpose = coordination.HandoffReview
	h.Receiver = coordination.Identity{Agent: reviewerAgent.String(), Principal: "reviewer-1"}
	h.Authority.Resources = nil
	h.Authority.Effects = nil
	h.Inputs = append([]evidence.Ref{req.Current.Candidate}, req.Current.Evidence...)
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	assignment, err := journal.ProposeAssignment(ctx, h)
	require.NoError(t, err)
	for _, state := range []coordination.AssignmentState{coordination.AssignmentAccepted, coordination.AssignmentWorking} {
		assignment, err = journal.AdvanceAssignment(ctx, assignmentChange(assignment, state))
		require.NoError(t, err)
	}
	reviewer := newRun(reviewerAgent)
	require.NoError(t, s.CreateRun(ctx, reviewer))
	findings := make([]coordination.ReviewFinding, 0, len(h.Criteria))
	for _, criterion := range h.Criteria {
		findings = append(findings, coordination.ReviewFinding{CriterionID: criterion, Assessment: coordination.ReviewSupported, Reason: "retained evidence supports criterion", EvidenceIDs: []id.ID{req.Current.Candidate.ID}})
	}
	r, err := coordination.SealReview(coordination.Review{SchemaVersion: 1, ID: id.New(coordination.PrefixReview), Handoff: h, SubjectAssignmentID: req.AssignmentID, SubjectRevision: req.AssignmentRevision, SubjectDigest: req.AssignmentDigest, BindingDigest: subject.Submission.BindingDigest, RunID: reviewer.ID, RunRevision: reviewer.Revision + 1, PreparedDigest: evidence.Digest([]byte("independent reviewer")), ReviewedAt: time.Now().UTC(), Findings: findings})
	require.NoError(t, err)
	output, err := r.Output()
	require.NoError(t, err)
	reviewer.State, reviewer.StopReason, reviewer.Output = run.StateCompleted, run.StopFinalAnswer, string(output)
	reviewer.Provenance.PreparedDigest, reviewer.Provenance.AssignmentDigest = r.PreparedDigest, h.Digest
	reviewer.Provenance.ParentRunID, reviewer.Provenance.AcceptanceRef = h.ParentRunID.String(), subject.Submission.JobID.String()
	job, err := resultCapability[verification.JobStore](t, s).GetVerificationJob(ctx, subject.Submission.JobID)
	require.NoError(t, err)
	reviewer.Provenance.AcceptanceRef = job.Request.ContractID.String()
	require.NoError(t, s.UpdateRun(ctx, reviewer))
	adjudication := coordination.TaskAdjudicationRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixTaskAdjudication), PlanID: h.PlanID, PlanDigest: h.PlanDigest, TaskID: h.TaskID, AssignmentID: req.AssignmentID, AssignmentRevision: req.AssignmentRevision, AssignmentDigest: req.AssignmentDigest, BindingDigest: r.BindingDigest, ReviewID: r.ID, ReviewDigest: r.Digest, PolicyRevision: "review/1", Actor: h.Sender.Principal, Reason: "required independent review"}
	return ctx, req, r, adjudication
}

// TaskAdjudication exercises required review, retained evidence and atomic acceptance.
func TaskAdjudication(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"supported", "missing", "contradicted", "downgrade", "changed_policy", "human_override", "late_contradiction", "deleted_evidence", "failed_verifier"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, req, review, request := taskAdjudicationFixture(t, s)
			adjudications := resultCapability[coordination.TaskAdjudicationStore](t, s)
			accepts := resultCapability[coordination.TaskAcceptanceStore](t, s)
			_, err := accepts.AcceptTaskResult(ctx, req)
			require.Error(t, err, "plan requirement must reject old request before any review exists")
			if mode == "missing" {
				return
			}
			if mode == "contradicted" || mode == "human_override" {
				review.Findings[0].Assessment = coordination.ReviewContradicted
				review, err = coordination.SealReview(review)
				require.NoError(t, err)
				current, readErr := s.GetRun(ctx, review.RunID)
				require.NoError(t, readErr)
				output, outputErr := review.Output()
				require.NoError(t, outputErr)
				current.Output = string(output)
				require.NoError(t, s.UpdateRun(ctx, current))
				review.RunRevision = current.Revision
				review, err = coordination.SealReview(review)
				require.NoError(t, err)
				request.ReviewDigest = review.Digest
			}
			_, err = resultCapability[coordination.ReviewStore](t, s).RecordAssignmentReview(ctx, review)
			require.NoError(t, err)
			if mode == "changed_policy" {
				request.PolicyRevision = "changed"
			}
			if mode == "human_override" {
				request.Actor = "human"
			}
			a, err := adjudications.RecordTaskAdjudication(ctx, request)
			if mode == "changed_policy" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			retry, err := adjudications.RecordTaskAdjudication(ctx, request)
			require.NoError(t, err)
			require.Equal(t, a, retry)
			req.SchemaVersion = 2
			req.Adjudication = &coordination.TaskAdjudicationRef{ID: a.Request.ID, Digest: a.Digest}
			if mode == "downgrade" {
				req.SchemaVersion = 1
				req.Adjudication = nil
			}
			if mode == "late_contradiction" {
				review.ID = id.New(coordination.PrefixReview)
				review.Findings[0].Assessment = coordination.ReviewContradicted
				current, readErr := s.GetRun(ctx, review.RunID)
				require.NoError(t, readErr)
				review, err = coordination.SealReview(review)
				require.NoError(t, err)
				output, outputErr := review.Output()
				require.NoError(t, outputErr)
				current.Output = string(output)
				require.NoError(t, s.UpdateRun(ctx, current))
				review.RunRevision = current.Revision
				review, err = coordination.SealReview(review)
				require.NoError(t, err)
				_, err = resultCapability[coordination.ReviewStore](t, s).RecordAssignmentReview(ctx, review)
				require.NoError(t, err)
			}
			if mode == "deleted_evidence" {
				require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), req.Current.Candidate, "deleted"))
			}
			if mode == "failed_verifier" {
				current, readErr := s.GetRun(ctx, req.Result.RunID)
				require.NoError(t, readErr)
				current.State = run.StateFailed
				require.NoError(t, s.UpdateRun(ctx, current))
			}
			accepted, err := accepts.AcceptTaskResult(ctx, req)
			if mode != "supported" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.True(t, accepted.Fresh)
			repeat, err := accepts.AcceptTaskResult(ctx, req)
			require.NoError(t, err)
			require.False(t, repeat.Fresh)
		})
	}
}

func taskCompletionFixture(t *testing.T, s store.Store) (context.Context, coordination.TaskCompletionRequest, coordination.TaskAcceptanceRequest, coordination.Review) {
	t.Helper()
	ctx, accept, review, _ := taskAdjudicationFixture(t, s)
	policy := execution.ReservationPolicy{SchemaVersion: 1, PlanID: review.Handoff.PlanID, Scope: review.Handoff.Scope, Limits: execution.Resources{Steps: int64(review.Handoff.Authority.Budget.MaxSteps), Tokens: int64(review.Handoff.Authority.Budget.MaxTokens), ToolCalls: int64(review.Handoff.Authority.Budget.MaxToolCalls), FanOut: 10}, Deadline: review.Handoff.Deadline, TokenOnly: true}
	budgets := resultCapability[execution.BudgetStore](t, s)
	require.NoError(t, budgets.CreateReservationPolicy(ctx, policy))
	h := review.Handoff
	h.ID = id.New(coordination.PrefixHandoff)
	h.AssignmentID = id.New(coordination.PrefixAssignment)
	h, err := coordination.SealHandoff(h)
	require.NoError(t, err)
	reservation := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: h.PlanID, PlanDigest: h.PlanDigest, Scope: h.Scope, TaskID: h.TaskID, OwnerRef: h.AssignmentID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: int64(h.Authority.Budget.MaxSteps), Tokens: int64(h.Authority.Budget.MaxTokens), ToolCalls: int64(h.Authority.Budget.MaxToolCalls), FanOut: 1}, Deadline: h.Deadline}
	req := coordination.TaskCompletionRequest{SchemaVersion: 1, SessionID: id.NewSessionID(), AssignmentID: accept.AssignmentID, AssignmentRevision: accept.AssignmentRevision, AssignmentDigest: accept.AssignmentDigest, BindingDigest: review.BindingDigest, Handoff: h, RunID: id.NewAgentRunID(), ReviewID: id.New(coordination.PrefixReview), AdjudicationID: id.New(coordination.PrefixTaskAdjudication), AcceptanceID: id.New(coordination.PrefixTaskAcceptance), PreparedDigest: evidence.Digest([]byte("independent prepared")), InputDigest: evidence.Digest([]byte("review input")), Reservation: reservation, Actor: h.Sender.Principal, Reason: "review before acceptance"}
	return ctx, req, accept, review
}

// TaskCompletionAdmission checks that review identity and capacity are atomic.
func TaskCompletionAdmission(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req, _, _ := taskCompletionFixture(t, s)
	completionStore := resultCapability[coordination.TaskCompletionStore](t, s)
	type admissionResult struct {
		result coordination.TaskCompletion
		err    error
	}
	outcomes := make(chan admissionResult, 2)
	var started sync.WaitGroup
	started.Add(2)
	for range 2 {
		go func() {
			started.Done()
			started.Wait()
			result, callErr := completionStore.ReserveTaskCompletion(ctx, req)
			outcomes <- admissionResult{result, callErr}
		}()
	}
	var admitted coordination.TaskCompletion
	fresh := 0
	for range 2 {
		outcome := <-outcomes
		require.NoError(t, outcome.err)
		if outcome.result.Fresh {
			fresh++
			admitted = outcome.result
		}
	}
	require.Equal(t, 1, fresh)
	err := admitted.Validate()
	require.NoError(t, err)
	require.True(t, admitted.Fresh)
	require.NoError(t, admitted.Validate())
	retry, err := completionStore.ReserveTaskCompletion(ctx, req)
	require.NoError(t, err)
	require.False(t, retry.Fresh)
	require.Equal(t, admitted.Digest, retry.Digest)
	stored, err := completionStore.GetTaskCompletion(ctx, req.AssignmentID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.False(t, stored.Fresh)
	assigned, err := resultCapability[coordination.AssignmentStore](t, s).GetAssignment(ctx, req.Handoff.AssignmentID)
	require.NoError(t, err)
	require.Equal(t, coordination.AssignmentWorking, assigned.State)
	changed := req
	changed.RunID = id.NewAgentRunID()
	_, err = completionStore.ReserveTaskCompletion(ctx, changed)
	require.Error(t, err)
	retained, err := resultCapability[execution.BudgetStore](t, s).GetReservation(ctx, req.Reservation.ID)
	require.NoError(t, err)
	require.Nil(t, retained.Settlement)
}
