package optimization

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// RetainDecision binds an operator's reason to the measured decision being reviewed.
// It does not replace that decision or change a production activation.
type RetainDecision struct {
	DecisionID id.ID  `json:"decision_id"`
	Reason     string `json:"reason"`
}

// Validate checks bounded review content and the decision identity kind.
func (r RetainDecision) Validate() error {
	if _, err := ParseID(r.DecisionID.String(), PrefixDecision); err != nil {
		return err
	}
	if !validReviewReason(r.Reason) {
		return ErrInvalidSpec
	}
	return nil
}

// RetainRequest supplies the reviewed decision, revision and durable command key.
type RetainRequest struct {
	CommandRequest
	RetainDecision
}

// RetainedBaseline is a transaction-stamped human review, separate from the
// immutable measured decision. It closes adoption of this experiment's candidate.
type RetainedBaseline struct {
	DecisionID id.ID       `json:"decision_id"`
	Baseline   VersionRef  `json:"baseline"`
	Actor      IdentityRef `json:"actor"`
	Reason     string      `json:"reason"`
	CreatedAt  time.Time   `json:"created_at"`
}

// RetainCommand closes a pending review without changing any production route.
// It remains available without the original requester's current grants or old
// artifact content. A currently active adoption of this decision must first be
// explicitly deactivated or replaced through the activation workflow.
func (o *Optimizer) RetainCommand(ctx context.Context, request RetainRequest) (Experiment, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return Experiment{}, err
	}
	if err := request.Validate(); err != nil {
		return Experiment{}, err
	}
	change, err := operatorTransition(request.CommandRequest, ActionRetain, StateCompleted)
	if err != nil {
		return Experiment{}, err
	}
	review := request.RetainDecision
	change.Retain = &review
	return o.executeCommand(ctx, change, ActionRetain)
}
