package verification

import (
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// ReportObservationRefs returns detached output evidence bound by the report
// digest. These refs are produced during evaluation, never added to its original
// input binding. Callers must hydrate them before treating the report as usable.
func ReportObservationRefs(r Report) ([]evidence.Ref, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	refs, err := collectObservationRefs(r)
	if err != nil {
		return nil, err
	}
	for i := range refs {
		refs[i].Scope.Levels = append(refs[i].Scope.Levels[:0:0], refs[i].Scope.Levels...)
	}
	return refs, nil
}

func collectObservationRefs(r Report) ([]evidence.Ref, error) {
	seen := map[id.ID]bool{r.Binding.Candidate.ID: true}
	for _, ref := range r.Binding.Evidence {
		seen[ref.ID] = true
	}
	var refs []evidence.Ref
	remaining := int64(4 << 20)
	for _, outcome := range r.Outcomes {
		if len(outcome.ObservationRefs) > 16 {
			return nil, ErrInvalid
		}
		for _, ref := range outcome.ObservationRefs {
			if ref.Validate() != nil || ref.Scope.Canonical() != r.Binding.Scope.Canonical() || ref.Sensitivity != evidence.SensitivityRestricted || ref.Bytes > 1<<20 || ref.Bytes > remaining || seen[ref.ID] || !ref.CreatedAt.Before(r.ExpiresAt) || ref.RetainUntil.Before(r.ExpiresAt) || len(refs) >= 256 {
				return nil, ErrInvalid
			}
			seen[ref.ID] = true
			remaining -= ref.Bytes
			refs = append(refs, ref)
		}
	}
	return refs, nil
}
