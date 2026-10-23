package run

import (
	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/verification"
)

// ValidateExecutionInputs checks saved run identity against the immutable input
// artifact. History and the model selector are bound through the artifact and
// prepared digest; model and tool implementation pins also match provenance.
func (r *Run) ValidateExecutionInputs(inputs verification.ExecutionInputs) error {
	data, err := inputs.Encode()
	if err != nil || evidence.Digest(data) != r.Provenance.ExecutionInputsDigest || inputs.RunID != r.ID || inputs.AgentID != r.AgentID || inputs.SessionID != r.SessionID || inputs.Input != r.Input || inputs.PreparedDigest != r.Provenance.PreparedDigest || inputs.ModelRevision != r.Provenance.ModelRevision || len(inputs.Tools) != len(r.Provenance.Tools) {
		return cortex.ErrRunConflict
	}
	for i, tool := range inputs.Tools {
		if tool.Name != r.Provenance.Tools[i].Name || tool.Revision != r.Provenance.Tools[i].Revision {
			return cortex.ErrRunConflict
		}
	}
	return nil
}
