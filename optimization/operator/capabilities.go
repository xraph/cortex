package operator

import (
	"context"

	opt "github.com/xraph/cortex/optimization"
)

// CapabilitiesInput optionally selects a frozen experiment for readiness checks.
type CapabilitiesInput struct {
	ExperimentID string `json:"experiment_id,omitempty" query:"experiment_id" optional:"true"`
}

// Capabilities reads current integration checks without starting external work.
func (r Reader) Capabilities(ctx context.Context, in CapabilitiesInput) (opt.CapabilitySnapshot, error) {
	return read(r, func(o *opt.Optimizer) (opt.CapabilitySnapshot, error) {
		key, err := optionalID(in.ExperimentID, opt.PrefixExperiment)
		if err != nil {
			return opt.CapabilitySnapshot{}, err
		}
		return o.Capabilities(ctx, key)
	})
}
