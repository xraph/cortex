package verification_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestReportObservationRefsBindDynamicEvidence(t *testing.T) {
	_, report, now := fixture(t)
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: report.Binding.Scope, MediaType: "application/json", Version: "1", Producer: "supervisor/v1", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now.Add(time.Second), RetainUntil: now.Add(2 * time.Hour)}, Data: []byte(`{"check":"passed"}`)})
	require.NoError(t, err)
	report.Outcomes[0].ObservationRefs = []evidence.Ref{artifact.Ref}
	sealed, err := verification.SealReport(report)
	require.NoError(t, err)
	refs, err := verification.ReportObservationRefs(sealed)
	require.NoError(t, err)
	require.Equal(t, []evidence.Ref{artifact.Ref}, refs)
	require.Equal(t, verification.StatusStale, verification.CheckFreshness(sealed, sealed.Binding, now))
	require.Equal(t, verification.StatusPassed, verification.CheckFreshness(sealed, sealed.Binding, now.Add(2*time.Second)))
	for name, change := range map[string]func(*verification.Report){
		"cross scope":       func(r *verification.Report) { r.Outcomes[0].ObservationRefs[0].Scope.Levels[0].Value = "foreign" },
		"input masquerades": func(r *verification.Report) { r.Outcomes[0].ObservationRefs[0] = r.Binding.Candidate },
		"duplicate": func(r *verification.Report) {
			r.Outcomes[0].ObservationRefs = append(r.Outcomes[0].ObservationRefs, r.Outcomes[0].ObservationRefs[0])
		},
		"retention too short": func(r *verification.Report) { r.Outcomes[0].ObservationRefs[0].RetainUntil = now.Add(time.Minute) },
		"unsupported sensitivity": func(r *verification.Report) {
			r.Outcomes[0].ObservationRefs[0].Sensitivity = evidence.SensitivityPublic
		},
	} {
		t.Run(name, func(t *testing.T) {
			copyReport, copyErr := verification.SealReport(sealed)
			require.NoError(t, copyErr)
			change(&copyReport)
			require.Error(t, copyReport.Validate())
			_, copyErr = verification.SealReport(copyReport)
			require.Error(t, copyErr)
		})
	}
}
