package optimization

import "github.com/xraph/cortex/id"

// TrialArtifactRefs includes evidence from every attempt, including superseded executions.
func TrialArtifactRefs(trial Trial) []ArtifactRef {
	var refs []ArtifactRef
	if trial.Execution != nil {
		refs = append(refs, ExecutionArtifactRefs(*trial.Execution)...)
	}
	for _, attempt := range trial.Attempts {
		if attempt.Execution != nil {
			refs = append(refs, ExecutionArtifactRefs(*attempt.Execution)...)
		}
	}
	return refs
}

// ExecutionArtifactRefs includes captured child artifacts and parent evidence.
func ExecutionArtifactRefs(record ExecutionRecord) []ArtifactRef {
	refs := append([]ArtifactRef(nil), record.Artifacts...)
	if record.Orchestration != nil {
		for _, child := range record.Orchestration.Children {
			if child.Execution != nil {
				refs = append(refs, child.Execution.Artifacts...)
			}
		}
	}
	return refs
}

// ExecutionRunIDs includes the team and every original reserved child identity.
func ExecutionRunIDs(record ExecutionRecord) []id.ID {
	keys := []id.ID{record.ExecutionID()}
	if record.Orchestration != nil {
		for _, child := range record.Orchestration.Children {
			keys = append(keys, child.Reservation.Request.TaskID)
		}
	}
	return keys
}
