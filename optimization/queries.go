package optimization

import "github.com/xraph/cortex/id"

// PageQuery bounds a resource read. A cursor binds the scope, collection and filters.
type PageQuery struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor,omitempty"`
}

// TrialQuery filters one experiment's immutable trial identities and current state.
type TrialQuery struct {
	PageQuery
	VersionID id.ID      `json:"version_id,omitempty"`
	Split     Split      `json:"split,omitempty"`
	State     TrialState `json:"state,omitempty"`
}

// VersionQuery filters immutable harness versions within the exact scope.
type VersionQuery struct {
	PageQuery
	AgentID id.AgentID  `json:"agent_id,omitempty"`
	Kind    HarnessKind `json:"kind,omitempty"`
}

// LessonQuery can omit lessons whose frozen sources are no longer eligible.
type LessonQuery struct {
	PageQuery
	Domain        string        `json:"domain,omitempty"`
	EvidenceClass EvidenceClass `json:"evidence_class,omitempty"`
	EligibleOnly  bool          `json:"eligible_only,omitempty"`
}

// PublicationQuery filters local delivery records. PendingOnly omits acknowledged,
// superseded and terminally failed entries; workers still need a fenced claim.
type PublicationQuery struct {
	PageQuery
	ExperimentID id.ID                `json:"experiment_id,omitempty"`
	LessonID     id.ID                `json:"lesson_id,omitempty"`
	Operation    PublicationOperation `json:"operation,omitempty"`
	PendingOnly  bool                 `json:"pending_only,omitempty"`
}
