package planning

import (
	"math"
	"reflect"
	"slices"
	"sort"

	"github.com/xraph/cortex/id"
)

// Revision is a proposed replacement graph plus host-authenticated audit data.
// ApplyRevision assigns chain metadata; model-provided revision metadata is ignored.
type Revision struct {
	Plan   Plan
	Actor  string
	Reason string
}

// ApplyRevision validates a detached replacement against the current grant and
// contract. The durable store must still compare expected under its write lock.
// It does not transfer claims or reuse accepted evidence across revisions.
func ApplyRevision(current Plan, proposal Revision, expected int64, limits Limits) (Plan, error) {
	if expected != current.Revision {
		return Plan{}, ErrConflict
	}
	if err := Validate(current, limits); err != nil {
		return Plan{}, err
	}
	if current.Revision == math.MaxInt64 {
		return Plan{}, invalid("revision exhausted")
	}
	next := proposal.Plan
	for _, previous := range current.Tasks {
		if previous.Review == nil {
			continue
		}
		found := false
		for _, task := range next.Tasks {
			if task.ID == previous.ID {
				found = true
				if !reflect.DeepEqual(previous.Review, task.Review) || !slices.Equal(previous.Criteria, task.Criteria) {
					return Plan{}, invalid("revision replaces host review policy")
				}
			}
		}
		if !found {
			return Plan{}, invalid("revision removes reviewed task")
		}
	}

	if next.ID != current.ID || next.Scope.Canonical() != current.Scope.Canonical() || next.Contract.Digest != current.Contract.Digest || next.Objective != current.Objective || next.Origin != current.Origin {
		return Plan{}, invalid("plan identity or acceptance changed")
	}
	if !budgetWithin(next.Budget, current.Budget) || !resourcesWithin(next.Resources, current.Resources) {
		return Plan{}, invalid("revision widens authority or budget")
	}
	owners := make(map[string]bool, len(current.Owners))
	for _, owner := range current.Owners {
		owners[owner] = true
	}
	for _, owner := range next.Owners {
		if !owners[owner] {
			return Plan{}, invalid("revision widens owner roster")
		}
	}
	if !effectsWithin(next.Effects, current.Effects) || !sourcesEqual(next.Sources, current.Sources) {
		return Plan{}, invalid("revision replaces host effect or source pins")
	}
	next.Revision = current.Revision + 1
	next.PreviousDigest = current.Digest
	next.Actor = proposal.Actor
	next.Reason = proposal.Reason
	next.AffectedTasks = nil
	// Validate before comparing definitions or traversing dependency edges.
	sealed, err := Seal(next, limits)
	if err != nil {
		return Plan{}, err
	}
	sealed.AffectedTasks = affectedTasks(current, sealed)
	return Seal(sealed, limits)
}

func affectedTasks(current, next Plan) []id.ID {
	old := make(map[id.ID]Task, len(current.Tasks))
	changed := make(map[id.ID]bool)
	for _, task := range current.Tasks {
		old[task.ID] = task
	}
	for _, task := range next.Tasks {
		previous, exists := old[task.ID]
		if !exists || !tasksEqual(previous, task) {
			changed[task.ID] = true
		}
		delete(old, task.ID)
	}
	for taskID := range old {
		changed[taskID] = true
	}
	// Global grant changes require every task to revalidate its assumptions.
	if !current.Budget.Equal(next.Budget) || !slices.Equal(current.Resources, next.Resources) || !slices.Equal(current.Owners, next.Owners) || !slices.Equal(current.Effects, next.Effects) {
		for _, task := range next.Tasks {
			changed[task.ID] = true
		}
	}
	for again := true; again; {
		again = false
		for _, task := range next.Tasks {
			if changed[task.ID] {
				continue
			}
			for _, dependency := range task.DependsOn {
				if changed[dependency] {
					changed[task.ID] = true
					again = true
					break
				}
			}
		}
	}
	out := make([]id.ID, 0, len(changed))
	for taskID := range changed {
		out = append(out, taskID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func tasksEqual(a, b Task) bool {
	return reflect.DeepEqual(a.Review, b.Review) && a.ID == b.ID && a.Instruction == b.Instruction && a.Owner == b.Owner && a.Budget.Equal(b.Budget) && a.Output == b.Output && slices.Equal(a.DependsOn, b.DependsOn) && slices.Equal(a.Criteria, b.Criteria) && slices.Equal(a.Resources, b.Resources) && slices.Equal(a.Effects, b.Effects) && slices.Equal(a.Sources, b.Sources)
}
