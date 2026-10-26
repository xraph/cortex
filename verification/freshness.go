package verification

import "time"

// CheckFreshness checks integrity, expiry and a trusted current binding.
// Passed here means the binding is current, not that the checks passed.
// Access revocation must be reflected by the host's current authority revision
// or denied before this function is called. It performs no external I/O.
func CheckFreshness(report Report, current Binding, at time.Time) Status {
	if at.IsZero() || report.Validate() != nil || current.Validate() != nil {
		return StatusInconclusive
	}
	if report.CheckedAt.After(at) || !report.ExpiresAt.After(at) {
		return StatusStale
	}
	oldDigest, err := report.Binding.Digest()
	if err != nil {
		return StatusInconclusive
	}
	currentDigest, err := current.Digest()
	if err != nil {
		return StatusInconclusive
	}
	if oldDigest != currentDigest {
		return StatusStale
	}
	refs := append([]time.Time{report.Binding.Candidate.RetainUntil}, evidenceExpiries(report.Binding)...)
	for _, expiry := range refs {
		if !expiry.After(at) {
			return StatusStale
		}
	}
	for _, outcome := range report.Outcomes {
		for _, ref := range outcome.ObservationRefs {
			if ref.CreatedAt.After(at) || !ref.RetainUntil.After(at) {
				return StatusStale
			}
		}
		for _, observation := range outcome.StateObservations {
			if observation.FinishedAt.After(at) || !observation.ExpiresAt.After(at) {
				return StatusStale
			}
		}
		if outcome.Human != nil && !outcome.Human.ExpiresAt.After(at) {
			return StatusStale
		}
	}
	return StatusPassed
}

func evidenceExpiries(binding Binding) []time.Time {
	expiries := make([]time.Time, 0, len(binding.Evidence))
	for _, ref := range binding.Evidence {
		expiries = append(expiries, ref.RetainUntil)
	}
	return expiries
}
