package run

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// ContentProtected identifies historical reads that must use the guarded result
// endpoint for bytes. A saved requirement or result keeps protection active even
// when the current host has no reader configured.
func ContentProtected(r *Run, readerConfigured bool) bool {
	return r != nil && (readerConfigured || r.Provenance.VerificationRequired || r.ResultRef != "")
}

// PublicView preserves ordinary legacy reads and projects protected runs onto an
// explicit metadata allowlist. It never authorizes content or mutates the source.
// Trusted host code can still obtain the original through the engine/store APIs.
func PublicView(r *Run, readerConfigured bool) *Run {
	if !ContentProtected(r, readerConfigured) {
		return r
	}
	out := &Run{
		Entity: r.Entity, ID: r.ID, AgentID: r.AgentID, SessionID: r.SessionID,
		Revision: r.Revision, Scope: cortex.Scope{Levels: append([]cortex.Level(nil), r.Scope.Levels...)},
		State: r.State, VerificationStatus: r.VerificationStatus.Normalized(), StopReason: r.StopReason,
		Budget: r.Budget, Usage: r.Usage, StepCount: r.StepCount, TokensUsed: r.TokensUsed,
		StartedAt: publicTime(r.StartedAt), CompletedAt: publicTime(r.CompletedAt),
		Provenance: ExecutionProvenance{VerificationRequired: r.Provenance.VerificationRequired, PublicationStatus: r.Provenance.PublicationStatus},
		Metadata:   map[string]any{"content_withheld": true},
	}
	if resultID, err := id.ParseWithPrefix(r.ResultRef, verification.PrefixResult); err == nil {
		out.ResultRef = resultID.String()
	}
	return out
}

func publicTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
