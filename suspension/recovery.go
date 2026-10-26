package suspension

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ResumeExpiry proves an admitted continuation expired before execution started.
// It grants no execution or retry authority and does not release reservations.
type ResumeExpiry struct {
	SchemaVersion int          `json:"schema_version"`
	Version       ClaimVersion `json:"version"`
	Scope         cortex.Scope `json:"scope"`
	RequestDigest string       `json:"request_digest"`
	EventID       id.ID        `json:"event_id"`
	RunRevision   int64        `json:"run_revision"`
	ExpiredAt     time.Time    `json:"expired_at"`
	Fresh         bool         `json:"-"`
}

// ResumeRecoveryStore retains exact-scope recovery evidence independently of
// the live pause. LatestResumeAdmission orders by suspension ID. Expiry and
// execution start must share the run lock; an existing start refuses expiry.
// Hosts check current authority before every operation.
type ResumeRecoveryStore interface {
	LatestResumeAdmission(context.Context, id.AgentRunID) (ResumeAdmission, error)
	ExpireResumeAdmission(context.Context, id.AgentRunID, id.SuspensionID, string) (ResumeExpiry, error)
	GetResumeExpiry(context.Context, id.AgentRunID, id.SuspensionID) (ResumeExpiry, error)
}
