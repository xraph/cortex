package coordination

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// TaskRecoveryProof identifies host-authenticated, permanent quiescence of the
// original worker incarnation and all its tools. Its signed/attested content must
// bind the full claim, seal and snapshot, authenticate the original provider and
// effect receipts, and prove delayed delivery cannot produce further effects.
// Shape and hashes alone prove none of this. ExpiresAt bounds the adoption window;
// permanent quiescence cannot be withdrawn after adoption. It grants no retry.
type TaskRecoveryProof struct {
	SchemaVersion  int                   `json:"schema_version"`
	ClaimDigest    string                `json:"claim_digest"`
	Seal           execution.RunCallSeal `json:"seal"`
	SnapshotDigest string                `json:"snapshot_digest"`
	Issuer         string                `json:"issuer"`
	Revision       string                `json:"revision"`
	ProofRef       string                `json:"proof_ref"`
	ProofDigest    string                `json:"proof_digest"`
	ObservedAt     time.Time             `json:"observed_at"`
	ExpiresAt      time.Time             `json:"expires_at"`
}

// Freeze validates and detaches proof metadata without authenticating its issuer.
func (p TaskRecoveryProof) Freeze() (TaskRecoveryProof, string, error) {
	if p.SchemaVersion != 1 || !evidence.ValidDigest(p.ClaimDigest) || p.Seal.Validate() != nil || !evidence.ValidDigest(p.SnapshotDigest) || !handoffText(p.Issuer, 512) || !handoffText(p.Revision, 256) || !handoffText(p.ProofRef, 4096) || !evidence.ValidDigest(p.ProofDigest) || p.ObservedAt.IsZero() || p.ObservedAt.Before(p.Seal.At) || !p.ExpiresAt.After(p.ObservedAt) || p.ExpiresAt.Sub(p.ObservedAt) > MaxClaimDuration {
		return TaskRecoveryProof{}, "", ErrInvalidTaskAcceptance
	}
	data, err := json.Marshal(p)
	if err != nil || len(data) > 32<<10 {
		return TaskRecoveryProof{}, "", ErrInvalidTaskAcceptance
	}
	var frozen TaskRecoveryProof
	if json.Unmarshal(data, &frozen) != nil {
		return TaskRecoveryProof{}, "", ErrInvalidTaskAcceptance
	}
	return frozen, evidence.Digest(data), nil
}

func (p TaskRecoveryProof) validateClaim(c Claim, at time.Time) error {
	if _, _, err := p.Freeze(); err != nil {
		return err
	}
	if p.ClaimDigest != c.Digest || p.Seal.Request.RunID != c.RunID || p.Seal.Request.Scope.Canonical() != c.Scope.Canonical() || p.ObservedAt.Before(c.At) || at.Before(p.ObservedAt) || !at.Before(p.ExpiresAt) {
		return ErrTaskAcceptanceConflict
	}
	return nil
}

// AdoptRecoveredClaim records a proof-bound recovery transition without renewing
// the expired lease. Storage must first validate current task and journal evidence.
func AdoptRecoveredClaim(c Claim, token ClaimToken, acceptanceID id.ID, proof TaskRecoveryProof, at time.Time) (Claim, error) {
	if err := proof.validateClaim(c, at); err != nil {
		return Claim{}, err
	}
	_, digest, err := proof.Freeze()
	if err != nil {
		return Claim{}, err
	}
	return completeRecoveredClaim(c, token, acceptanceID, digest, at)
}

func completeRecoveredClaim(c Claim, token ClaimToken, acceptanceID id.ID, digest string, at time.Time) (Claim, error) {
	if c.Validate() != nil || acceptanceID.IsNil() || acceptanceID.Prefix() != PrefixTaskAcceptance || !evidence.ValidDigest(digest) || at.IsZero() || at.Before(c.At) {
		return Claim{}, ErrInvalidClaim
	}
	if c.State != ClaimRecovering || c.Token() != token || c.Revision == math.MaxInt64 || !at.Before(c.Deadline) {
		return Claim{}, ErrClaimConflict
	}
	c = nextClaim(c, at)
	c.State = ClaimCompleted
	c.AcceptanceID = &acceptanceID
	c.RecoveryDigest = digest
	return sealClaim(c)
}

// ValidateTaskRecoverySnapshot checks terminality and original invocation
// bindings. The caller must hold current host authority, authenticate the proof,
// and keep the journal locked through the adoption transaction.
func ValidateTaskRecoverySnapshot(proof TaskRecoveryProof, snapshot execution.RunRecoverySnapshot, c Claim, h Handoff, current verification.Binding, at time.Time) error {
	if c.Validate() != nil || c.State != ClaimRecovering || h.Validate() != nil || current.Validate() != nil || snapshot.Validate() != nil {
		return ErrInvalidTaskAcceptance
	}
	if err := proof.validateClaim(c, at); err != nil {
		return err
	}
	if snapshot.Digest != proof.SnapshotDigest || snapshot.Seal.RequestDigest != proof.Seal.RequestDigest || !snapshot.Seal.At.Equal(proof.Seal.At) || h.Scope.Canonical() != c.Scope.Canonical() || h.PlanID != c.PlanID || h.PlanDigest != c.PlanDigest || h.PlanRevision != c.PlanRevision || h.GrantRef != c.GrantRef || h.TaskID != c.TaskID || h.Receiver != c.Worker || h.Fence != c.Fence || current.RunID != c.RunID || current.Scope.Canonical() != c.Scope.Canonical() || current.Context.PrincipalRef != c.Worker.Principal {
		return ErrTaskAcceptanceConflict
	}
	for _, entry := range snapshot.Calls {
		r := entry.Call.Request
		if r.ClaimID != c.ID || r.AgentID.String() != c.Worker.Agent || r.ParentRunID != h.ParentRunID || r.AssignmentDigest != h.Digest || r.PreparedDigest != current.Context.PreparedDigest || r.Deadline.After(c.ExpiresAt) || entry.Call.StartedAt.Before(c.AcquiredAt) || proof.ObservedAt.Before(entry.Call.StartedAt) || (!entry.Call.CompletedAt.IsZero() && proof.ObservedAt.Before(entry.Call.CompletedAt)) {
			return ErrTaskAcceptanceConflict
		}
		if r.Kind != execution.RunCallTool {
			if entry.Provider == nil || entry.Provider.ValidateObservation(entry.Call) != nil || proof.ObservedAt.Before(entry.Provider.ObservedAt) {
				return ErrTaskAcceptanceConflict
			}
		}
	}
	for _, effect := range snapshot.Effects {
		// A legacy unbound operation needs separate adjudication; it cannot be
		// silently attributed to this claim because it shares the run identifier.
		if effect.Request.RunCall == nil || effect.Receipt == nil {
			return ErrTaskAcceptanceConflict
		}
	}
	return nil
}

// TaskRecoveryStore atomically adopts a verified result from a recovering claim.
// Only trusted host code may call it after authenticating Recovery and fencing
// current grants/revisions. It preserves reservations and never invokes work.
type TaskRecoveryStore interface {
	AdoptTaskResult(context.Context, TaskAcceptanceRequest) (TaskAcceptance, error)
}
