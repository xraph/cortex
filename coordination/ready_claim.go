package coordination

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

const PrefixTaskClaimedEvent id.Prefix = "tclaimed"

// ReadyTaskClaimRequest reserves an attempt after reloading every accepted
// prerequisite. The host must fence current grants and external revisions for
// Dependencies and authenticate the worker before calling storage.
type ReadyTaskClaimRequest struct {
	SchemaVersion int                  `json:"schema_version"`
	Acquisition   BudgetedClaimRequest `json:"acquisition"`
	Dependencies  []AcceptedDependency `json:"dependencies,omitempty"`
}

// Freeze checks and detaches a bounded request. Caller reports cannot replace
// the verification material retained by the store.
func (r ReadyTaskClaimRequest) Freeze() (ReadyTaskClaimRequest, string, error) {
	acquisition, _, err := r.Acquisition.Freeze()
	if err != nil || r.SchemaVersion != 1 || !validAcceptedDependencies(r.Dependencies, acquisition.Reservation.Scope, id.ID{}) {
		return ReadyTaskClaimRequest{}, "", ErrInvalidClaim
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 1<<20 {
		return ReadyTaskClaimRequest{}, "", ErrInvalidClaim
	}
	var frozen ReadyTaskClaimRequest
	if json.Unmarshal(data, &frozen) != nil {
		return ReadyTaskClaimRequest{}, "", ErrInvalidClaim
	}
	return frozen, evidence.Digest(data), nil
}

// TaskClaimOutbox retains metadata for authorized event delivery. It contains no
// public stream sequence, artifact content or permission to dispatch.
type TaskClaimOutbox struct {
	EventID id.ID  `json:"event_id"`
	Type    string `json:"type"`
	ClaimID id.ID  `json:"claim_id"`
	PlanID  id.ID  `json:"plan_id"`
	TaskID  id.ID  `json:"task_id"`
}

// ReadyTaskClaim records the original readiness admission. Fresh is true only
// when this call confirms the first commit. Acquisition is always historical;
// the host rechecks current claims, grants, budgets and evidence at dispatch.
type ReadyTaskClaim struct {
	RequestDigest string          `json:"request_digest"`
	Acquisition   BudgetedClaim   `json:"acquisition"`
	Outbox        TaskClaimOutbox `json:"outbox"`
	Fresh         bool            `json:"-"`
}

// ReadyTaskClaimStore checks prerequisites, acquires ownership, reserves shared
// capacity and retains an event intent atomically. It never invokes an agent.
type ReadyTaskClaimStore interface {
	ClaimReadyTask(context.Context, ReadyTaskClaimRequest) (ReadyTaskClaim, error)
	GetReadyTaskClaim(context.Context, id.ID) (ReadyTaskClaim, error)
}

func validAcceptedDependencies(deps []AcceptedDependency, scope cortex.Scope, self id.ID) bool {
	if len(deps) > 255 {
		return false
	}
	seen := make(map[id.ID]bool, len(deps))
	for _, dep := range deps {
		if dep.AcceptanceID.IsNil() || dep.AcceptanceID.Prefix() != PrefixTaskAcceptance || dep.AcceptanceID == self || seen[dep.AcceptanceID] || !evidence.ValidDigest(dep.Digest) || dep.Current.Validate() != nil || dep.Current.Scope.Canonical() != scope.Canonical() {
			return false
		}
		seen[dep.AcceptanceID] = true
	}
	return true
}
