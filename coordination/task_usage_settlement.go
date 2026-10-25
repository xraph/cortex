package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

var (
	ErrInvalidTaskSettlement  = cortex.ErrInvalidTaskSettlement
	ErrTaskSettlementConflict = cortex.ErrTaskSettlementConflict
)

// TaskBillingProof identifies host-authenticated billing evidence. Its content
// must bind the scope, claim, seal, usage and closure digests, allocation,
// original provider accounts and routing, verifier attempts and pricing.
// Known charges must be final. Unknown charges,
// including delayed delivery, must be bounded by the retained settlement charge.
// Other work must add no unaccounted charges to refunded dimensions. Hashes and
// references alone establish none of these facts.
type TaskBillingProof struct {
	Issuer          string    `json:"issuer"`
	Revision        string    `json:"revision"`
	ProofRef        string    `json:"proof_ref"`
	ProofDigest     string    `json:"proof_digest"`
	PricingRevision string    `json:"pricing_revision,omitempty"`
	ObservedAt      time.Time `json:"observed_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// TaskUsageSettlementRequest binds final accounting to accepted or terminated work,
// closed admission, one complete usage observation and the original allocation.
type TaskUsageSettlementRequest struct {
	SchemaVersion     int              `json:"schema_version"`
	Usage             TaskUsageRequest `json:"usage"`
	UsageDigest       string           `json:"usage_digest"`
	ClosureDigest     string           `json:"closure_digest"`
	ReservationID     id.ID            `json:"reservation_id"`
	ReservationDigest string           `json:"reservation_digest"`
	Billing           TaskBillingProof `json:"billing"`
	Actor             string           `json:"actor"`
	Reason            string           `json:"reason"`
}

// Freeze detaches settlement input without authenticating its proof or actor.
func (r TaskUsageSettlementRequest) Freeze() (TaskUsageSettlementRequest, string, error) {
	usage, err := r.Usage.Freeze()
	p := r.Billing
	if err != nil || r.SchemaVersion != 1 || !evidence.ValidDigest(r.UsageDigest) || !evidence.ValidDigest(r.ClosureDigest) || r.ReservationID.IsNil() || r.ReservationID.Prefix() != execution.PrefixReservation || !evidence.ValidDigest(r.ReservationDigest) || !handoffText(r.Actor, 512) || !handoffText(r.Reason, 4096) || !handoffText(p.Issuer, 512) || !handoffText(p.Revision, 256) || !handoffText(p.ProofRef, 4096) || !evidence.ValidDigest(p.ProofDigest) || (p.PricingRevision != "" && !handoffText(p.PricingRevision, 256)) || p.ObservedAt.IsZero() || p.ObservedAt.Before(r.Usage.Seal.At) || !p.ExpiresAt.After(p.ObservedAt) || p.ExpiresAt.Sub(p.ObservedAt) > MaxClaimDuration {
		return TaskUsageSettlementRequest{}, "", ErrInvalidTaskSettlement
	}
	r.Usage = usage
	data, err := json.Marshal(r)
	if err != nil || len(data) > 32<<10 {
		return TaskUsageSettlementRequest{}, "", ErrInvalidTaskSettlement
	}
	return r, evidence.Digest(data), nil
}

// PrepareTaskUsageSettlement derives charges from complete retained evidence.
// Callers must authenticate Billing and hold current authority and storage
// guards. This function does not establish those guarantees or write accounting.
func PrepareTaskUsageSettlement(r TaskUsageSettlementRequest, snapshot TaskUsageSnapshot, closed TaskUsageClosure, reservation execution.Reservation, at time.Time) (execution.Settlement, error) {
	_, digest, err := r.Freeze()
	if err != nil {
		return execution.Settlement{}, err
	}
	if snapshot.Validate() != nil || closed.Validate() != nil || reservation.Validate() != nil {
		return execution.Settlement{}, ErrInvalidTaskSettlement
	}
	c, allocation := snapshot.Claim, reservation.Request
	if !closed.Request.MatchesClaim(c) || c.Token() != r.Usage.Claim || snapshot.Digest != r.UsageDigest || snapshot.Journal.Digest != r.Usage.SnapshotDigest || snapshot.Journal.Seal.RequestDigest != r.Usage.Seal.RequestDigest || !snapshot.Journal.Seal.At.Equal(r.Usage.Seal.At) || closed.Digest != r.ClosureDigest || closed.Request.Usage.Claim != c.Token() || closed.Request.Usage.Seal.RequestDigest != r.Usage.Seal.RequestDigest || !closed.Request.Usage.Seal.At.Equal(r.Usage.Seal.At) || r.Billing.ObservedAt.Before(closed.At) || r.Billing.ObservedAt.Before(c.At) || at.Before(r.Billing.ObservedAt) || !at.Before(r.Billing.ExpiresAt) || reservation.Settlement != nil || allocation.ID != r.ReservationID || reservation.RequestDigest != r.ReservationDigest || allocation.OwnerRef != c.ID.String() || allocation.Kind != execution.ReservationLeaf || allocation.PlanID != c.PlanID || allocation.PlanDigest != c.PlanDigest || allocation.TaskID != c.TaskID || allocation.Scope.Canonical() != c.Scope.Canonical() || allocation.PricingRevision != r.Billing.PricingRevision {
		return execution.Settlement{}, ErrTaskSettlementConflict
	}
	for _, entry := range snapshot.Journal.Calls {
		if entry.Call.Request.ClaimID != c.ID || entry.Call.Request.AgentID.String() != c.Worker.Agent || r.Billing.ObservedAt.Before(entry.Call.StartedAt) || r.Billing.ObservedAt.Before(entry.Call.CompletedAt) || (entry.Provider != nil && r.Billing.ObservedAt.Before(entry.Provider.ObservedAt)) {
			return execution.Settlement{}, ErrTaskSettlementConflict
		}
	}
	for _, job := range snapshot.Jobs {
		if r.Billing.ObservedAt.Before(job.UpdatedAt) || (allocation.MoneyBound && job.Request.EvaluationBudget != nil && job.Request.EvaluationBudget.PricingRevision != allocation.PricingRevision) {
			return execution.Settlement{}, ErrTaskSettlementConflict
		}
	}
	summary, err := SummarizeTaskUsage(snapshot)
	if err != nil {
		return execution.Settlement{}, err
	}
	if summary.OpenJobs != 0 {
		return execution.Settlement{}, ErrTaskSettlementConflict
	}
	if !allocation.MoneyBound {
		summary.Observed.USDMicros = 0
		summary.Known &^= execution.KnownMoney
		summary.Overflow &^= execution.KnownMoney
	}
	settlement := execution.Settlement{ReservationID: r.ReservationID, RequestDigest: r.ReservationDigest, Usage: execution.ReservationUsage{Known: summary.Known}, Receipt: "task-usage:" + digest, Overflow: summary.Overflow}
	if summary.Known&execution.KnownTokens != 0 {
		settlement.Usage.Actual.Tokens = summary.Observed.Tokens
	}
	if summary.Known&execution.KnownMoney != 0 {
		settlement.Usage.Actual.USDMicros = summary.Observed.USDMicros
	}
	if summary.Observed != (execution.Resources{}) {
		settlement.Minimum = &summary.Observed
	}
	settlement, _, err = settlement.Freeze()
	return settlement, err
}

// TaskUsageSettlement records both the guarded request and accounting receipt.
// Fresh is true only for a confirmed first write and is never persisted.
type TaskUsageSettlement struct {
	SchemaVersion int                         `json:"schema_version"`
	Request       TaskUsageSettlementRequest  `json:"request"`
	RequestDigest string                      `json:"request_digest"`
	Settlement    execution.SettlementReceipt `json:"settlement"`
	Digest        string                      `json:"digest"`
	Fresh         bool                        `json:"-"`
}

// SealTaskUsageSettlement validates and detaches a receipt without granting funds.
func SealTaskUsageSettlement(s TaskUsageSettlement) (TaskUsageSettlement, error) {
	r, digest, err := s.Request.Freeze()
	if err != nil || s.SchemaVersion != 1 || s.RequestDigest != digest {
		return TaskUsageSettlement{}, ErrInvalidTaskSettlement
	}
	settled, settledDigest, err := s.Settlement.Request.Freeze()
	if err != nil || settledDigest != s.Settlement.Digest || settled.ReservationID != r.ReservationID || settled.RequestDigest != r.ReservationDigest || settled.CloseBranch || settled.Receipt != "task-usage:"+digest || s.Settlement.At.Before(r.Billing.ObservedAt) || !s.Settlement.At.Before(r.Billing.ExpiresAt) || settled.Usage.Known & ^(execution.KnownTokens|execution.KnownMoney) != 0 || settled.Overflow & ^(execution.KnownTokens|execution.KnownMoney) != 0 {
		return TaskUsageSettlement{}, ErrInvalidTaskSettlement
	}
	if settled.Minimum != nil && (settled.Minimum.Steps != 0 || settled.Minimum.ToolCalls != 0 || settled.Minimum.FanOut != 0 || settled.Minimum.Replans != 0) {
		return TaskUsageSettlement{}, ErrInvalidTaskSettlement
	}
	if r.Billing.PricingRevision == "" && (settled.Usage.Known&execution.KnownMoney != 0 || settled.Overflow&execution.KnownMoney != 0 || (settled.Minimum != nil && settled.Minimum.USDMicros != 0)) {
		return TaskUsageSettlement{}, ErrInvalidTaskSettlement
	}
	s.Request, s.Settlement.Request, s.Digest = r, settled, ""
	data, err := json.Marshal(s)
	if err != nil {
		return TaskUsageSettlement{}, err
	}
	s.Digest = evidence.Digest(data)
	return s, nil
}

// Validate checks integrity and operation binding, not billing authenticity.
func (s TaskUsageSettlement) Validate() error {
	sealed, err := SealTaskUsageSettlement(s)
	if err != nil || sealed.Digest != s.Digest {
		return ErrInvalidTaskSettlement
	}
	return nil
}

// TaskUsageSettlementStore atomically compares current evidence and settles the
// original budgeted claim. Trusted callers must authenticate billing and hold
// current authority through the write. Get returns nil when no receipt exists.
type TaskUsageSettlementStore interface {
	SettleTaskUsage(context.Context, TaskUsageSettlementRequest) (TaskUsageSettlement, error)
	GetTaskUsageSettlement(context.Context, id.AgentRunID) (*TaskUsageSettlement, error)
}

// TaskUsageSettlementFence holds current host authority through one synchronous
// transaction callback. Resolve external billing proof before entering it.
type TaskUsageSettlementFence interface {
	WithTaskUsageSettlementFence(context.Context, TaskUsageSettlementRequest, func(TaskUsageSettlementRequest) error) error
}

// TaskUsageSettlementAuthorizer authenticates Actor and Billing for the full
// request, including historical retries. It must refuse unknown charges without
// an enforced upper bound and any unaccounted fees in refunded dimensions.
type TaskUsageSettlementAuthorizer interface {
	AuthorizeTaskUsageSettlement(context.Context, TaskUsageSettlementRequest) error
}

// TaskUsageSettler commits accounting under explicit host authority. Retry the
// identical request after uncertain acknowledgments; reads grant no authority.
type TaskUsageSettler interface {
	Settle(context.Context, TaskUsageSettlementRequest) (TaskUsageSettlement, error)
}

type guardedTaskUsageSettler struct {
	store      TaskUsageSettlementStore
	fence      TaskUsageSettlementFence
	authorizer TaskUsageSettlementAuthorizer
}

// NewTaskUsageSettler requires explicit billing authority and fencing ports.
func NewTaskUsageSettler(store TaskUsageSettlementStore, fence TaskUsageSettlementFence, authorizer TaskUsageSettlementAuthorizer) (TaskUsageSettler, error) {
	if hostport.IsNil(store) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidTaskSettlement
	}
	return &guardedTaskUsageSettler{store: store, fence: fence, authorizer: authorizer}, nil
}

func (s *guardedTaskUsageSettler) Settle(ctx context.Context, r TaskUsageSettlementRequest) (TaskUsageSettlement, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	guard := taskAuthority[TaskUsageSettlementRequest]{operation: "task usage settlement", invalid: ErrInvalidTaskSettlement, conflict: ErrTaskSettlementConflict, scope: func(r TaskUsageSettlementRequest) cortex.Scope { return r.Usage.Seal.Request.Scope }, fence: s.fence.WithTaskUsageSettlementFence, authorize: s.authorizer.AuthorizeTaskUsageSettlement}
	return withTaskAuthority(bounded, r, guard, func(tc context.Context, frozen TaskUsageSettlementRequest, digest string) (TaskUsageSettlement, error) {
		receipt, err := s.store.SettleTaskUsage(tc, frozen)
		if err != nil {
			return TaskUsageSettlement{}, err
		}
		if receipt.Validate() != nil || receipt.RequestDigest != digest {
			return TaskUsageSettlement{}, ErrTaskSettlementConflict
		}
		return SealTaskUsageSettlement(receipt)
	})
}
