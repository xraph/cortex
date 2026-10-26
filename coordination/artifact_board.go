package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

const (
	// PrefixBoardTransition identifies an immutable publication or resolution.
	PrefixBoardTransition id.Prefix = "btrans"
	// MaxBoardTransitions bounds all candidates and resolutions for one resource.
	MaxBoardTransitions = 128
	// MaxPlanBoardTransitions bounds retained transitions within one plan.
	MaxPlanBoardTransitions = 4096
	// MaxBoardRequestBytes bounds retained binding and dependency metadata.
	MaxBoardRequestBytes = 64 << 10
)

// ArtifactBoardRequest publishes an accepted output or explicitly resolves a
// retained conflict. Resource is an exact declared write grant. Resolve names
// the conflicting transition whose candidate is selected, never a content patch.
// Current and Dependencies are freshly resolved under the host authority fence.
type ArtifactBoardRequest struct {
	SchemaVersion    int                    `json:"schema_version"`
	ID               id.ID                  `json:"id"`
	Scope            cortex.Scope           `json:"scope"`
	PlanID           id.ID                  `json:"plan_id"`
	PlanDigest       string                 `json:"plan_digest"`
	Resource         planning.ResourceGrant `json:"resource"`
	AcceptanceID     id.ID                  `json:"acceptance_id"`
	AcceptanceDigest string                 `json:"acceptance_digest"`
	Current          verification.Binding   `json:"current"`
	Dependencies     []AcceptedDependency   `json:"dependencies,omitempty"`
	ExpectedRevision int64                  `json:"expected_revision"`
	ExpectedDigest   string                 `json:"expected_digest,omitempty"`
	Resolve          id.ID                  `json:"resolve,omitempty"`
	Actor            string                 `json:"actor"`
	Reason           string                 `json:"reason"`
}

// Freeze validates and detaches bounded input without establishing authority.
func (r ArtifactBoardRequest) Freeze() (ArtifactBoardRequest, string, error) {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixBoardTransition || cortex.ValidateScope(r.Scope) != nil || r.PlanID.IsNil() || r.PlanID.Prefix() != planning.PrefixPlan || !evidence.ValidDigest(r.PlanDigest) || r.Resource.Access != planning.AccessWrite || !handoffText(r.Resource.Resource, 4096) || !handoffText(r.Resource.Revision, 512) || r.AcceptanceID.IsNil() || r.AcceptanceID.Prefix() != PrefixTaskAcceptance || !evidence.ValidDigest(r.AcceptanceDigest) || r.Current.Validate() != nil || r.Current.Scope.Canonical() != r.Scope.Canonical() || r.ExpectedRevision < 0 || r.ExpectedRevision > MaxBoardTransitions || (r.ExpectedRevision == 0 && r.ExpectedDigest != "") || (r.ExpectedRevision > 0 && !evidence.ValidDigest(r.ExpectedDigest)) || (!r.Resolve.IsNil() && r.Resolve.Prefix() != PrefixBoardTransition) || !handoffText(r.Actor, 512) || !handoffText(r.Reason, 4096) || !validAcceptedDependencies(r.Dependencies, r.Scope, r.AcceptanceID) {
		return ArtifactBoardRequest{}, "", planning.ErrInvalid
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxBoardRequestBytes {
		return ArtifactBoardRequest{}, "", planning.ErrInvalid
	}
	var detached ArtifactBoardRequest
	if json.Unmarshal(data, &detached) != nil {
		return ArtifactBoardRequest{}, "", planning.ErrInvalid
	}
	return detached, evidence.Digest(data), nil
}

// ArtifactBoardEntry retains a candidate even when its expected head lost a
// race. A conflict never advances HeadRevision. PreviousDigest names the head
// observed at admission. Only explicit resolution can transfer its owner.
type ArtifactBoardEntry struct {
	SchemaVersion  int                  `json:"schema_version"`
	Request        ArtifactBoardRequest `json:"request"`
	RequestDigest  string               `json:"request_digest"`
	PlanRevision   int64                `json:"plan_revision"`
	Sequence       int64                `json:"sequence"`
	HeadRevision   int64                `json:"head_revision"`
	PreviousDigest string               `json:"previous_digest,omitempty"`
	Conflict       bool                 `json:"conflict"`
	Owner          Identity             `json:"owner"`
	TaskID         id.ID                `json:"task_id"`
	Artifact       evidence.Ref         `json:"artifact"`
	At             time.Time            `json:"at"`
	Digest         string               `json:"digest"`
}

// SealArtifactBoardEntry checks retained structure and detaches references.
func SealArtifactBoardEntry(e ArtifactBoardEntry) (ArtifactBoardEntry, error) {
	req, digest, err := e.Request.Freeze()
	if err != nil || e.SchemaVersion != 1 || digest != e.RequestDigest || e.PlanRevision < 1 || e.Sequence < 1 || e.Sequence > MaxBoardTransitions || e.HeadRevision < 0 || e.HeadRevision > e.Sequence || (!e.Conflict && e.HeadRevision < 1) || (e.Conflict && !req.Resolve.IsNil()) || (e.PreviousDigest != "" && !evidence.ValidDigest(e.PreviousDigest)) || !e.Owner.valid() || e.TaskID.IsNil() || e.TaskID.Prefix() != planning.PrefixTask || e.Artifact.Validate() != nil || e.Artifact.Scope.Canonical() != req.Scope.Canonical() || e.Artifact.ID != req.Current.Candidate.ID || e.Artifact.SHA256 != req.Current.Candidate.SHA256 || e.At.IsZero() {
		return ArtifactBoardEntry{}, planning.ErrInvalid
	}
	e.Request = req
	e.Digest = ""
	data, err := json.Marshal(e)
	if err != nil || len(data) > 128<<10 {
		return ArtifactBoardEntry{}, planning.ErrInvalid
	}
	var detached ArtifactBoardEntry
	if json.Unmarshal(data, &detached) != nil {
		return ArtifactBoardEntry{}, planning.ErrInvalid
	}
	detached.Digest = evidence.Digest(data)
	return detached, nil
}

// Validate verifies integrity, not current evidence freshness or authority.
func (e ArtifactBoardEntry) Validate() error {
	sealed, err := SealArtifactBoardEntry(e)
	if err != nil || sealed.Digest != e.Digest {
		return planning.ErrInvalid
	}
	return nil
}

// ArtifactBoardRead pins an exact resource and finite history page. Metadata
// remains private; historical references do not grant content access or certify
// that a formerly accepted output is still valid under current source revisions.
type ArtifactBoardRead struct {
	Scope    cortex.Scope           `json:"scope"`
	PlanID   id.ID                  `json:"plan_id"`
	Resource planning.ResourceGrant `json:"resource"`
	After    int64                  `json:"after"`
	Limit    int                    `json:"limit"`
}

// Freeze checks metadata read bounds before any storage access.
func (r ArtifactBoardRead) Freeze() (ArtifactBoardRead, string, error) {
	if cortex.ValidateScope(r.Scope) != nil || r.PlanID.IsNil() || r.PlanID.Prefix() != planning.PrefixPlan || r.Resource.Access != planning.AccessWrite || !handoffText(r.Resource.Resource, 4096) || !handoffText(r.Resource.Revision, 512) || r.After < 0 || r.After > MaxBoardTransitions || r.Limit < 1 || r.Limit > MaxBoardTransitions {
		return ArtifactBoardRead{}, "", planning.ErrInvalid
	}
	data, err := json.Marshal(r)
	if err != nil {
		return ArtifactBoardRead{}, "", err
	}
	var frozen ArtifactBoardRead
	if json.Unmarshal(data, &frozen) != nil {
		return ArtifactBoardRead{}, "", planning.ErrInvalid
	}
	return frozen, evidence.Digest(data), nil
}

// ArtifactBoardStore is trusted atomic storage. Public callers use ArtifactBoard
// so historical retries and reads still authenticate the current caller.
type ArtifactBoardStore interface {
	WriteArtifactBoard(context.Context, ArtifactBoardRequest) (ArtifactBoardEntry, error)
	ReadArtifactBoard(context.Context, ArtifactBoardRead) ([]ArtifactBoardEntry, error)
}
