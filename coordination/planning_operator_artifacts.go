package coordination

import (
	"context"
	"sort"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
)

// MaxPlanningArtifactResources bounds a complete operator board projection.
const MaxPlanningArtifactResources = MaxPlanBoardTransitions / MaxBoardTransitions

// PlanningArtifactReadResolver selects exact declared writable resources whose
// metadata the caller may inspect. ArtifactBoard independently authorizes reads.
// Return complete histories (After zero, Limit MaxBoardTransitions), at most
// MaxPlanningArtifactResources resources. Omitted resources remain private.
type PlanningArtifactReadResolver interface {
	ResolvePlanningArtifactReads(context.Context, planning.Plan) ([]ArtifactBoardRead, error)
}

// PlanningArtifactCandidate references historical accepted output. It neither
// certifies current verification nor grants access to the referenced content.
type PlanningArtifactCandidate struct {
	ID       id.ID        `json:"id"`
	TaskID   id.ID        `json:"task_id"`
	Owner    Identity     `json:"owner"`
	Artifact evidence.Ref `json:"artifact"`
	At       time.Time    `json:"at"`
}

// PlanningArtifactView reports a durable head and unresolved ownership conflicts.
// Head pins are required when explicitly resolving a selected conflict.
type PlanningArtifactView struct {
	Resource     planning.ResourceGrant      `json:"resource"`
	HeadRevision int64                       `json:"head_revision"`
	HeadDigest   string                      `json:"head_digest,omitempty"`
	Head         *PlanningArtifactCandidate  `json:"head,omitempty"`
	Conflicts    []PlanningArtifactCandidate `json:"conflicts"`
}

func (o *PlanningOperator) artifactViews(ctx context.Context, plan planning.Plan) ([]PlanningArtifactView, error) {
	if hostport.IsNil(o.config.ArtifactReads) {
		return []PlanningArtifactView{}, nil
	}
	detached, err := planning.Seal(plan, planning.Limits{})
	if err != nil {
		return nil, err
	}
	selected, err := o.config.ArtifactReads.ResolvePlanningArtifactReads(ctx, detached)
	if err != nil {
		return nil, err
	}
	if len(selected) > MaxPlanningArtifactResources {
		return nil, planning.ErrInvalid
	}
	declared := make(map[planning.ResourceGrant]bool, len(plan.Resources))
	for _, resource := range plan.Resources {
		if resource.Access == planning.AccessWrite {
			declared[resource] = true
		}
	}
	reads := make([]ArtifactBoardRead, 0, len(selected))
	for _, read := range selected {
		frozen, _, freezeErr := read.Freeze()
		if freezeErr != nil || frozen.PlanID != plan.ID || frozen.Scope.Canonical() != plan.Scope.Canonical() || !declared[frozen.Resource] || frozen.After != 0 || frozen.Limit != MaxBoardTransitions {
			return nil, planning.ErrInvalid
		}
		delete(declared, frozen.Resource)
		reads = append(reads, frozen)
	}
	sort.Slice(reads, func(i, j int) bool { return reads[i].Resource.Resource < reads[j].Resource.Resource })
	views := make([]PlanningArtifactView, 0, len(reads))
	for i, read := range reads {
		entries, readErr := o.config.Artifacts.Read(ctx, read)
		if readErr != nil {
			return nil, readErr
		}
		view, projectErr := planningArtifactView(read.Resource, entries)
		if projectErr != nil {
			return nil, projectErr
		}
		views = append(views, view)
		reads[i].After = int64(len(entries))
		reads[i].Limit = 1
	}
	// Recheck every selected resource after hydration. A changed history cannot
	// be returned as a complete head projection, even if its earlier page was valid.
	for _, read := range reads {
		tail, readErr := o.config.Artifacts.Read(ctx, read)
		if readErr != nil {
			return nil, readErr
		}
		if len(tail) != 0 {
			return nil, planning.ErrConflict
		}
	}
	return views, nil
}

func planningArtifactView(resource planning.ResourceGrant, entries []ArtifactBoardEntry) (PlanningArtifactView, error) {
	view := PlanningArtifactView{Resource: resource, Conflicts: []PlanningArtifactCandidate{}}
	pending := map[id.ID]PlanningArtifactCandidate{}
	seen := map[id.ID]bool{}
	for _, entry := range entries {
		if seen[entry.Request.ID] || entry.PreviousDigest != view.HeadDigest {
			return PlanningArtifactView{}, planning.ErrConflict
		}
		seen[entry.Request.ID] = true
		candidate := PlanningArtifactCandidate{ID: entry.Request.ID, TaskID: entry.TaskID, Owner: entry.Owner, Artifact: entry.Artifact, At: entry.At}
		if entry.Conflict {
			if entry.HeadRevision != view.HeadRevision {
				return PlanningArtifactView{}, planning.ErrConflict
			}
			pending[candidate.ID] = candidate
			continue
		}
		if entry.HeadRevision != view.HeadRevision+1 {
			return PlanningArtifactView{}, planning.ErrConflict
		}
		if !entry.Request.Resolve.IsNil() {
			if _, exists := pending[entry.Request.Resolve]; !exists {
				return PlanningArtifactView{}, planning.ErrConflict
			}
			delete(pending, entry.Request.Resolve)
		}
		view.Head = &candidate
		view.HeadRevision = entry.HeadRevision
		view.HeadDigest = entry.Digest
	}
	// Preserve retained transition order so a refresh does not reorder conflicts.
	for _, entry := range entries {
		if candidate, exists := pending[entry.Request.ID]; exists {
			view.Conflicts = append(view.Conflicts, candidate)
		}
	}
	return view, nil
}
