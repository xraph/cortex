package optimization

// TrialArtifactRefs includes evidence from every attempt, including superseded executions.
func TrialArtifactRefs(trial Trial) []ArtifactRef {
	var refs []ArtifactRef
	if trial.Execution != nil {
		refs = append(refs, trial.Execution.Artifacts...)
	}
	for _, attempt := range trial.Attempts {
		if attempt.Execution != nil {
			refs = append(refs, attempt.Execution.Artifacts...)
		}
	}
	return refs
}
