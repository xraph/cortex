package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/planning"
)

// CurrentTaskAcceptanceRequest pins a retained acceptance and freshly authorized
// bindings for its full prerequisite chain. References never grant access.
type CurrentTaskAcceptanceRequest struct {
	Accepted     AcceptedDependency   `json:"accepted"`
	Dependencies []AcceptedDependency `json:"dependencies,omitempty"`
}

// Validate bounds the evidence closure before storage or external resolution.
func (r CurrentTaskAcceptanceRequest) Validate() error {
	scope := r.Accepted.Current.Scope
	if !validAcceptedDependencies([]AcceptedDependency{r.Accepted}, scope, r.Accepted.Current.RunID) || !validAcceptedDependencies(r.Dependencies, scope, r.Accepted.AcceptanceID) {
		return ErrInvalidTaskAcceptance
	}
	return nil
}

// CurrentTaskAcceptance is an observation of current acceptance, never a lease.
// ValidUntil includes reviewer and prerequisite evidence retention bounds.
type CurrentTaskAcceptance struct {
	State      planning.TaskState `json:"state"`
	ObservedAt time.Time          `json:"observed_at"`
	ValidUntil time.Time          `json:"valid_until"`
}

// CurrentTaskAcceptanceStore reuses transactional admission validation for an
// operator read, including late contradictions and changed assignment state.
// The host must fence current grants and external source revisions for all inputs.
type CurrentTaskAcceptanceStore interface {
	ReadCurrentTaskAcceptance(context.Context, CurrentTaskAcceptanceRequest) (CurrentTaskAcceptance, error)
}
