package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func testStateObservation(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	vs, ok := s.(verification.Store)
	require.True(t, ok)
	es, ok := s.(evidence.Storage)
	require.True(t, ok)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "state-observations"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	now := time.Now().UTC()
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "host", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("captured candidate")})
	require.NoError(t, err)
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "check current inventory", Criteria: []verification.Criterion{{ID: "inventory", Kind: verification.KindState, Required: true, Verifier: "state", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute, MaxSourceAge: 10 * time.Second}}})
	require.NoError(t, err)
	source := verification.Source{Resource: "inventory:one", Revision: "1", Digest: evidence.Digest([]byte("state")), RetrievedAt: now}
	binding := verification.Binding{Scope: scope, RunID: id.NewAgentRunID(), ContractDigest: contract.Digest, Candidate: artifact.Ref, Context: verification.ContextSnapshot{PrincipalRef: "reader", AuthorityRevision: "1", PreparedDigest: evidence.Digest([]byte("prepared")), PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1", Sources: []verification.Source{source}}}
	digest, err := binding.Digest()
	require.NoError(t, err)
	observation := verification.StateObservation{BindingDigest: digest, ManifestDigest: evidence.Digest([]byte("registered manifest")), Source: source, StartedAt: now, FinishedAt: now, ExpiresAt: now.Add(10 * time.Second)}
	report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: binding, CheckedAt: now, ExpiresAt: now.Add(time.Minute), Outcomes: []verification.Outcome{{CriterionID: "inventory", Verifier: "state", VerifierRevision: "1", Status: verification.StatusPassed, StateObservations: []verification.StateObservation{observation}}}})
	require.NoError(t, err)
	require.NoError(t, vs.CreateContract(ctx, contract))
	require.NoError(t, es.SaveEvidence(ctx, artifact))
	require.NoError(t, vs.SaveReport(ctx, report))
	saved, err := vs.GetReport(ctx, report.ID)
	require.NoError(t, err)
	require.Equal(t, report, saved)
	require.Equal(t, verification.StatusPassed, verification.Aggregate(contract, []verification.Report{saved}, now).Status)
	require.Equal(t, verification.StatusStale, verification.CheckFreshness(saved, binding, now.Add(10*time.Second)))
	saved.Outcomes[0].StateObservations[0].ExpiresAt = now.Add(time.Hour)
	saved, err = verification.SealReport(saved)
	require.NoError(t, err)
	require.ErrorIs(t, verification.ValidateReportForContract(contract, saved), verification.ErrInvalid)
	require.Error(t, vs.SaveReport(ctx, saved))
}
