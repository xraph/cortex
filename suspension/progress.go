package suspension

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ProgressResult is the checked result of one tool call that finished in
// the current step, as the model will see it.
type ProgressResult struct {
	CallID  string `json:"call_id"`
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
	// Outcome is "completed", "failed" or "denied".
	Outcome string `json:"outcome"`
}

// EndingKind names the end a claim was made to write.
type EndingKind string

const (
	// EndingCancel is a cancel: the run ends cancelled.
	EndingCancel EndingKind = "cancel"
	// EndingExpire is an expiry: the run fails because nobody answered its
	// pause in time.
	EndingExpire EndingKind = "expire"
)

// Ending is the end a claim was made to write, recorded with the claim so
// that a worker that dies before writing it does not leave the run to
// continue as if it had never been asked to end.
type Ending struct {
	Kind EndingKind `json:"kind"`
	// Reason is what the run's error says, for an expiry.
	Reason string `json:"reason,omitempty"`
}

// Progress is a running run's state at its last durable boundary, saved by
// the worker that holds the run's lease. A replica that reclaims the run
// after that worker dies continues from Cont, treats every call in Started
// as already handed to its handler, and reuses the results in Done instead
// of running those calls again.
type Progress struct {
	RunID id.AgentRunID `json:"run_id"`
	// Scope is the scope the snapshot is stored under. SaveProgress sets it
	// from the context.
	Scope cortex.Scope `json:"scope"`
	// Owner is the lease owner that wrote the snapshot.
	Owner string `json:"owner"`
	// Cont is the run's state at the last durable boundary. Stores encode
	// it the way they encode a suspension's continuation, so Cont.Prepared
	// comes back byte for byte.
	Cont Continuation `json:"continuation"`
	// Started lists the calls that are not safe to retry and were handed
	// to their handler since Cont.
	Started []PendingCall `json:"started,omitempty"`
	// Done lists the calls of the current step that finished.
	Done []ProgressResult `json:"done,omitempty"`
	// Ending, when set, is the end a claim was made to write (a cancel or
	// an expiry) and did not get to. A worker that reclaims the run
	// finishes that end instead of continuing the run.
	Ending *Ending `json:"ending,omitempty"`
	// UpdatedAt is when the snapshot was saved. SaveProgress sets it.
	UpdatedAt time.Time `json:"updated_at"`
}

// ProgressStore is an optional capability of suspension stores. Every
// method works at the caller's exact scope.
type ProgressStore interface {
	// SaveProgress upserts the run's snapshot at the exact scope, only while the run is running
	// under p.Owner's lease; ErrRunLeaseLost otherwise and nothing is written. An empty Owner
	// never matches, not even a run that holds no lease. On success it sets p.Scope and
	// p.UpdatedAt to what it stored.
	SaveProgress(ctx context.Context, p *Progress) error
	// GetProgress reads the run's snapshot. ErrProgressNotFound when there is none at the exact
	// scope.
	GetProgress(ctx context.Context, runID id.AgentRunID) (*Progress, error)
	// DeleteProgress removes the run's snapshot at the exact scope. It returns nil when there is
	// none.
	DeleteProgress(ctx context.Context, runID id.AgentRunID) error
}
