// Package operator shares typed request adapters between HTTP and dashboard
// transports. Every read retains the optimizer's current caller and scope checks.
package operator

import opt "github.com/xraph/cortex/optimization"

// PageInput carries the opaque continuation returned by the preceding page.
type PageInput struct {
	Limit  int    `json:"limit,omitempty" query:"limit" optional:"true"`
	Cursor string `json:"cursor,omitempty" query:"cursor" optional:"true"`
}

func (p PageInput) query() opt.PageQuery { return opt.PageQuery{Limit: p.Limit, Cursor: p.Cursor} }

// IDInput identifies one resource in the caller's exact scope.
type IDInput struct {
	ID string `json:"id" path:"id"`
}

// ExperimentPageInput selects a bounded child collection of one experiment.
type ExperimentPageInput struct {
	PageInput
	ID string `json:"id" path:"id"`
}

// ChildInput binds a child detail to its original parent experiment.
type ChildInput struct {
	ExperimentID string `json:"experiment_id" path:"experiment_id"`
	ID           string `json:"id" path:"id"`
}

// ExperimentsInput filters the experiment collection.
type ExperimentsInput struct {
	PageInput
	AgentID string    `json:"agent_id,omitempty" query:"agent_id" optional:"true"`
	State   opt.State `json:"state,omitempty" query:"state" optional:"true"`
}

// TrialsInput selects original attempts for one experiment.
type TrialsInput struct {
	PageInput
	ID        string         `json:"id" path:"id"`
	VersionID string         `json:"version_id,omitempty" query:"version_id" optional:"true"`
	Split     opt.Split      `json:"split,omitempty" query:"split" optional:"true"`
	State     opt.TrialState `json:"state,omitempty" query:"state" optional:"true"`
}

// VersionsInput filters immutable versions by owner and purpose.
type VersionsInput struct {
	PageInput
	AgentID string          `json:"agent_id,omitempty" query:"agent_id" optional:"true"`
	Kind    opt.HarnessKind `json:"kind,omitempty" query:"kind" optional:"true"`
}

// LessonsInput filters locally retained learning records.
type LessonsInput struct {
	PageInput
	Domain        string            `json:"domain,omitempty" query:"domain" optional:"true"`
	EvidenceClass opt.EvidenceClass `json:"evidence_class,omitempty" query:"evidence_class" optional:"true"`
	EligibleOnly  bool              `json:"eligible_only,omitempty" query:"eligible_only" optional:"true"`
}

// PublicationsInput filters durable delivery records independently of remote indexes.
type PublicationsInput struct {
	PageInput
	ExperimentID string                   `json:"experiment_id,omitempty" query:"experiment_id" optional:"true"`
	LessonID     string                   `json:"lesson_id,omitempty" query:"lesson_id" optional:"true"`
	Operation    opt.PublicationOperation `json:"operation,omitempty" query:"operation" optional:"true"`
	PendingOnly  bool                     `json:"pending_only,omitempty" query:"pending_only" optional:"true"`
}

// ActivationsInput identifies the route whose immutable history is requested.
type ActivationsInput struct {
	PageInput
	AgentID string          `json:"agent_id" query:"agent_id"`
	Channel string          `json:"channel,omitempty" query:"channel" optional:"true"`
	Kind    opt.HarnessKind `json:"kind,omitempty" query:"kind" optional:"true"`
}
