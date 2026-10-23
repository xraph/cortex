package assurance

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func (s *Repository) validateExecutionInputs(ctx context.Context, r *run.Run, binding verification.Binding) error {
	ref := binding.Context.ExecutionInputs
	if ref == nil {
		if r.Provenance.ExecutionInputsDigest != "" || r.Provenance.VerificationRequired {
			return cortex.ErrRunConflict
		}
		return nil // Legacy explicit verification jobs did not capture execution inputs.
	}
	if ref.SHA256 != r.Provenance.ExecutionInputsDigest {
		return cortex.ErrRunConflict
	}
	artifact, err := s.LoadEvidence(ctx, *ref)
	if err != nil {
		return err
	}
	inputs, err := verification.DecodeExecutionInputs(artifact.Data)
	if err != nil {
		return err
	}
	return r.ValidateExecutionInputs(inputs)
}
