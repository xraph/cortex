package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

type completionAbandonmentStore struct {
	coordination.TaskCompletionAbandonmentStore
	lose bool
}

func (s *completionAbandonmentStore) AbandonTaskCompletion(ctx context.Context, r coordination.TaskCompletionAbandonmentRequest) (coordination.TaskCompletionSettlement, error) {
	result, err := s.TaskCompletionAbandonmentStore.AbandonTaskCompletion(ctx, r)
	if err == nil && s.lose {
		s.lose = false
		return coordination.TaskCompletionSettlement{}, errCompletionFixture
	}
	return result, err
}

type completionAbandonmentAuthority struct{ deny, duplicate bool }

func (a completionAbandonmentAuthority) WithTaskCompletionAbandonmentFence(_ context.Context, r coordination.TaskCompletionAbandonmentRequest, next func(coordination.TaskCompletionAbandonmentRequest) error) error {
	if err := next(r); err != nil {
		return err
	}
	if a.duplicate {
		return next(r)
	}
	return nil
}
func (a completionAbandonmentAuthority) AuthorizeTaskCompletionAbandonment(context.Context, coordination.TaskCompletionAbandonmentRequest) error {
	if a.deny {
		return errCompletionFixture
	}
	return nil
}

func completionAbandonment(c coordination.TaskCompletion) coordination.TaskCompletionAbandonmentRequest {
	now := time.Now().UTC()
	return coordination.TaskCompletionAbandonmentRequest{Completion: c, Proof: coordination.PreparedRunAbandonment{AdmissionDigest: c.Digest, Issuer: "worker-registry", ProofRef: "permanent-worker-exclusion", ProofDigest: evidence.Digest([]byte("host-quiescence-proof")), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, Actor: c.Request.Actor, Reason: "worker and provider permanently excluded"}
}

// TaskCompletionAbandonment tests permanent closure, conservative accounting and
// current authority. These fixture attestations do not qualify a live host fence.
func TaskCompletionAbandonment(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"absent", "lost_ack", "late_supported_review", "expired", "future", "wrong_admission", "denied", "duplicate_callback", "scope"} {
		t.Run(mode, func(t *testing.T) {
			backend := newStore(t)
			ctx, req, accept, review := taskCompletionFixture(t, backend)
			intents := resultCapability[coordination.TaskCompletionStore](t, backend)
			admitted, err := intents.ReserveTaskCompletion(ctx, req)
			require.NoError(t, err)
			settlements := resultCapability[coordination.TaskCompletionSettlementStore](t, backend)
			pending, err := settlements.SettleTaskCompletion(ctx, req.AssignmentID, admitted.Digest)
			require.NoError(t, err)
			require.Nil(t, pending, "absence alone cannot close dispatch")
			if mode == "late_supported_review" {
				_, err = resultCapability[coordination.ReviewStore](t, backend).RecordAssignmentReview(ctx, review)
				require.NoError(t, err)
				adjudication, adjudicationErr := resultCapability[coordination.TaskAdjudicationStore](t, backend).RecordTaskAdjudication(ctx, coordination.TaskAdjudicationRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixTaskAdjudication), PlanID: req.Handoff.PlanID, PlanDigest: req.Handoff.PlanDigest, TaskID: req.Handoff.TaskID, AssignmentID: req.AssignmentID, AssignmentRevision: req.AssignmentRevision, AssignmentDigest: req.AssignmentDigest, BindingDigest: req.BindingDigest, ReviewID: review.ID, ReviewDigest: review.Digest, PolicyRevision: "review/1", Actor: req.Actor, Reason: "independent supported review"})
				require.NoError(t, adjudicationErr)
				accept.SchemaVersion = 2
				accept.Adjudication = &coordination.TaskAdjudicationRef{ID: adjudication.Request.ID, Digest: adjudication.Digest}
			}
			request := completionAbandonment(admitted)
			authority := completionAbandonmentAuthority{deny: mode == "denied", duplicate: mode == "duplicate_callback"}
			closer, err := coordination.NewTaskCompletionCloser(&completionAbandonmentStore{TaskCompletionAbandonmentStore: resultCapability[coordination.TaskCompletionAbandonmentStore](t, backend), lose: mode == "lost_ack"}, authority)
			require.NoError(t, err)
			switch mode {
			case "expired":
				request.Proof.ObservedAt = admitted.ObservedAt
				request.Proof.ExpiresAt = time.Now().Add(-time.Nanosecond)
			case "future":
				request.Proof.ObservedAt = time.Now().Add(time.Minute)
				request.Proof.ExpiresAt = time.Now().Add(2 * time.Minute)
			case "wrong_admission":
				request.Proof.AdmissionDigest = evidence.Digest([]byte("another admission"))
			case "scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}})
			}
			closed, err := closer.Abandon(ctx, request)
			if mode == "lost_ack" {
				require.Error(t, err)
				closed, err = closer.Abandon(ctx, request)
			}
			if mode != "absent" && mode != "lost_ack" && mode != "late_supported_review" && mode != "duplicate_callback" {
				require.Error(t, err)
				return
			}
			if mode == "duplicate_callback" {
				require.Error(t, err, "multiple callbacks cannot report success")
				return
			}
			require.NoError(t, err)
			if mode == "late_supported_review" {
				_, acceptErr := resultCapability[coordination.TaskAcceptanceStore](t, backend).AcceptTaskResult(ctx, accept)
				require.Error(t, acceptErr, "supported review cannot reopen abandoned dispatch")
			}
			require.NoError(t, closed.Validate())
			require.NotNil(t, closed.Abandonment)
			require.Zero(t, closed.Settlement.Request.Usage.Known)
			budget := resultCapability[execution.BudgetStore](t, backend)
			reservation, err := budget.GetReservation(ctx, req.Reservation.ID)
			require.NoError(t, err)
			require.NotNil(t, reservation.Settlement)
			account, err := budget.GetBudgetSnapshot(ctx, req.Handoff.PlanID)
			require.NoError(t, err)
			require.True(t, account.Charged.Covers(req.Reservation.Allocation), "unknown provider costs cannot be refunded")
			retry, err := closer.Abandon(ctx, request)
			require.NoError(t, err)
			require.Equal(t, closed.Digest, retry.Digest)
			request.Proof.ProofRef = "another proof"
			_, err = closer.Abandon(ctx, request)
			require.Error(t, err)
			replayed, err := intents.ReserveTaskCompletion(ctx, req)
			require.NoError(t, err)
			require.False(t, replayed.Fresh)
			retained, err := settlements.SettleTaskCompletion(ctx, req.AssignmentID, admitted.Digest)
			require.NoError(t, err)
			require.Equal(t, closed.Digest, retained.Digest)
		})
	}
}
