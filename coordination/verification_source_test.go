package coordination_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

type verificationDiscovery func(context.Context, verification.JobListRequest) ([]verification.Job, error)

func (f verificationDiscovery) ListVerificationJobs(ctx context.Context, r verification.JobListRequest) ([]verification.Job, error) {
	return f(ctx, r)
}

func verificationDiscoveryJob(t *testing.T, scope cortex.Scope) verification.Job {
	t.Helper()
	now := time.Now().UTC()
	ref := evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "host", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour), SHA256: evidence.Digest([]byte("candidate")), Bytes: 9}
	binding := verification.Binding{Scope: scope, RunID: id.NewAgentRunID(), Candidate: ref, ContractDigest: evidence.Digest([]byte("contract")), Context: verification.ContextSnapshot{PrincipalRef: "operator", AuthorityRevision: "1", PreparedDigest: evidence.Digest([]byte("prepared")), PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}
	request, digest, err := (verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: 1, ContractID: id.New(verification.PrefixContract), Binding: binding, Deadline: now.Add(time.Minute), MaxAttempts: 3}).Freeze()
	require.NoError(t, err)
	job, err := verification.SealJob(verification.Job{SchemaVersion: 1, Request: request, RequestDigest: digest, RunRevision: 2, Revision: 1, State: verification.JobQueued, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	return job
}

func TestScopedVerificationSourcePagesAndRechecksAuthority(t *testing.T) {
	scopes := []cortex.Scope{{Levels: []cortex.Level{{Key: "workspace", Value: "empty"}}}, {Levels: []cortex.Level{{Key: "workspace", Value: "populated"}}}}
	job := verificationDiscoveryJob(t, scopes[1])
	calls, grants := 0, 0
	revoked := false
	denied := errors.New("denied")
	source, err := coordination.NewScopedVerificationSource(verificationDiscovery(func(ctx context.Context, r verification.JobListRequest) ([]verification.Job, error) {
		calls++
		if cortex.ScopeFromContext(ctx).Canonical() != job.Request.Binding.Scope.Canonical() || !r.After.IsNil() {
			return nil, nil
		}
		return []verification.Job{job}, nil
	}), scopes, func(context.Context, cortex.Scope) error {
		grants++
		if revoked && grants%2 == 0 {
			return denied
		}
		return nil
	})
	require.NoError(t, err)
	scopes[0].Levels[0].Value = "caller mutation"
	empty, err := source.ListCapturedVerificationJobs(t.Context(), coordination.VerificationJobListRequest{Limit: 1})
	require.NoError(t, err)
	require.Empty(t, empty.Jobs)
	require.NotEmpty(t, empty.Next)
	page, err := source.ListCapturedVerificationJobs(t.Context(), coordination.VerificationJobListRequest{Limit: 1, After: empty.Next})
	require.NoError(t, err)
	require.Equal(t, []coordination.ScheduledVerificationJob{{Scope: job.Request.Binding.Scope, ID: job.Request.ID}}, page.Jobs)
	end, err := source.ListCapturedVerificationJobs(t.Context(), coordination.VerificationJobListRequest{Limit: 1, After: page.Next})
	require.NoError(t, err)
	require.Empty(t, end.Jobs)
	require.Empty(t, end.Next)
	require.Equal(t, 3, calls)
	revoked = true
	rejected, err := source.ListCapturedVerificationJobs(t.Context(), coordination.VerificationJobListRequest{Limit: 1, After: empty.Next})
	require.ErrorIs(t, err, denied)
	require.Empty(t, rejected.Jobs)
	for _, cursor := range []string{"bad", "-1:", "2:", "1:" + id.NewAgentRunID().String()} {
		_, err = source.ListCapturedVerificationJobs(t.Context(), coordination.VerificationJobListRequest{Limit: 1, After: cursor})
		require.Error(t, err)
	}
}

func TestScopedVerificationSourceRejectsForeignMalformedAndCancelledPages(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "own"}}}
	job := verificationDiscoveryJob(t, scope)
	foreign := verificationDiscoveryJob(t, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}})
	changed := job
	changed.Revision++
	for _, rows := range [][]verification.Job{{foreign}, {changed}, {job, job}} {
		source, err := coordination.NewScopedVerificationSource(verificationDiscovery(func(context.Context, verification.JobListRequest) ([]verification.Job, error) { return rows, nil }), []cortex.Scope{scope}, func(context.Context, cortex.Scope) error { return nil })
		require.NoError(t, err)
		page, err := source.ListCapturedVerificationJobs(t.Context(), coordination.VerificationJobListRequest{Limit: 2})
		require.Error(t, err)
		require.Empty(t, page.Jobs)
	}
	reads := 0
	source, err := coordination.NewScopedVerificationSource(verificationDiscovery(func(context.Context, verification.JobListRequest) ([]verification.Job, error) {
		reads++
		return nil, nil
	}), []cortex.Scope{scope}, func(context.Context, cortex.Scope) error { return errors.New("denied") })
	require.NoError(t, err)
	_, err = source.ListCapturedVerificationJobs(t.Context(), coordination.VerificationJobListRequest{Limit: 1})
	require.Error(t, err)
	require.Zero(t, reads)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = source.ListCapturedVerificationJobs(cancelled, coordination.VerificationJobListRequest{Limit: 1})
	require.ErrorIs(t, err, context.Canceled)
	for _, scopes := range [][]cortex.Scope{nil, {scope, scope}, make([]cortex.Scope, 257)} {
		_, err = coordination.NewScopedVerificationSource(verificationDiscovery(func(context.Context, verification.JobListRequest) ([]verification.Job, error) { return nil, nil }), scopes, func(context.Context, cortex.Scope) error { return nil })
		require.Error(t, err)
	}
}
