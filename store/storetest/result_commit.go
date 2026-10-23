package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func resultFixture(t *testing.T, s store.Store) (context.Context, verification.CommitRequest) {
	t.Helper()
	ctx := ctxWithScope("result-commit")
	scope, err := cortex.RequireScope(ctx)
	require.NoError(t, err)
	vs := resultCapability[verification.Store](t, s)
	es := resultCapability[evidence.Storage](t, s)
	now := time.Now().UTC().Add(-time.Second)
	a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "host", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("private candidate text")})
	require.NoError(t, err)
	c, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "check", Criteria: []verification.Criterion{{ID: "test", Kind: verification.KindTest, Required: true, Verifier: "test", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	r := mustCreateRun(t, s, ctx)
	r.State = run.StateCompleted
	r.StopReason = run.StopFinalAnswer
	r.VerificationStatus = verification.StatusPending
	r.Output = string(a.Data)
	r.Provenance.PreparedDigest = evidence.Digest([]byte("prepared"))
	r.Provenance.AcceptanceRef = c.ID.String()
	require.NoError(t, s.UpdateRun(ctx, r))
	binding := verification.Binding{Scope: scope, RunID: r.ID, Candidate: a.Ref, ContractDigest: c.Digest, Context: verification.ContextSnapshot{PrincipalRef: "operator", AuthorityRevision: "1", PreparedDigest: r.Provenance.PreparedDigest, PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}
	report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: binding, CheckedAt: now, ExpiresAt: now.Add(time.Hour), Outcomes: []verification.Outcome{{CriterionID: "test", Verifier: "test", VerifierRevision: "1", Status: verification.StatusPassed}}})
	require.NoError(t, err)
	require.NoError(t, vs.CreateContract(ctx, c))
	require.NoError(t, es.SaveEvidence(ctx, a))
	require.NoError(t, vs.SaveReport(ctx, report))
	return ctx, verification.CommitRequest{SchemaVersion: 1, ResultID: id.New(verification.PrefixResult), ExpectedRunRevision: r.Revision, ContractID: c.ID, ReportIDs: []id.ID{report.ID}, Current: binding}
}

func testVerifiedResultCommit(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	cs := resultCapability[verification.CommitStore](t, s)
	ctx, req := resultFixture(t, s)
	receipt, err := cs.CommitVerifiedResult(ctx, req)
	require.NoError(t, err)
	require.NoError(t, receipt.Validate())
	got, err := cs.GetCommittedResult(ctx, req.ResultID)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	current, err := s.GetRun(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, req.ExpectedRunRevision+1, current.Revision)
	require.Equal(t, req.ResultID.String(), current.ResultRef)
	require.Equal(t, verification.StatusPassed, current.VerificationStatus)
	payload, err := json.Marshal(receipt.Outbox)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "private candidate text")
	require.NotContains(t, string(payload), "operator")
	retry, err := cs.CommitVerifiedResult(ctx, req)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), req.Current.Candidate, "revoked after commit"))
	retry, err = cs.CommitVerifiedResult(ctx, req)
	require.NoError(t, err)
	require.Equal(t, receipt, retry, "retry returns historical receipt after revocation")
	changed := req
	changed.Current.Context.AuthorityRevision = "revoked"
	_, err = cs.CommitVerifiedResult(ctx, changed)
	require.ErrorIs(t, err, verification.ErrConflict)
	changed = req
	changed.ResultID = id.New(verification.PrefixResult)
	_, err = cs.CommitVerifiedResult(ctx, changed)
	require.ErrorIs(t, err, cortex.ErrRunConflict)
	_, err = cs.GetCommittedResult(ctx, changed.ResultID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	_, err = cs.GetCommittedResult(ctxWithScope("other"), req.ResultID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	_, err = cs.CommitVerifiedResult(context.Background(), req)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = cs.CommitVerifiedResult(cancelled, req)
	require.ErrorIs(t, err, context.Canceled)
}

func testVerifiedResultDenied(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	cs := resultCapability[verification.CommitStore](t, s)
	cases := map[string]func(context.Context, *verification.CommitRequest){
		"authority": func(_ context.Context, r *verification.CommitRequest) { r.Current.Context.AuthorityRevision = "2" },
		"policy":    func(_ context.Context, r *verification.CommitRequest) { r.Current.Context.PolicyRevision = "2" },
		"source": func(_ context.Context, r *verification.CommitRequest) {
			r.Current.Context.Sources = []verification.Source{{Resource: "source", Revision: "2", Digest: evidence.Digest([]byte("source")), RetrievedAt: time.Now().Add(-time.Second)}}
		},
		"unknown report": func(_ context.Context, r *verification.CommitRequest) {
			r.ReportIDs = []id.ID{id.New(verification.PrefixReport)}
		},
		"duplicate report": func(_ context.Context, r *verification.CommitRequest) {
			r.ReportIDs = append(r.ReportIDs, r.ReportIDs[0])
		},
		"revision": func(_ context.Context, r *verification.CommitRequest) { r.ExpectedRunRevision++ },
		"deleted evidence": func(ctx context.Context, r *verification.CommitRequest) {
			require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), r.Current.Candidate, "revoked"))
		},
		"candidate mismatch": func(ctx context.Context, r *verification.CommitRequest) {
			current, err := s.GetRun(ctx, r.Current.RunID)
			require.NoError(t, err)
			current.Output = "changed"
			require.NoError(t, s.UpdateRun(ctx, current))
			r.ExpectedRunRevision = current.Revision
		},
		"not pending": func(ctx context.Context, r *verification.CommitRequest) {
			current, err := s.GetRun(ctx, r.Current.RunID)
			require.NoError(t, err)
			current.VerificationStatus = verification.StatusUnchecked
			require.NoError(t, s.UpdateRun(ctx, current))
			r.ExpectedRunRevision = current.Revision
		},
		"failed report": func(ctx context.Context, r *verification.CommitRequest) {
			vs := resultCapability[verification.Store](t, s)
			report, err := vs.GetReport(ctx, r.ReportIDs[0])
			require.NoError(t, err)
			report.ID = id.New(verification.PrefixReport)
			report.Outcomes[0].Status = verification.StatusFailed
			report, err = verification.SealReport(report)
			require.NoError(t, err)
			require.NoError(t, vs.SaveReport(ctx, report))
			r.ReportIDs = []id.ID{report.ID}
		},
		"expired report": func(ctx context.Context, r *verification.CommitRequest) {
			vs := resultCapability[verification.Store](t, s)
			report, err := vs.GetReport(ctx, r.ReportIDs[0])
			require.NoError(t, err)
			report.ID = id.New(verification.PrefixReport)
			report.ExpiresAt = report.CheckedAt.Add(time.Millisecond)
			report, err = verification.SealReport(report)
			require.NoError(t, err)
			require.NoError(t, vs.SaveReport(ctx, report))
			r.ReportIDs = []id.ID{report.ID}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, req := resultFixture(t, s)
			mutate(ctx, &req)
			before, err := s.GetRun(ctx, req.Current.RunID)
			require.NoError(t, err)
			receipt, err := cs.CommitVerifiedResult(ctx, req)
			require.Error(t, err)
			require.Zero(t, receipt)
			after, err := s.GetRun(ctx, req.Current.RunID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			_, err = cs.GetCommittedResult(ctx, req.ResultID)
			require.ErrorIs(t, err, verification.ErrNotFound)
		})
	}
}

func testVerifiedResultConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	cs := resultCapability[verification.CommitStore](t, s)
	ctx, req := resultFixture(t, s)
	other := req
	other.ResultID = id.New(verification.PrefixResult)
	type attempt struct {
		receipt  verification.CommitReceipt
		err      error
		resultID id.ID
	}
	results := make(chan attempt, 2)
	start := make(chan struct{})
	for _, candidate := range []verification.CommitRequest{req, other} {
		go func() {
			<-start
			receipt, err := cs.CommitVerifiedResult(ctx, candidate)
			results <- attempt{receipt, err, candidate.ResultID}
		}()
	}
	close(start)
	winners := 0
	for range 2 {
		result := <-results
		if result.err == nil {
			winners++
			require.NoError(t, result.receipt.Validate())
		} else {
			require.ErrorIs(t, result.err, cortex.ErrRunConflict)
			require.Zero(t, result.receipt)
			_, err := cs.GetCommittedResult(ctx, result.resultID)
			require.ErrorIs(t, err, verification.ErrNotFound)
		}
	}
	require.Equal(t, 1, winners)
	current, err := s.GetRun(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, req.ExpectedRunRevision+1, current.Revision)
}

func resultCapability[T any](t *testing.T, s store.Store) T {
	t.Helper()
	capability, ok := s.(T)
	require.True(t, ok, "required result storage capability missing")
	return capability
}
