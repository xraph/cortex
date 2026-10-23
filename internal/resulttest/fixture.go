// Package resulttest supplies committed-result fixtures for transport tests.
package resulttest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

// Authorizer implements current content permission for a test host.
type Authorizer func(context.Context, verification.ResultContentRequest) error

// AuthorizeResultContent calls the configured test permission check.
func (f Authorizer) AuthorizeResultContent(ctx context.Context, r verification.ResultContentRequest) error {
	return f(ctx, r)
}

// Gate screens result content in transport fixtures.
type Gate func(context.Context, safety.PublicationRequest) (safety.PublicationDecision, error)

// Check calls the configured test screener.
func (f Gate) Check(ctx context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
	return f(ctx, r)
}

type clock struct{}

func (clock) Now() time.Time { return time.Now().UTC() }

type fence struct{}

func (fence) WithCommitFence(_ context.Context, b verification.Binding, fn func(verification.Binding) error) error {
	return fn(b)
}

// Options installs test-only host ports. The fence assumes sequential fixture
// mutations; it is not an implementation of a deployed cross-process guard.
func Options(auth Authorizer, gate Gate) []engine.Option {
	return []engine.Option{engine.WithResultContent(engine.ResultContentConfig{Authorizer: auth, Fence: fence{}, Clock: clock{}}), engine.WithPublicationGate(gate)}
}

// Commit saves an actual passed result with private content and retained evidence.
func Commit(ctx context.Context, t *testing.T, s store.Store) verification.CommitReceipt {
	t.Helper()
	scope, err := cortex.RequireScope(ctx)
	require.NoError(t, err)
	vs, ok := s.(verification.Store)
	require.True(t, ok)
	es, ok := s.(evidence.Storage)
	require.True(t, ok)
	cs, ok := s.(verification.CommitStore)
	require.True(t, ok)
	now := time.Now().UTC().Add(-time.Second)
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "host", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("SECRET candidate")})
	require.NoError(t, err)
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "check result", Criteria: []verification.Criterion{{ID: "test", Kind: verification.KindTest, Required: true, Verifier: "test", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "result-reader"}
	require.NoError(t, s.Create(ctx, ag))
	current := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateCompleted, StopReason: run.StopFinalAnswer, VerificationStatus: verification.StatusPending, Output: string(artifact.Data)}
	current.Provenance.PreparedDigest = evidence.Digest([]byte("prepared"))
	current.Provenance.AcceptanceRef = contract.ID.String()
	require.NoError(t, s.CreateRun(ctx, current))
	binding := verification.Binding{Scope: scope, RunID: current.ID, Candidate: artifact.Ref, ContractDigest: contract.Digest, Context: verification.ContextSnapshot{PrincipalRef: "operator", AuthorityRevision: "1", PreparedDigest: current.Provenance.PreparedDigest, PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}
	report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: binding, CheckedAt: now, ExpiresAt: now.Add(time.Hour), Outcomes: []verification.Outcome{{CriterionID: "test", Verifier: "test", VerifierRevision: "1", Status: verification.StatusPassed}}})
	require.NoError(t, err)
	require.NoError(t, vs.CreateContract(ctx, contract))
	require.NoError(t, es.SaveEvidence(ctx, artifact))
	require.NoError(t, vs.SaveReport(ctx, report))
	receipt, err := cs.CommitVerifiedResult(ctx, verification.CommitRequest{SchemaVersion: 1, ResultID: id.New(verification.PrefixResult), ExpectedRunRevision: current.Revision, ContractID: contract.ID, ReportIDs: []id.ID{report.ID}, Current: binding})
	require.NoError(t, err)
	return receipt
}
