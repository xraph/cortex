package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func testVerification(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	vs, ok := s.(verification.Store)
	require.True(t, ok, "verification capability missing")
	es, ok := s.(evidence.Storage)
	require.True(t, ok, "evidence capability missing")
	audit, ok := s.(evidence.TombstoneReader)
	require.True(t, ok, "tombstone capability missing")
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "verification"}, {Key: "project", Value: "one"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	ctx = evidence.WithDeletionActor(ctx, "principal:operator")
	now := time.Now().UTC()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "host", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("candidate")})
	require.NoError(t, err)
	c, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "check output", Criteria: []verification.Criterion{{ID: "test", Kind: verification.KindTest, Required: true, Verifier: "test", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute}}})
	require.NoError(t, err)
	r, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: verification.Binding{Scope: scope, RunID: id.NewAgentRunID(), Candidate: a.Ref, ContractDigest: c.Digest, Context: verification.ContextSnapshot{PrincipalRef: "principal:operator", AuthorityRevision: "1", PreparedDigest: evidence.Digest([]byte("prepared")), PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}, CheckedAt: now, ExpiresAt: now.Add(time.Minute), Outcomes: []verification.Outcome{{CriterionID: "test", Verifier: "test", VerifierRevision: "1", Status: verification.StatusPassed}}})
	require.NoError(t, err)
	require.ErrorIs(t, vs.SaveReport(ctx, r), verification.ErrNotFound)
	require.NoError(t, vs.CreateContract(ctx, c))
	require.NoError(t, vs.CreateContract(ctx, c))
	altered := c
	altered.Objective = "other"
	altered, err = verification.SealContract(altered)
	require.NoError(t, err)
	require.ErrorIs(t, vs.CreateContract(ctx, altered), verification.ErrConflict)
	gotContract, err := vs.GetContract(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, c, gotContract)
	require.NoError(t, es.SaveEvidence(ctx, a))
	require.NoError(t, es.SaveEvidence(ctx, a))
	changed := a
	changed.Data = []byte("other")
	changed, err = evidence.Seal(changed)
	require.NoError(t, err)
	require.ErrorIs(t, es.SaveEvidence(ctx, changed), evidence.ErrConflict)
	gotArtifact, err := es.LoadEvidence(ctx, a.Ref)
	require.NoError(t, err)
	require.Equal(t, a, gotArtifact)
	require.NoError(t, vs.SaveReport(ctx, r))
	require.NoError(t, vs.SaveReport(ctx, r))
	gotReport, err := vs.GetReport(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, r, gotReport)
	changedReport := r
	changedReport.Outcomes = append([]verification.Outcome(nil), r.Outcomes...)
	changedReport.Outcomes[0].Status = verification.StatusFailed
	changedReport, err = verification.SealReport(changedReport)
	require.NoError(t, err)
	require.ErrorIs(t, vs.SaveReport(ctx, changedReport), verification.ErrConflict)
	reports, err := vs.ListReports(ctx, verification.ListRequest{RunID: r.Binding.RunID, Limit: 1})
	require.NoError(t, err)
	require.Len(t, reports, 1)
	reports, err = vs.ListReports(ctx, verification.ListRequest{RunID: r.Binding.RunID, After: r.ID, Limit: 1})
	require.NoError(t, err)
	require.Empty(t, reports)
	_, err = vs.ListReports(ctx, verification.ListRequest{RunID: r.Binding.RunID})
	require.Error(t, err)
	for _, otherScope := range []cortex.Scope{{Levels: scope.Levels[:1]}, {Levels: []cortex.Level{{Key: "org", Value: "other"}, {Key: "project", Value: "one"}}}} {
		other := cortex.WithScope(ctx, otherScope)
		_, err = vs.GetContract(other, c.ID)
		require.ErrorIs(t, err, verification.ErrNotFound)
		_, err = vs.GetReport(other, r.ID)
		require.ErrorIs(t, err, verification.ErrNotFound)
		_, err = es.LoadEvidence(other, a.Ref)
		require.Error(t, err)
		require.Error(t, es.SaveEvidence(other, a))
		require.Error(t, vs.CreateContract(other, c))
		require.Error(t, vs.SaveReport(other, r))
		require.Error(t, es.TombstoneEvidence(other, a.Ref, "revoked"))
	}
	require.ErrorIs(t, vs.CreateContract(context.Background(), c), cortex.ErrNoScope)
	require.ErrorIs(t, es.SaveEvidence(context.Background(), a), cortex.ErrNoScope)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, vs.SaveReport(cancelled, r), context.Canceled)
	require.ErrorIs(t, es.TombstoneEvidence(cancelled, a.Ref, "revoked"), context.Canceled)
	require.NoError(t, es.TombstoneEvidence(ctx, a.Ref, "source revoked"))
	require.NoError(t, es.TombstoneEvidence(ctx, a.Ref, "source revoked"))
	require.ErrorIs(t, es.TombstoneEvidence(ctx, a.Ref, "different reason"), evidence.ErrConflict)
	_, err = es.LoadEvidence(ctx, a.Ref)
	require.ErrorIs(t, err, evidence.ErrDeleted)
	require.ErrorIs(t, es.SaveEvidence(ctx, a), evidence.ErrDeleted)
	tombstone, err := audit.GetEvidenceTombstone(ctx, a.Ref)
	require.NoError(t, err)
	require.Equal(t, a.Ref, tombstone.Ref)
	require.Equal(t, "principal:operator", tombstone.Actor)
	require.Equal(t, "source revoked", tombstone.Reason)
	require.False(t, tombstone.DeletedAt.IsZero())
	inspector, ok := s.(store.ScopeInspector)
	require.True(t, ok)
	var issues []store.ScopeIssue
	require.NoError(t, inspector.InspectScopes(context.Background(), func(issue store.ScopeIssue) error { issues = append(issues, issue); return nil }))
	require.Empty(t, issues, "valid assurance records must use the backend scope codec")
	// Revocation preserves the report for audit, without granting fresh validity.
	gotReport, err = vs.GetReport(ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, r.Digest, gotReport.Digest)
}

func testEvidenceConcurrentIdentity(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	es, ok := s.(evidence.Storage)
	require.True(t, ok)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "race"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	now := time.Now().UTC()
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "host", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("one")})
	require.NoError(t, err)
	b := a
	b.Data = []byte("two")
	b, err = evidence.Seal(b)
	require.NoError(t, err)
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, candidate := range []evidence.Artifact{a, b} {
		go func() { <-start; results <- es.SaveEvidence(ctx, candidate) }()
	}
	close(start)
	first, second := <-results, <-results
	if first == nil {
		require.ErrorIs(t, second, evidence.ErrConflict)
	} else {
		require.ErrorIs(t, first, evidence.ErrConflict)
		require.NoError(t, second)
	}
}
