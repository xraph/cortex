package state

import "github.com/xraph/cortex/optimization"

// Operator commands cannot carry worker evidence or skip lifecycle admission.
// This check runs after identical committed retries have been recognized.
func validateOperatorTransition(experiment optimization.Experiment, change optimization.Transition) error {
	if change.Retain != nil && change.CommandAction != optimization.ActionRetain {
		return optimization.ErrInvalidSpec
	}
	if change.CommandAction == "" {
		return nil
	}
	if change.Decision != nil || change.NextPhase != "" || change.Lease != (optimization.WorkLease{}) || len(change.Candidates)+len(change.Versions)+len(change.Trials)+len(change.TrialUpdates)+len(change.Evaluations)+len(change.Publications) > 0 {
		return optimization.ErrInvalidSpec
	}
	switch change.CommandAction {
	case optimization.ActionRetain:
		if change.Retain == nil || change.NextState != optimization.StateCompleted {
			return optimization.ErrInvalidSpec
		}
		if err := change.Retain.Validate(); err != nil {
			return err
		}
		if experiment.State != optimization.StateReviewRequired || experiment.Spec.Parent != nil || experiment.RetainedBaseline != nil {
			return optimization.ErrConflict
		}

	case optimization.ActionStart:
		if change.NextState != optimization.StateQueued {
			return optimization.ErrInvalidSpec
		}
		if experiment.State != optimization.StateDraft {
			return optimization.ErrConflict
		}
	case optimization.ActionResume:
		if change.NextState != optimization.StateQueued {
			return optimization.ErrInvalidSpec
		}
		if experiment.State != optimization.StatePaused {
			return optimization.ErrConflict
		}
	case optimization.ActionCancel:
		if change.NextState != optimization.StateCancelled {
			return optimization.ErrInvalidSpec
		}
	default:
		return optimization.ErrInvalidSpec
	}
	return nil
}
