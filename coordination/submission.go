package coordination

import (
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// Submission pins a retained verification job and its exact execution binding.
// It contains no peer verdict. Storage must load the job, fenced run and bytes;
// structurally valid references alone cannot admit a submission.
type Submission struct {
	SchemaVersion    int           `json:"schema_version"`
	JobID            id.ID         `json:"job_id"`
	JobRequestDigest string        `json:"job_request_digest"`
	RunID            id.AgentRunID `json:"run_id"`
	RunRevision      int64         `json:"run_revision"`
	BindingDigest    string        `json:"binding_digest"`
}

// Validate checks the pins without authenticating the producer or reading bytes.
func (s Submission) Validate() error {
	if s.SchemaVersion != 1 || s.JobID.IsNil() || s.JobID.Prefix() != verification.PrefixJob || !evidence.ValidDigest(s.JobRequestDigest) || s.RunID.IsNil() || s.RunID.Prefix() != id.PrefixAgentRun || s.RunRevision < 1 || !evidence.ValidDigest(s.BindingDigest) {
		return ErrInvalidAssignment
	}
	return nil
}
