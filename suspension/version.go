package suspension

import (
	"context"
	"errors"

	"github.com/xraph/cortex/id"
)

// ErrVersionClaimUnavailable means storage cannot atomically claim a pinned pause.
var ErrVersionClaimUnavailable = errors.New("cortex: suspension version claim unavailable")

// ClaimVersion identifies the pause and run revision validated before resume.
// A later suspension of the same run must never satisfy this claim.
type ClaimVersion struct {
	RunID        id.AgentRunID
	SuspensionID id.SuspensionID
	RunRevision  int64
}

// VersionedStore fences the suspension identity and run revision in the same
// transaction as the paused-to-running transition. Failed or ambiguous commits
// return no suspension. Retrying cannot claim a later pause in the same run.
// This primitive does not retain a resume payload or grant recovery authority.
type VersionedStore interface {
	ClaimSuspensionVersion(context.Context, ClaimVersion) (*Suspension, error)
}
