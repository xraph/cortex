package operator

import (
	"context"

	opt "github.com/xraph/cortex/optimization"
)

// RetainInput identifies the measured decision and the reviewer's reason.
// The command key comes separately from the transport envelope.
type RetainInput struct {
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expected_revision"`
	DecisionID       string `json:"decision_id"`
	Reason           string `json:"reason"`
}

// Retain closes a pending review while preserving measured evidence and routes.
func (c Commands) Retain(ctx context.Context, in RetainInput, key string) (opt.Experiment, error) {
	if c.Optimizer == nil {
		return opt.Experiment{}, opt.ErrUnavailable
	}
	experimentID, err := opt.ParseID(in.ID, opt.PrefixExperiment)
	if err != nil {
		return opt.Experiment{}, err
	}
	decisionID, err := opt.ParseID(in.DecisionID, opt.PrefixDecision)
	if err != nil {
		return opt.Experiment{}, err
	}
	return c.Optimizer.RetainCommand(ctx, opt.RetainRequest{
		CommandRequest: opt.CommandRequest{ExperimentID: experimentID, ExpectedRevision: in.ExpectedRevision, IdempotencyKey: key},
		RetainDecision: opt.RetainDecision{DecisionID: decisionID, Reason: in.Reason},
	})
}
