package verification_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestEvaluationBudgetConservativelyChargesUnknownUsage(t *testing.T) {
	budget := verification.EvaluationBudget{MaxCalls: 2, MaxTokens: 100, MaxUSDMicros: 50, PricingRevision: "prices-1"}
	require.NoError(t, budget.Validate())
	request := verification.CallRequest{ID: id.New(verification.PrefixCall), Verifier: "checker", VerifierRevision: "1", MaxTokens: 80, MaxUSDMicros: 40}
	call := verification.EvaluationCall{Request: request}
	require.Equal(t, verification.EvaluationDebit{Calls: 1, Tokens: 80, USDMicros: 40}, verification.DebitCalls([]verification.EvaluationCall{call}))
	call.Completion = &verification.CallCompletion{CallID: request.ID, Usage: verification.EvaluationUsage{Tokens: 10, TokensKnown: true, Receipt: "provider-1"}}
	require.Equal(t, verification.EvaluationDebit{Calls: 1, Tokens: 10, USDMicros: 40}, verification.DebitCalls([]verification.EvaluationCall{call}))
	call.Completion.Usage = verification.EvaluationUsage{Tokens: 110, TokensKnown: true, USDMicros: 60, MoneyKnown: true, Receipt: "provider-2"}
	require.Equal(t, verification.EvaluationDebit{Calls: 1, Tokens: 110, USDMicros: 60}, verification.DebitCalls([]verification.EvaluationCall{call}))
	require.ErrorIs(t, budget.Admit(verification.DebitCalls([]verification.EvaluationCall{call}), request), verification.ErrJobBudgetExceeded)
	call.Completion.Usage.Tokens = math.MaxInt64
	require.Equal(t, int64(math.MaxInt64), verification.DebitCalls([]verification.EvaluationCall{call, call}).Tokens)
}

func TestEvaluationBudgetRejectsImplicitFreeMoney(t *testing.T) {
	for _, budget := range []verification.EvaluationBudget{
		{MaxCalls: 0, TokenOnly: true},
		{MaxCalls: 257, TokenOnly: true},
		{MaxCalls: 1, MaxTokens: -1, TokenOnly: true},
		{MaxCalls: 1},
		{MaxCalls: 1, MaxUSDMicros: 1},
		{MaxCalls: 1, MaxUSDMicros: 1, TokenOnly: true},
	} {
		require.ErrorIs(t, budget.Validate(), verification.ErrInvalid)
	}
	budget := verification.EvaluationBudget{MaxCalls: 1, TokenOnly: true}
	require.NoError(t, budget.Validate())
	request := verification.CallRequest{ID: id.New(verification.PrefixCall), Verifier: "local-schema", VerifierRevision: "1"}
	require.NoError(t, budget.Admit(verification.EvaluationDebit{}, request))
	request.MaxTokens = 1
	require.ErrorIs(t, budget.Admit(verification.EvaluationDebit{}, request), verification.ErrJobBudgetExceeded)
}

func TestBudgetedJobSnapshotsDetachAndValidateCalls(t *testing.T) {
	contract, report, now := fixture(t)
	req := verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: 1, ContractID: contract.ID, Binding: report.Binding, Deadline: now.Add(time.Minute), MaxAttempts: 2, EvaluationBudget: &verification.EvaluationBudget{MaxCalls: 2, MaxTokens: 100, TokenOnly: true}}
	frozen, digest, err := req.Freeze()
	require.NoError(t, err)
	req.EvaluationBudget.MaxTokens = 0
	require.Equal(t, int64(100), frozen.EvaluationBudget.MaxTokens)
	claimID := id.New(verification.PrefixJobClaim)
	call := verification.EvaluationCall{Request: verification.CallRequest{ID: id.New(verification.PrefixCall), Verifier: "checker", VerifierRevision: "1", MaxTokens: 50}, Generation: 1, CriterionIDs: []string{"test"}, StartedAt: now}
	job, err := verification.SealJob(verification.Job{SchemaVersion: 1, Request: frozen, RequestDigest: digest, RunRevision: 2, Revision: 2, Generation: 1, Attempts: 1, ClaimIDs: []id.ID{claimID}, State: verification.JobRunning, Lease: &verification.JobLease{JobID: frozen.ID, ClaimID: claimID, Owner: "worker", Generation: 1, ExpiresAt: now.Add(time.Second)}, CreatedAt: now, UpdatedAt: now, Calls: []verification.EvaluationCall{call}})
	require.NoError(t, err)
	detached, err := verification.SealJob(job)
	require.NoError(t, err)
	job.Calls[0].CriterionIDs[0] = "mutated"
	require.NoError(t, detached.Validate())
	for _, mutate := range []func(*verification.Job){
		func(j *verification.Job) { j.Calls[0].Generation++ },
		func(j *verification.Job) { j.Calls[0].Request.MaxTokens = 101 },
		func(j *verification.Job) { j.Calls[0].StartedAt = now.Add(time.Hour) },
		func(j *verification.Job) { j.Calls = append(j.Calls, j.Calls[0]) },
		func(j *verification.Job) { j.Calls[0].CriterionIDs = []string{"test", "test"} },
		func(j *verification.Job) {
			j.Calls[0].Completion = &verification.CallCompletion{CallID: call.Request.ID, Failure: "timeout"}
			j.Calls[0].CompletedAt = now
		},
	} {
		changed, copyErr := verification.SealJob(detached)
		require.NoError(t, copyErr)
		mutate(&changed)
		_, copyErr = verification.SealJob(changed)
		require.ErrorIs(t, copyErr, verification.ErrInvalid)
	}
	detached.State, detached.Lease = verification.JobInconclusive, nil
	detached.Calls[0].Completion = &verification.CallCompletion{CallID: call.Request.ID, Failure: "timeout"}
	detached.Calls[0].CompletedAt = now
	sealed, err := verification.SealJob(detached)
	require.NoError(t, err)
	detached.Calls[0].Completion.Failure = "changed"
	require.NoError(t, sealed.Validate())
}
