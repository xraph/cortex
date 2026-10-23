package verification_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func captureIntentFixture(t *testing.T) verification.CaptureIntent {
	t.Helper()
	contract, report, now := fixture(t)
	binding := report.Binding
	binding.Candidate.SHA256 = evidence.Digest(nil)
	binding.Candidate.Bytes = 0
	intent, err := verification.SealCaptureIntent(verification.CaptureIntent{SchemaVersion: 1, ID: id.New(verification.PrefixCaptureIntent), ResultID: id.New(verification.PrefixResult), MaxCandidateBytes: 1024, Template: verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: 1, ContractID: contract.ID, Binding: binding, Deadline: now.Add(time.Minute), MaxAttempts: 3, EvaluationBudget: &verification.EvaluationBudget{MaxCalls: 3, MaxTokens: 100, TokenOnly: true}}})
	require.NoError(t, err)
	return intent
}

func TestCaptureIntentPinsPolicyBeforeOutput(t *testing.T) {
	intent := captureIntentFixture(t)
	original := intent.Digest
	job, err := intent.JobFor(2, "captured output")
	require.NoError(t, err)
	require.Equal(t, evidence.Digest([]byte("captured output")), job.Binding.Candidate.SHA256)
	require.Equal(t, int64(len("captured output")), job.Binding.Candidate.Bytes)
	require.Equal(t, int64(2), job.ExpectedRunRevision)
	require.Equal(t, int64(1), intent.Template.ExpectedRunRevision)
	require.Equal(t, evidence.Digest(nil), intent.Template.Binding.Candidate.SHA256)
	job.Binding.Scope.Levels[0].Value = "mutated"
	job.EvaluationBudget.MaxCalls = 1
	require.NoError(t, intent.Validate())
	require.Equal(t, original, intent.Digest)
	_, err = intent.JobFor(1, "output")
	require.Error(t, err)
	intent.MaxCandidateBytes = 2
	intent, err = verification.SealCaptureIntent(intent)
	require.NoError(t, err)
	_, err = intent.JobFor(2, "large")
	require.ErrorIs(t, err, evidence.ErrTooLarge)
}

func TestCaptureIntentRejectsAmbiguousTemplates(t *testing.T) {
	for _, mutate := range []func(*verification.CaptureIntent){
		func(i *verification.CaptureIntent) { i.Template.ExpectedRunRevision = math.MaxInt64 - 2 },
		func(i *verification.CaptureIntent) { i.SchemaVersion = 2 },
		func(i *verification.CaptureIntent) { i.ID = id.New(verification.PrefixJob) },
		func(i *verification.CaptureIntent) { i.ResultID = id.New(verification.PrefixJob) },
		func(i *verification.CaptureIntent) { i.MaxCandidateBytes = 0 },
		func(i *verification.CaptureIntent) { i.Template.Binding.Candidate.Bytes = 1 },
		func(i *verification.CaptureIntent) {
			i.Template.Binding.Candidate.SHA256 = evidence.Digest([]byte("fixed candidate"))
		},
		func(i *verification.CaptureIntent) { i.Template.EvaluationBudget = nil },
	} {
		intent := captureIntentFixture(t)
		mutate(&intent)
		_, err := verification.SealCaptureIntent(intent)
		require.Error(t, err)
	}
	intent := captureIntentFixture(t)
	intent.Template.Binding.Context.AuthorityRevision = "changed"
	require.ErrorIs(t, intent.Validate(), verification.ErrInvalid)
}
