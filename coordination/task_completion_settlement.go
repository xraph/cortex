package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// TaskCompletionSettlement retains terminal reviewer accounting or an authenticated
// permanent abandonment.
// Unknown provider usage stays charged. This receipt cannot accept a task, prove
// an absent run never executed, or authorize another reviewer attempt.
type TaskCompletionSettlement struct {
	Abandonment      *TaskCompletionAbandonmentRequest `json:"abandonment,omitempty"`
	SchemaVersion    int                               `json:"schema_version"`
	AssignmentID     id.ID                             `json:"assignment_id"`
	CompletionDigest string                            `json:"completion_digest"`
	RunID            id.AgentRunID                     `json:"run_id"`
	RunRevision      int64                             `json:"run_revision"`
	RunDigest        string                            `json:"run_digest"`
	Settlement       execution.SettlementReceipt       `json:"settlement"`
	ObservedAt       time.Time                         `json:"observed_at"`
	Digest           string                            `json:"digest"`
}

// SealTaskCompletionSettlement validates the terminal accounting receipt.
func SealTaskCompletionSettlement(s TaskCompletionSettlement) (TaskCompletionSettlement, error) {
	_, settlementDigest, settlementErr := s.Settlement.Request.Freeze()
	if s.SchemaVersion != 1 || s.AssignmentID.IsNil() || s.AssignmentID.Prefix() != PrefixAssignment || !evidence.ValidDigest(s.CompletionDigest) || s.RunID.IsNil() || s.RunID.Prefix() != id.PrefixAgentRun || settlementErr != nil || s.Settlement.Request.Receipt != s.CompletionDigest || s.Settlement.Request.Usage.Known != 0 || s.Settlement.Request.Minimum == nil || settlementDigest != s.Settlement.Digest || s.Settlement.At.IsZero() || s.ObservedAt.IsZero() || s.Settlement.At.After(s.ObservedAt) {
		return TaskCompletionSettlement{}, ErrInvalidReview
	}
	if s.Abandonment == nil {
		if s.RunRevision < 1 || !evidence.ValidDigest(s.RunDigest) {
			return TaskCompletionSettlement{}, ErrInvalidReview
		}
	} else {
		request, _, err := s.Abandonment.Freeze()
		if err != nil || s.RunRevision != 0 || s.RunDigest != "" || request.Completion.Digest != s.CompletionDigest || request.Completion.Request.AssignmentID != s.AssignmentID || request.Completion.Request.RunID != s.RunID || request.Completion.Reservation.Request.ID != s.Settlement.Request.ReservationID || request.Completion.Reservation.RequestDigest != s.Settlement.Request.RequestDigest || s.ObservedAt.Before(request.Proof.ObservedAt) || !s.ObservedAt.Before(request.Proof.ExpiresAt) {
			return TaskCompletionSettlement{}, ErrInvalidReview
		}
		s.Abandonment = &request
	}
	s.Digest = ""
	data, err := json.Marshal(s)
	if err != nil || len(data) > MaxHandoffBytes+128<<10 {
		return TaskCompletionSettlement{}, ErrInvalidReview
	}
	var frozen TaskCompletionSettlement
	if json.Unmarshal(data, &frozen) != nil {
		return TaskCompletionSettlement{}, ErrInvalidReview
	}
	frozen.Digest = evidence.Digest(data)
	return frozen, nil
}

// Validate checks integrity, not current authority or review success.
func (s TaskCompletionSettlement) Validate() error {
	sealed, err := SealTaskCompletionSettlement(s)
	if err != nil || sealed.Digest != s.Digest {
		return ErrInvalidReview
	}
	return nil
}

// TaskCompletionSettlementStore closes exact terminal reviewer runs under the
// plan/run/account transaction. An existing abandonment is returned unchanged.
// Nil means the reserved run is absent or pending;
// no inference of nonexecution is made. Hosts authorize and fence every call.
type TaskCompletionSettlementStore interface {
	SettleTaskCompletion(context.Context, id.ID, string) (*TaskCompletionSettlement, error)
	GetTaskCompletionSettlement(context.Context, id.ID) (*TaskCompletionSettlement, error)
}
