package run

import "github.com/xraph/cortex/safety"

// ExecutionPurpose is trusted host metadata. Model output cannot select it.
type ExecutionPurpose string

const (
	PurposeNormal                 ExecutionPurpose = "normal"
	PurposeEvaluation             ExecutionPurpose = "evaluation"
	PurposeOptimizationTrial      ExecutionPurpose = "optimization_trial"
	PurposeOptimizationController ExecutionPurpose = "optimization_controller"
)

// Revision binds a named source or tool to its frozen content or host revision.
type Revision struct {
	Name     string `json:"name"`
	Digest   string `json:"digest,omitempty"`
	Revision string `json:"revision,omitempty"`
}

// SourceSnapshot records a successful store capture. ID identifies this capture,
// not a reusable database revision or a model/tool implementation pin.
// External retrieval is not covered by a store transaction.
type SourceSnapshot struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Isolation string `json:"isolation"`
	Scope     string `json:"scope"`
}

// ExecutionProvenance identifies the inputs behind a run without copying prompt
// or evidence bytes into metadata. Empty fields mean unavailable, not verified.
type ExecutionProvenance struct {
	VerificationRequired bool   `json:"verification_required,omitempty"`
	VerificationIntentID string `json:"verification_intent_id,omitempty"`
	// SourceValidation distinguishes read-set checks from atomic store capture.
	// read_set_checked detects observed changes but is not an atomic snapshot.
	SourceSnapshot       SourceSnapshot           `json:"source_snapshot,omitzero"`
	SourceValidation     string                   `json:"source_validation,omitempty"`
	SourceReads          []Revision               `json:"source_reads,omitempty"`
	PublicationStatus    safety.PublicationStatus `json:"publication_status,omitempty"`
	SchemaVersion        int                      `json:"schema_version"`
	PreparedDigest       string                   `json:"prepared_digest"`
	DerivedFrom          string                   `json:"derived_from,omitempty"`
	Purpose              ExecutionPurpose         `json:"purpose"`
	ParentRunID          string                   `json:"parent_run_id,omitempty"`
	ExperimentID         string                   `json:"experiment_id,omitempty"`
	CandidateID          string                   `json:"candidate_id,omitempty"`
	EnvironmentReceiptID string                   `json:"environment_receipt_id,omitempty"`
	TrialID              string                   `json:"trial_id,omitempty"`
	HarnessID            string                   `json:"harness_id,omitempty"`
	HarnessVersion       string                   `json:"harness_version,omitempty"`
	AcceptanceRef        string                   `json:"acceptance_ref,omitempty"`
	ModelRevision        string                   `json:"model_revision,omitempty"`
	Sources              []Revision               `json:"sources,omitempty"`
	Tools                []Revision               `json:"tools,omitempty"`
	Diagnostics          []string                 `json:"diagnostics,omitempty"`
}
