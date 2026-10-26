package coordination

import (
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/planning"
)

// PreparedRunAbandonment is authenticated permanent quiescence of an admission
// whose run may never have been created. Missing rows alone are never proof.
type PreparedRunAbandonment struct {
	AdmissionDigest string    `json:"admission_digest"`
	Issuer          string    `json:"issuer"`
	ProofRef        string    `json:"proof_ref"`
	ProofDigest     string    `json:"proof_digest"`
	ObservedAt      time.Time `json:"observed_at"`
	ExpiresAt       time.Time `json:"expires_at"`
}

func (p PreparedRunAbandonment) validate() error {
	if !evidence.ValidDigest(p.AdmissionDigest) || !handoffText(p.Issuer, 512) || !handoffText(p.ProofRef, 4096) || !evidence.ValidDigest(p.ProofDigest) || p.ObservedAt.IsZero() || !p.ExpiresAt.After(p.ObservedAt) || p.ExpiresAt.Sub(p.ObservedAt) > MaxClaimDuration {
		return planning.ErrInvalid
	}
	return nil
}
