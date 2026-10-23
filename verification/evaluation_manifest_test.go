package verification_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func evaluationManifestFixture(t *testing.T) (verification.EvaluationManifest, verification.TestRequest, time.Time) {
	t.Helper()
	contract, report, now := fixture(t)
	binding := report.Binding
	refs := make([]evidence.Ref, 3)
	for i, name := range []string{"tests", "fixtures", "schema"} {
		artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: binding.Scope, MediaType: "application/json", Version: "1", Producer: "host-grading", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now.Add(-time.Minute), RetainUntil: now.Add(time.Hour)}, Data: []byte(name)})
		require.NoError(t, err)
		refs[i] = artifact.Ref
	}
	binding.Evidence = refs
	binding.Context.Sources = []verification.Source{{Resource: "fixture:one", Revision: "1", Digest: refs[1].SHA256, RetrievedAt: now.Add(-time.Minute)}}
	m := verification.EvaluationManifest{SchemaVersion: 1, Revision: contract.Criteria[0].Parameters.ManifestRevision, Binding: binding, Criterion: contract.Criteria[0], TestBundle: refs[0], FixtureDataset: refs[1], ResultSchema: refs[2], SupervisorRevision: evidence.Digest([]byte("supervisor")), RequiredChecks: []string{"required-behavior", "tenant-identity"}, StateProbes: binding.Context.Sources}
	return m, verification.TestRequest{CheckRequest: verification.CheckRequest{Criterion: contract.Criteria[0], Binding: binding, Deadline: now.Add(time.Minute)}}, now
}

func TestEvaluationManifestDetachesAndDetectsMutation(t *testing.T) {
	original, req, now := evaluationManifestFixture(t)
	sealed, err := verification.SealEvaluationManifest(original)
	require.NoError(t, err)
	require.True(t, evidence.ValidDigest(sealed.Digest))
	require.NoError(t, sealed.Validate())
	require.NoError(t, sealed.ValidateRequest(req, now))
	original.RequiredChecks[0] = "changed"
	original.Binding.Scope.Levels[0].Value = "other"
	original.Binding.Context.Sources[0].Revision = "other"
	require.Equal(t, "required-behavior", sealed.RequiredChecks[0])
	require.Equal(t, "one", sealed.Binding.Scope.Levels[0].Value)
	require.Equal(t, "1", sealed.StateProbes[0].Revision)
	require.NoError(t, sealed.Validate())
	sealed.SupervisorRevision = evidence.Digest([]byte("replacement"))
	require.ErrorIs(t, sealed.Validate(), verification.ErrInvalid)
}

func TestEvaluationManifestTargetsExactCapturedRequest(t *testing.T) {
	for name, mutate := range map[string]func(*verification.TestRequest){
		"artifact": func(r *verification.TestRequest) { r.Binding.Candidate.SHA256 = evidence.Digest([]byte("other")) },
		"tenant":   func(r *verification.TestRequest) { r.Binding.Scope.Levels[0].Value = "other" },
		"requirement": func(r *verification.TestRequest) {
			r.Binding.ContractDigest = evidence.Digest([]byte("irrelevant task"))
		},
		"criterion":   func(r *verification.TestRequest) { r.Criterion.ID = "irrelevant" },
		"optional":    func(r *verification.TestRequest) { r.Criterion.Required = false },
		"dataset":     func(r *verification.TestRequest) { r.Binding.Context.DatasetRevision = "other" },
		"environment": func(r *verification.TestRequest) { r.Binding.Context.EnvironmentRevision = "other" },
		"grant":       func(r *verification.TestRequest) { r.Binding.Context.AuthorityRevision = "revoked" },
		"manifest":    func(r *verification.TestRequest) { r.Criterion.Parameters.ManifestRevision = "other" },
		"run":         func(r *verification.TestRequest) { r.Binding.RunID = id.NewAgentRunID() },
	} {
		t.Run(name, func(t *testing.T) {
			manifest, req, now := evaluationManifestFixture(t)
			sealed, err := verification.SealEvaluationManifest(manifest)
			require.NoError(t, err)
			mutate(&req)
			require.ErrorIs(t, sealed.ValidateRequest(req, now), verification.ErrInvalid)
		})
	}
}

func TestEvaluationManifestRejectsUnboundGradingMaterial(t *testing.T) {
	for name, mutate := range map[string]func(*verification.EvaluationManifest){
		"unsupported":             func(m *verification.EvaluationManifest) { m.SchemaVersion = 2 },
		"non-test":                func(m *verification.EvaluationManifest) { m.Criterion.Kind = verification.KindSemantic },
		"revision":                func(m *verification.EvaluationManifest) { m.Revision = "other" },
		"missing supervisor":      func(m *verification.EvaluationManifest) { m.SupervisorRevision = "" },
		"missing checks":          func(m *verification.EvaluationManifest) { m.RequiredChecks = nil },
		"duplicate checks":        func(m *verification.EvaluationManifest) { m.RequiredChecks = []string{"one", "one"} },
		"blank check":             func(m *verification.EvaluationManifest) { m.RequiredChecks = []string{" "} },
		"too many checks":         func(m *verification.EvaluationManifest) { m.RequiredChecks = make([]string, 257) },
		"large check":             func(m *verification.EvaluationManifest) { m.RequiredChecks = []string{string(make([]byte, 1025))} },
		"unbound tests":           func(m *verification.EvaluationManifest) { m.TestBundle.Version = "other" },
		"candidate as tests":      func(m *verification.EvaluationManifest) { m.TestBundle = m.Binding.Candidate },
		"same protected identity": func(m *verification.EvaluationManifest) { m.ResultSchema = m.TestBundle },
		"wrong tenant fixture":    func(m *verification.EvaluationManifest) { m.FixtureDataset.Scope.Levels[0].Value = "other" },
		"public tests":            func(m *verification.EvaluationManifest) { m.TestBundle.Sensitivity = evidence.SensitivityPublic },
		"unknown probe": func(m *verification.EvaluationManifest) {
			m.StateProbes = []verification.Source{{Resource: "production", Revision: "1", Digest: evidence.Digest([]byte("state")), RetrievedAt: m.Binding.Candidate.CreatedAt}}
		},
		"changed probe": func(m *verification.EvaluationManifest) {
			m.StateProbes = append([]verification.Source(nil), m.StateProbes...)
			m.StateProbes[0].Revision = "other"
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, _, _ := evaluationManifestFixture(t)
			mutate(&m)
			_, err := verification.SealEvaluationManifest(m)
			require.ErrorIs(t, err, verification.ErrInvalid)
		})
	}
}

func TestEvaluationManifestRejectsExpiredAndFutureEvidence(t *testing.T) {
	for _, mode := range []string{"deadline", "retention", "future artifact", "future source", "old source", "zero clock"} {
		t.Run(mode, func(t *testing.T) {
			m, req, now := evaluationManifestFixture(t)
			switch mode {
			case "deadline":
				req.Deadline = now
			case "retention":
				req.Deadline = now.Add(time.Hour)
			case "future artifact":
				now = now.Add(-2 * time.Minute)
			case "future source":
				m.Binding.Context.Sources[0].RetrievedAt = now.Add(time.Second)
			case "old source":
				m.Criterion.MaxSourceAge = time.Second
				req.Criterion = m.Criterion
			case "zero clock":
				now = time.Time{}
			}
			req.Binding = m.Binding
			sealed, err := verification.SealEvaluationManifest(m)
			require.NoError(t, err)
			require.ErrorIs(t, sealed.ValidateRequest(req, now), verification.ErrInvalid)
		})
	}
}
