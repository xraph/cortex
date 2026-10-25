package coordination_test

import (
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func taskUsageFixture(t *testing.T) coordination.TaskUsageSnapshot {
	t.Helper()
	accepted, claim := taskAcceptanceClaimFixture(t, true)
	journal, err := execution.SealRunRecoverySnapshot(execution.RunRecoverySnapshot{SchemaVersion: 1, Seal: accepted.Request.Recovery.Seal})
	require.NoError(t, err)
	binding := accepted.Request.Current
	jobRequest := verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: 1, ContractID: id.New(verification.PrefixContract), Binding: binding, Deadline: claim.At.Add(time.Minute), MaxAttempts: 2, EvaluationBudget: &verification.EvaluationBudget{MaxCalls: 2, MaxTokens: 100, MaxUSDMicros: 100, PricingRevision: "prices-1"}}
	jobRequest, digest, err := jobRequest.Freeze()
	require.NoError(t, err)
	callID := id.New(verification.PrefixCall)
	call := verification.EvaluationCall{Request: verification.CallRequest{ID: callID, Verifier: "checker", VerifierRevision: "1", MaxTokens: 20, MaxUSDMicros: 10}, Generation: 1, CriterionIDs: []string{"quality"}, StartedAt: claim.At, CompletedAt: claim.At, Completion: &verification.CallCompletion{CallID: callID, Failure: "rejected", Usage: verification.EvaluationUsage{Tokens: 7, USDMicros: 3, TokensKnown: true, MoneyKnown: true, Receipt: "verifier-receipt"}}}
	job, err := verification.SealJob(verification.Job{SchemaVersion: 1, Request: jobRequest, RequestDigest: digest, RunRevision: 2, Revision: 2, Generation: 1, Attempts: 1, ClaimIDs: []id.ID{id.New(verification.PrefixJobClaim)}, State: verification.JobFailed, Calls: []verification.EvaluationCall{call}, CreatedAt: claim.At, UpdatedAt: claim.At})
	require.NoError(t, err)
	snapshot, err := coordination.SealTaskUsageSnapshot(coordination.TaskUsageSnapshot{SchemaVersion: 1, Claim: claim, Journal: journal, Jobs: []verification.Job{job}})
	require.NoError(t, err)
	return snapshot
}

func resealTaskUsage(t *testing.T, s coordination.TaskUsageSnapshot) coordination.TaskUsageSnapshot {
	t.Helper()
	var err error
	for i := range s.Jobs {
		s.Jobs[i].Request, s.Jobs[i].RequestDigest, err = s.Jobs[i].Request.Freeze()
		require.NoError(t, err)
		s.Jobs[i], err = verification.SealJob(s.Jobs[i])
		require.NoError(t, err)
	}
	s.Journal, err = execution.SealRunRecoverySnapshot(s.Journal)
	require.NoError(t, err)
	s, err = coordination.SealTaskUsageSnapshot(s)
	require.NoError(t, err)
	return s
}

func TestTaskUsageIncludesFailedAndEarlierVerifierWork(t *testing.T) {
	s := taskUsageFixture(t)
	prior := s.Jobs[0]
	prior.Request.ID = id.New(verification.PrefixJob)
	s.Jobs = append(s.Jobs, prior)
	sort.Slice(s.Jobs, func(i, j int) bool { return s.Jobs[i].Request.ID.String() < s.Jobs[j].Request.ID.String() })
	s = resealTaskUsage(t, s)
	usage, err := coordination.SummarizeTaskUsage(s)
	require.NoError(t, err)
	require.Equal(t, int64(14), usage.Observed.Tokens)
	require.Equal(t, int64(6), usage.Observed.USDMicros)
	require.Equal(t, execution.KnownTokens|execution.KnownMoney, usage.Known)
	require.Equal(t, 2, usage.VerifierCalls)
	require.Zero(t, usage.OpenJobs)
	before := s.Jobs[0].Calls[0].Completion.Usage.Tokens
	copyOfSnapshot, err := coordination.SealTaskUsageSnapshot(s)
	require.NoError(t, err)
	copyOfSnapshot.Jobs[0].Calls[0].Completion.Usage.Tokens++
	require.Equal(t, before, s.Jobs[0].Calls[0].Completion.Usage.Tokens)
	require.Error(t, copyOfSnapshot.Validate())
}

func TestTaskUsageMissingUsageRetainsObservedLowerBound(t *testing.T) {
	s := taskUsageFixture(t)
	prior := s.Jobs[0]
	prior.Request.ID = id.New(verification.PrefixJob)
	prior.Calls = nil
	prior.State = verification.JobCancelled
	prior.Request.EvaluationBudget = nil
	s.Jobs = append(s.Jobs, prior)
	sort.Slice(s.Jobs, func(i, j int) bool { return s.Jobs[i].Request.ID.String() < s.Jobs[j].Request.ID.String() })
	s = resealTaskUsage(t, s)
	usage, err := coordination.SummarizeTaskUsage(s)
	require.NoError(t, err)
	require.Zero(t, usage.Known, "unmetered jobs do not prove zero usage")
	require.Equal(t, int64(7), usage.Observed.Tokens)
	require.Equal(t, int64(3), usage.Observed.USDMicros)
}

func TestTaskUsageOverflowCannotBecomeExactUsage(t *testing.T) {
	s := taskUsageFixture(t)
	prior := s.Jobs[0]
	prior.Request.ID = id.New(verification.PrefixJob)
	s.Jobs[0].Calls[0].Completion.Usage.Tokens = math.MaxInt64
	s.Jobs = append(s.Jobs, prior)
	sort.Slice(s.Jobs, func(i, j int) bool { return s.Jobs[i].Request.ID.String() < s.Jobs[j].Request.ID.String() })
	s = resealTaskUsage(t, s)
	usage, err := coordination.SummarizeTaskUsage(s)
	require.NoError(t, err)
	require.Equal(t, int64(math.MaxInt64), usage.Observed.Tokens)
	require.Equal(t, execution.KnownTokens, usage.Overflow)
	require.Equal(t, execution.KnownMoney, usage.Known)
}

func TestTaskUsageRejectsForeignOrDuplicateJobs(t *testing.T) {
	for _, mode := range []string{"run", "scope", "foreign valid job", "duplicate", "digest", "count"} {
		t.Run(mode, func(t *testing.T) {
			s := taskUsageFixture(t)
			switch mode {
			case "run":
				s.Journal.Seal.Request.RunID = id.NewAgentRunID()
			case "scope":
				s.Jobs[0].Request.Binding.Scope.Levels[0].Value = "foreign"
			case "foreign valid job":
				s.Jobs[0] = taskUsageFixture(t).Jobs[0]
			case "duplicate":
				s.Jobs = append(s.Jobs, s.Jobs[0])
			case "digest":
				s.Journal.Digest = evidence.Digest([]byte("other"))
			case "count":
				s.Jobs = make([]verification.Job, coordination.MaxTaskUsageJobs+1)
			}
			_, err := coordination.SealTaskUsageSnapshot(s)
			require.Error(t, err)
		})
	}
}

func TestTaskUsageCombinesProviderAndVerifierReceipts(t *testing.T) {
	s := taskUsageFixture(t)
	r := execution.RunCallRequest{SchemaVersion: 1, ID: id.New(execution.PrefixRunCall), Scope: s.Claim.Scope, RunID: s.Claim.RunID, AgentID: id.NewAgentID(), ClaimID: s.Claim.ID, ParentRunID: id.NewAgentRunID(), Kind: execution.RunCallComplete, OperationDigest: evidence.Digest([]byte("slot")), RequestDigest: evidence.Digest([]byte("request")), PreparedDigest: evidence.Digest([]byte("prepared")), AssignmentDigest: evidence.Digest([]byte("assignment")), Deadline: s.Claim.ExpiresAt}
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	provider := &execution.ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.ProviderCallFailed, Provider: "fixture", AdapterRevision: "1", AccountRef: "account", OperationID: "operation", ProofRef: "receipt", ProofDigest: evidence.Digest([]byte("proof")), ObservedAt: s.Claim.At, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 11, USDMicros: 5}, Known: execution.KnownTokens | execution.KnownMoney}}
	s.Journal.Calls = []execution.RecoveredRunCall{{Call: execution.RunCallRecord{Request: r, StartedAt: s.Claim.AcquiredAt}, Provider: provider}}
	s = resealTaskUsage(t, s)
	usage, err := coordination.SummarizeTaskUsage(s)
	require.NoError(t, err)
	require.Equal(t, int64(18), usage.Observed.Tokens)
	require.Equal(t, int64(8), usage.Observed.USDMicros)
	require.Equal(t, 1, usage.ProviderCalls)
	require.Equal(t, 1, usage.VerifierCalls)

	// Local token estimates cannot substitute for a missing provider receipt.
	s.Journal.Calls[0].Provider = nil
	s.Journal.Calls[0].Call.Completion = &execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("result")), TokensReported: true, ReportedTokens: 99}
	s.Journal.Calls[0].Call.CompletedAt = s.Claim.At
	s = resealTaskUsage(t, s)
	usage, err = coordination.SummarizeTaskUsage(s)
	require.NoError(t, err)
	require.Zero(t, usage.Known)
	require.Equal(t, int64(7), usage.Observed.Tokens)
	// Contradictory provider evidence must fail before it becomes usage.
	s.Journal.Calls[0].Provider = provider
	s = resealTaskUsage(t, s)
	_, err = coordination.SummarizeTaskUsage(s)
	require.Error(t, err)
}

func TestTaskUsageUnknownDimensionsAndOpenJobs(t *testing.T) {
	s := taskUsageFixture(t)
	s.Jobs[0].Calls[0].Completion.Usage = verification.EvaluationUsage{TokensKnown: true, Receipt: "known-zero"}
	s = resealTaskUsage(t, s)
	usage, err := coordination.SummarizeTaskUsage(s)
	require.NoError(t, err)
	require.Equal(t, execution.KnownTokens, usage.Known)
	require.Zero(t, usage.Observed)

	s.Jobs[0].Calls[0].Completion = nil
	s.Jobs[0].Calls[0].CompletedAt = time.Time{}
	s.Jobs[0].State = verification.JobRunning
	s.Jobs[0].Lease = &verification.JobLease{JobID: s.Jobs[0].Request.ID, ClaimID: s.Jobs[0].ClaimIDs[0], Owner: "verifier", Generation: 1, ExpiresAt: s.Jobs[0].Request.Deadline}
	s = resealTaskUsage(t, s)
	usage, err = coordination.SummarizeTaskUsage(s)
	require.NoError(t, err)
	require.Zero(t, usage.Known)
	require.Equal(t, 1, usage.OpenJobs)
}
