package coordination_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func taskRunRequestFixture(t *testing.T) coordination.TaskRunRequest {
	t.Helper()
	_, dispatch := taskDispatchFixture(t)
	h := dispatch.Assignment.Handoff
	at := dispatch.Claim.At
	candidate := evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: h.Scope, Version: "1", Producer: "task", MediaType: "text/plain", Sensitivity: evidence.SensitivityRestricted, SHA256: evidence.Digest(nil), CreatedAt: at, RetainUntil: h.Deadline.Add(time.Hour)}
	transcript, input := candidate, candidate
	transcript.ID, input.ID = id.New(evidence.PrefixArtifact), id.New(evidence.PrefixArtifact)
	input.Bytes, input.SHA256 = 1, evidence.Digest([]byte("input"))
	contractID := id.New(verification.PrefixContract)
	binding := verification.Binding{Scope: h.Scope, RunID: dispatch.Claim.RunID, ContractDigest: h.ContractDigest, Candidate: candidate, Evidence: []evidence.Ref{input}, Context: verification.ContextSnapshot{PrincipalRef: h.Receiver.Principal, AuthorityRevision: "1", PreparedDigest: evidence.Digest([]byte("prepared")), PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1", ExecutionInputs: &input}}
	intent, err := verification.SealCaptureIntent(verification.CaptureIntent{SchemaVersion: 2, ID: id.New(verification.PrefixCaptureIntent), Template: verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: 1, ContractID: contractID, Binding: binding, Deadline: h.Deadline, MaxAttempts: 1, EvaluationBudget: &verification.EvaluationBudget{MaxCalls: 1, MaxTokens: 10, TokenOnly: true}}, ResultID: id.New(verification.PrefixResult), MaxCandidateBytes: 100, Transcript: &transcript, MaxTranscriptBytes: 100})
	require.NoError(t, err)
	r := run.Run{ID: dispatch.Claim.RunID, AgentID: id.NewAgentID(), SessionID: id.NewSessionID(), Scope: h.Scope, State: run.StateRunning, Input: "input", Budget: execution.Budget{MaxSteps: 1, MaxTokens: 10, MaxToolCalls: 1, Deadline: h.Deadline}, Provenance: run.ExecutionProvenance{SchemaVersion: 1, Purpose: run.PurposeNormal, PreparedDigest: binding.Context.PreparedDigest, AssignmentDigest: h.Digest, ParentRunID: h.ParentRunID.String(), AcceptanceRef: contractID.String(), VerificationRequired: true, VerificationIntentID: intent.ID.String(), ExecutionInputsDigest: input.SHA256}}
	req := coordination.TaskRunRequest{SchemaVersion: 1, Claim: dispatch.Claim.Token(), DispatchDigest: dispatch.RequestDigest, AssignmentRevision: dispatch.Assignment.Revision, AssignmentDigest: dispatch.Assignment.Digest, ParentRunRevision: 1, Run: r, Intent: intent, Actor: h.Receiver.Principal, Reason: "start"}
	req, _, err = req.Freeze()
	require.NoError(t, err)
	return req
}

func TestTaskRunRequiresPristineBoundedChild(t *testing.T) {
	for name, mutate := range map[string]func(*coordination.TaskRunRequest){
		"schema": func(r *coordination.TaskRunRequest) { r.SchemaVersion++ },
		"claim_provenance": func(r *coordination.TaskRunRequest) {
			r.Run.Provenance.TaskClaimID = id.New(coordination.PrefixClaim).String()
		},
		"claim":           func(r *coordination.TaskRunRequest) { r.Claim.Fence = 0 },
		"dispatch":        func(r *coordination.TaskRunRequest) { r.DispatchDigest = "" },
		"assignment":      func(r *coordination.TaskRunRequest) { r.AssignmentRevision = 0 },
		"parent_revision": func(r *coordination.TaskRunRequest) { r.ParentRunRevision = 0 },
		"scope":           func(r *coordination.TaskRunRequest) { r.Run.Scope = cortex.Scope{} },
		"revision":        func(r *coordination.TaskRunRequest) { r.Run.Revision = 1 },
		"created":         func(r *coordination.TaskRunRequest) { r.Run.CreatedAt = time.Now() },
		"started":         func(r *coordination.TaskRunRequest) { at := time.Now(); r.Run.StartedAt = &at },
		"state":           func(r *coordination.TaskRunRequest) { r.Run.State = run.StateCompleted },
		"invalid_utf8":    func(r *coordination.TaskRunRequest) { r.Run.Input = string([]byte{0xff}) },
		"output":          func(r *coordination.TaskRunRequest) { r.Run.Output = "old" },
		"usage":           func(r *coordination.TaskRunRequest) { r.Run.Usage.Tokens = 1 },
		"metadata":        func(r *coordination.TaskRunRequest) { r.Run.Metadata = map[string]any{"unbound": true} },
		"unlimited":       func(r *coordination.TaskRunRequest) { r.Run.Budget.MaxTokens = 0 },
		"purpose":         func(r *coordination.TaskRunRequest) { r.Run.Provenance.Purpose = run.PurposeEvaluation },
		"verification":    func(r *coordination.TaskRunRequest) { r.Run.Provenance.VerificationRequired = false },
		"input_binding":   func(r *coordination.TaskRunRequest) { r.Run.Provenance.ExecutionInputsDigest = evidence.Digest(nil) },
		"intent_binding": func(r *coordination.TaskRunRequest) {
			r.Run.Provenance.VerificationIntentID = id.New(verification.PrefixCaptureIntent).String()
		},
		"parent_identity": func(r *coordination.TaskRunRequest) { r.Run.Provenance.ParentRunID = r.Run.ID.String() },
		"actor":           func(r *coordination.TaskRunRequest) { r.Actor = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			req := taskRunRequestFixture(t)
			mutate(&req)
			frozen, digest, err := req.Freeze()
			require.Error(t, err)
			require.Zero(t, frozen)
			require.Empty(t, digest)
		})
	}
}

func TestTaskRunReceiptAndDetachedSnapshot(t *testing.T) {
	req := taskRunRequestFixture(t)
	frozen, digest, err := req.Freeze()
	require.NoError(t, err)
	at := req.Intent.Template.Binding.Candidate.CreatedAt.Add(time.Second)
	child, err := req.InitialRun(at)
	require.NoError(t, err)
	require.True(t, child.CreatedAt.Equal(at.Truncate(time.Millisecond)))
	data, err := json.Marshal(child)
	require.NoError(t, err)
	result := coordination.TaskRunStart{RequestDigest: digest, RunID: child.ID, RunDigest: evidence.Digest(data), CreatedAt: child.CreatedAt, IntentID: req.Intent.ID, IntentDigest: req.Intent.Digest, Fresh: true, Outbox: coordination.TaskRunOutbox{EventID: id.New(coordination.PrefixTaskRunEvent), Type: "task_run_created", ClaimID: req.Claim.ClaimID, PlanID: req.Claim.PlanID, TaskID: req.Claim.TaskID, RunID: req.Run.ID, IntentID: req.Intent.ID}}
	require.NoError(t, result.Validate(req))
	for _, mode := range []string{"digest", "time", "intent", "outbox", "run"} {
		changed := result
		switch mode {
		case "digest":
			changed.RunDigest = evidence.Digest(nil)
		case "time":
			changed.CreatedAt = changed.CreatedAt.Add(time.Second)
		case "intent":
			changed.IntentDigest = evidence.Digest(nil)
		case "outbox":
			changed.Outbox.RunID = id.NewAgentRunID()
		case "run":
			changed.RunID = id.NewAgentRunID()
		}
		require.Error(t, changed.Validate(req), mode)
	}
	req.Run.Scope.Levels[0].Value = "mutated"
	req.Intent.Template.Binding.Context.AuthorityRevision = "mutated"
	require.NotEqual(t, req.Run.Scope, frozen.Run.Scope)
	require.NotEqual(t, req.Intent.Template.Binding.Context, frozen.Intent.Template.Binding.Context)
	require.NoError(t, result.Validate(frozen))
	_, err = frozen.InitialRun(frozen.Run.Budget.Deadline)
	require.Error(t, err)
}
