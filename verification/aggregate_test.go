package verification_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func fixture(t *testing.T) (verification.Contract, verification.Report, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}
	c, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "produce correct output", Criteria: []verification.Criterion{
		{ID: "tests", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "v1", Parameters: verification.Parameters{ManifestRevision: "tests-v1"}, MaxAge: time.Hour},
		{ID: "quality", Kind: verification.KindSemantic, Verifier: "judge", VerifierRevision: "v2", Parameters: verification.Parameters{ManifestRevision: "rubric-v1"}, MaxAge: time.Hour},
	}})
	require.NoError(t, err)
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "host", Sensitivity: evidence.SensitivityInternal, CreatedAt: now.Add(-time.Minute), RetainUntil: now.Add(time.Hour)}, Data: []byte("result")})
	require.NoError(t, err)
	r, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: verification.Binding{Scope: scope, RunID: id.NewAgentRunID(), ContractDigest: c.Digest, Candidate: a.Ref, Context: verification.ContextSnapshot{PrincipalRef: "user:1", AuthorityRevision: "grant:1", PreparedDigest: evidence.Digest([]byte("prepared")), PolicyRevision: "policy:1", EnvironmentRevision: "env:1", DatasetRevision: "data:1"}}, CheckedAt: now, ExpiresAt: now.Add(time.Hour), Outcomes: []verification.Outcome{{CriterionID: "tests", Verifier: "tests", VerifierRevision: "v1", Status: verification.StatusPassed}}})
	require.NoError(t, err)
	return c, r, now
}

func TestRequiredCriterionMissingIsInconclusive(t *testing.T) {
	c, _, now := fixture(t)
	require.Equal(t, verification.StatusInconclusive, verification.Aggregate(c, nil, now).Status)
}
func TestOptionalScoreCannotOverrideRequiredFailure(t *testing.T) {
	c, r, now := fixture(t)
	r.Outcomes[0].Status = verification.StatusFailed
	score := 1.0
	r.Outcomes = append(r.Outcomes, verification.Outcome{CriterionID: "quality", Verifier: "judge", VerifierRevision: "v2", Status: verification.StatusPassed, Score: &score})
	r, err := verification.SealReport(r)
	require.NoError(t, err)
	require.Equal(t, verification.StatusFailed, verification.Aggregate(c, []verification.Report{r}, now).Status)
}
func TestContextRevisionMakesReportStale(t *testing.T) {
	_, r, now := fixture(t)
	current := r.Binding
	current.Context.DatasetRevision = "data:2"
	require.Equal(t, verification.StatusStale, verification.CheckFreshness(r, current, now))
}
func TestRevocationInvalidatesReport(t *testing.T) {
	_, r, now := fixture(t)
	current := r.Binding
	current.Context.AuthorityRevision = "grant:revoked"
	require.Equal(t, verification.StatusStale, verification.CheckFreshness(r, current, now))
}
func TestLegacyCompletedRemainsUnchecked(t *testing.T) {
	require.Equal(t, verification.StatusUnchecked, verification.Status("").Normalized())
}
func TestAggregateBindsCurrentEvidenceAndVerifier(t *testing.T) {
	c, r, now := fixture(t)
	require.Equal(t, verification.StatusPassed, verification.Aggregate(c, []verification.Report{r}, now).Status)
	for name, mutate := range map[string]func(*verification.Report){
		"expired":                 func(r *verification.Report) { r.ExpiresAt = now },
		"future":                  func(r *verification.Report) { r.CheckedAt = now.Add(time.Minute) },
		"wrong verifier revision": func(r *verification.Report) { r.Outcomes[0].VerifierRevision = "other" },
		"unknown criterion":       func(r *verification.Report) { r.Outcomes[0].CriterionID = "other" },
		"missing evidence":        func(r *verification.Report) { r.Binding.Candidate.SHA256 = evidence.Digest([]byte("different")) },
	} {
		t.Run(name, func(t *testing.T) {
			// Keep all identities equal before changing the selected field.
			altered := r
			altered.Outcomes = append([]verification.Outcome(nil), r.Outcomes...)
			mutate(&altered)
			decision := verification.Aggregate(c, []verification.Report{altered}, now)
			require.NotEqual(t, verification.StatusPassed, decision.Status)
		})
	}
	require.Equal(t, verification.StatusStale, verification.CheckFreshness(r, r.Binding, r.ExpiresAt))
}
func TestContractsRejectAmbiguousChecks(t *testing.T) {
	c, r, _ := fixture(t)
	c.Criteria = append(c.Criteria, c.Criteria[0])
	_, err := verification.SealContract(c)
	require.Error(t, err)
	score := math.NaN()
	r.Outcomes[0].Score = &score
	_, err = verification.SealReport(r)
	require.Error(t, err)
}

func TestValidReportsCannotBeMixedAcrossRunsOrRevisions(t *testing.T) {
	c, r, now := fixture(t)
	another := r
	another.ID = id.New(verification.PrefixReport)
	another.Binding.RunID = id.NewAgentRunID()
	another.Outcomes = []verification.Outcome{{CriterionID: "quality", Verifier: "judge", VerifierRevision: "v2", Status: verification.StatusPassed}}
	another, err := verification.SealReport(another)
	require.NoError(t, err)
	require.Equal(t, verification.StatusInconclusive, verification.Aggregate(c, []verification.Report{r, another}, now).Status)
	r.Outcomes[0].VerifierRevision = "unregistered"
	r, err = verification.SealReport(r)
	require.NoError(t, err)
	require.Equal(t, verification.StatusInconclusive, verification.Aggregate(c, []verification.Report{r}, now).Status)
}

func TestFreshnessUsesCurrentTimeAndRequiredEvidence(t *testing.T) {
	c, r, now := fixture(t)
	require.Equal(t, verification.StatusStale, verification.Aggregate(c, []verification.Report{r}, now.Add(time.Hour)).Status)
	require.Equal(t, verification.StatusStale, verification.Aggregate(c, []verification.Report{r}, now.Add(-time.Second)).Status)
	c.Criteria[0].MinEvidence = 1
	c, err := verification.SealContract(c)
	require.NoError(t, err)
	r.Binding.ContractDigest = c.Digest
	r, err = verification.SealReport(r)
	require.NoError(t, err)
	require.Equal(t, verification.StatusInconclusive, verification.Aggregate(c, []verification.Report{r}, now).Status)
	r.Outcomes[0].EvidenceIDs = []id.ID{r.Binding.Candidate.ID}
	r, err = verification.SealReport(r)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, verification.Aggregate(c, []verification.Report{r}, now).Status)
}

func TestHumanReviewRequiresExactEvidenceAndUnexpiredApproval(t *testing.T) {
	c, r, now := fixture(t)
	c.Criteria[0].Kind = verification.KindHuman
	c, err := verification.SealContract(c)
	require.NoError(t, err)
	r.Binding.ContractDigest = c.Digest
	r, err = verification.SealReport(r)
	require.NoError(t, err)
	require.Equal(t, verification.StatusInconclusive, verification.Aggregate(c, []verification.Report{r}, now).Status)
	digest, err := r.Binding.Digest()
	require.NoError(t, err)
	r.Outcomes[0].Human = &verification.HumanVerdict{Actor: "user:reviewer", DecisionRevision: "review:1", EvidenceDigest: digest, ExpiresAt: now.Add(time.Minute)}
	r, err = verification.SealReport(r)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, verification.Aggregate(c, []verification.Report{r}, now).Status)
	require.Equal(t, verification.StatusStale, verification.Aggregate(c, []verification.Report{r}, now.Add(time.Minute)).Status)
	r.Binding.Candidate.SHA256 = evidence.Digest([]byte("swapped"))
	_, err = verification.SealReport(r)
	require.Error(t, err)
}

func TestCanonicalRecordsRejectUnsupportedSchemaAndDetachInputs(t *testing.T) {
	c, r, _ := fixture(t)
	sealed, err := verification.SealContract(c)
	require.NoError(t, err)
	c.Criteria[0].ID = "changed"
	require.NoError(t, sealed.Validate())
	report, err := verification.SealReport(r)
	require.NoError(t, err)
	r.Binding.Scope.Levels[0].Value = "changed"
	require.NoError(t, report.Validate())
	sealed.SchemaVersion = 2
	_, err = verification.SealContract(sealed)
	require.Error(t, err)
	report.SchemaVersion = 2
	_, err = verification.SealReport(report)
	require.Error(t, err)
}

func TestCanonicalRecordsRejectLossyText(t *testing.T) {
	c, r, _ := fixture(t)
	c.Objective = string([]byte{0xff})
	_, err := verification.SealContract(c)
	require.Error(t, err)
	r.Outcomes[0].Reason = string([]byte{0xff})
	_, err = verification.SealReport(r)
	require.Error(t, err)
}

func TestSourceObservationWindowCanExpireBeforeReport(t *testing.T) {
	c, r, now := fixture(t)
	c.Criteria[0].MaxSourceAge = time.Minute
	c, err := verification.SealContract(c)
	require.NoError(t, err)
	r.Binding.ContractDigest = c.Digest
	r.Binding.Context.Sources = []verification.Source{{Resource: "source:1", Revision: "1", Digest: evidence.Digest([]byte("source")), RetrievedAt: now.Add(-2 * time.Minute)}}
	r, err = verification.SealReport(r)
	require.NoError(t, err)
	require.Equal(t, verification.StatusStale, verification.Aggregate(c, []verification.Report{r}, now).Status)
}
