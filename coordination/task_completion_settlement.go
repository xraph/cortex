package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// TaskCompletionSettlement closes reviewer accounting from an exact terminal run.
// Unknown provider usage stays charged. This receipt cannot accept a task, prove
// an absent run never executed, or authorize another reviewer attempt.
type TaskCompletionSettlement struct {
	SchemaVersion    int                         `json:"schema_version"`
	AssignmentID     id.ID                       `json:"assignment_id"`
	CompletionDigest string                      `json:"completion_digest"`
	RunID            id.AgentRunID               `json:"run_id"`
	RunRevision      int64                       `json:"run_revision"`
	RunDigest        string                      `json:"run_digest"`
	Settlement       execution.SettlementReceipt `json:"settlement"`
	ObservedAt       time.Time                   `json:"observed_at"`
	Digest           string                      `json:"digest"`
}

// SealTaskCompletionSettlement validates the terminal accounting receipt.
func SealTaskCompletionSettlement(s TaskCompletionSettlement) (TaskCompletionSettlement, error) {
	_, settlementDigest, settlementErr := s.Settlement.Request.Freeze()
	if s.SchemaVersion != 1 || s.AssignmentID.IsNil() || s.AssignmentID.Prefix() != PrefixAssignment || !evidence.ValidDigest(s.CompletionDigest) || s.RunID.IsNil() || s.RunID.Prefix() != id.PrefixAgentRun || s.RunRevision < 1 || !evidence.ValidDigest(s.RunDigest) || settlementErr != nil || s.Settlement.Request.Receipt != s.CompletionDigest || s.Settlement.Request.Usage.Known != 0 || s.Settlement.Request.Minimum == nil || settlementDigest != s.Settlement.Digest || s.Settlement.At.IsZero() || s.ObservedAt.IsZero() || s.Settlement.At.After(s.ObservedAt) {
		return TaskCompletionSettlement{}, ErrInvalidReview
	}
	s.Digest = ""
	data, err := json.Marshal(s)
	if err != nil || len(data) > 32<<10 {
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
// plan/run/account transaction. Nil means the reserved run is absent or pending;
// no inference of nonexecution is made. Hosts authorize and fence every call.
type TaskCompletionSettlementStore interface {
	SettleTaskCompletion(context.Context, id.ID, string) (*TaskCompletionSettlement, error)
	GetTaskCompletionSettlement(context.Context, id.ID) (*TaskCompletionSettlement, error)
}
