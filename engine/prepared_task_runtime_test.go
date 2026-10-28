package engine_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

type runtimeTaskGuard func(context.Context, engine.TaskCallRequest) (engine.TaskCallLease, error)

func (f runtimeTaskGuard) AcquireTaskCall(ctx context.Context, req engine.TaskCallRequest) (engine.TaskCallLease, error) {
	return f(ctx, req)
}

func TestRunPreparedTaskExecutesFreshAdmissionOnly(t *testing.T) {
	f, e, client, prepared, req, admitter := preparedTaskRuntimeFixture(t)
	require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
	acquired, released := 0, 0
	require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(func(_ context.Context, call engine.TaskCallRequest) (engine.TaskCallLease, error) {
		acquired++
		require.Equal(t, req.Run.ID, call.RunID)
		require.Equal(t, req.Claim.ClaimID, call.ClaimID)
		_, digest, err := call.Freeze()
		require.NoError(t, err)
		return engine.TaskCallLease{RequestDigest: digest, ExpiresAt: call.Deadline, Revoked: make(chan struct{}), Release: func() error { released++; return nil }}, nil
	}))(e))
	// The captured history is empty. Later conversation writes cannot change it.
	require.NoError(t, f.storage.SaveConversation(f.ctx, req.Run.AgentID, req.Run.SessionID, []memory.Message{{Role: "user", Content: "new unrelated history"}}))
	r, err := e.RunPreparedTask(f.ctx, prepared, req)
	require.NoError(t, err)
	require.Equal(t, req.Run.ID, r.ID)
	require.Equal(t, verification.StatusPassed, r.VerificationStatus)
	require.Equal(t, "done", r.Output)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, acquired)
	require.Equal(t, 1, released)
	require.Len(t, client.Request(t).Messages, 1)
	require.Equal(t, req.Run.Input, client.Request(t).Messages[0].Content)
	r, err = e.RunPreparedTask(f.ctx, prepared, req)
	require.ErrorIs(t, err, cortex.ErrTaskRunReconciliationRequired)
	require.Nil(t, r)
	require.Equal(t, 1, client.calls)
}

type runtimeTaskAdmitter func(context.Context, coordination.TaskRunRequest) (coordination.TaskRunStart, error)

func (f runtimeTaskAdmitter) Admit(ctx context.Context, req coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
	return f(ctx, req)
}

func allowRuntimeTaskCall(_ context.Context, req engine.TaskCallRequest) (engine.TaskCallLease, error) {
	_, digest, err := req.Freeze()
	return engine.TaskCallLease{RequestDigest: digest, ExpiresAt: req.Deadline, Revoked: make(chan struct{}), Release: func() error { return nil }}, err
}

func TestRunPreparedTaskRejectsUnboundStart(t *testing.T) {
	for _, mode := range []string{"missing_admitter", "missing_guard", "cancelled", "scope", "marker", "input", "model_revision", "sources", "budget", "deadline", "policy", "denied_input", "persona"} {
		t.Run(mode, func(t *testing.T) {
			f, e, client, p, req, admitter := preparedTaskRuntimeFixture(t)
			admissions := 0
			if mode != "missing_admitter" {
				require.NoError(t, engine.WithTaskRunAdmitter(runtimeTaskAdmitter(func(ctx context.Context, r coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
					admissions++
					return admitter.Admit(ctx, r)
				}))(e))
			}
			if mode != "missing_guard" {
				require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
			}
			ctx := f.ctx
			switch mode {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "scope":
				ctx = context.Background()
			case "marker":
				req.Run.Provenance.TaskClaimID = ""
			case "input":
				req.Run.Input = "changed"
			case "model_revision":
				req.Run.Provenance.ModelRevision = "changed"
			case "sources":
				req.Run.Provenance.Sources = append(req.Run.Provenance.Sources, run.Revision{Name: "forged", Revision: "2"})
			case "budget":
				req.Run.Budget.MaxTokens++
			case "deadline":
				req.Run.Budget.Deadline = req.Run.Budget.Deadline.Add(time.Minute)
			case "persona":
				req.Run.PersonaRef = "changed"
			case "policy", "denied_input":
				cfg := requiredConfiguration(t, f)
				if mode == "policy" {
					cfg.Revision = "changed"
				} else {
					cfg.CaptureAuthorizer = captureAuthorizer(func(context.Context, evidence.Action, evidence.Ref) error { return cortex.ErrEvidenceAccessDenied })
				}
				require.NoError(t, engine.WithRequiredVerification(cfg)(e))
			}
			r, err := e.RunPreparedTask(ctx, p, req)
			require.Error(t, err)
			require.Nil(t, r)
			require.Zero(t, admissions)
			require.Zero(t, client.calls)
			_, err = f.storage.GetRun(f.ctx, req.Run.ID)
			require.ErrorIs(t, err, cortex.ErrRunNotFound)
		})
	}
}

func TestRunPreparedTaskAmbiguousStartNeverInvokes(t *testing.T) {
	for _, mode := range []string{"denied", "lost_ack", "cancelled_after_commit", "bad_receipt", "changed_child", "mutated_request"} {
		t.Run(mode, func(t *testing.T) {
			f, e, client, p, req, admitter := preparedTaskRuntimeFixture(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
			require.NoError(t, engine.WithTaskRunAdmitter(runtimeTaskAdmitter(func(ctx context.Context, offered coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
				if mode == "denied" {
					return coordination.TaskRunStart{}, errors.New("private authority reason")
				}
				start, err := admitter.Admit(ctx, offered)
				require.NoError(t, err)
				switch mode {
				case "lost_ack":
					return coordination.TaskRunStart{}, errors.New("lost acknowledgment")
				case "cancelled_after_commit":
					cancel()
				case "bad_receipt":
					start.RunDigest = evidence.Digest(nil)
				case "changed_child":
					child, readErr := f.storage.GetRun(ctx, offered.Run.ID)
					require.NoError(t, readErr)
					child.State = run.StateFailed
					require.NoError(t, f.storage.UpdateRun(ctx, child))
				case "mutated_request":
					offered.Run.Scope.Levels[0].Value = "changed by host"
				}
				return start, nil
			}))(e))
			r, err := e.RunPreparedTask(ctx, p, req)
			if mode == "mutated_request" {
				require.NoError(t, err)
				require.Equal(t, req.Run.Scope.Canonical(), r.Scope.Canonical())
				require.Equal(t, 1, client.calls)
			} else {
				require.ErrorIs(t, err, cortex.ErrTaskRunReconciliationRequired)
				require.NotContains(t, err.Error(), "private authority")
				require.Nil(t, r)
				require.Zero(t, client.calls)
			}
		})
	}
}

func TestStreamPreparedTaskUsesFreshAdmission(t *testing.T) {
	f, e, client, p, req, admitter := preparedTaskRuntimeFixture(t)
	require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
	require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
	client.chunks = []llm.Chunk{{Content: "done", FinishReason: "stop"}}
	events := make(chan engine.StreamEvent, 16)
	require.NoError(t, e.StreamPreparedTask(f.ctx, p, req, events))
	for event := range events {
		require.NotEqual(t, engine.EventError, event.Type)
	}
	child, err := f.storage.GetRun(f.ctx, req.Run.ID)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, child.VerificationStatus)
	require.Equal(t, 1, client.calls)
	events = make(chan engine.StreamEvent, 16)
	require.ErrorIs(t, e.StreamPreparedTask(f.ctx, p, req, events), cortex.ErrTaskRunReconciliationRequired)
	_, open := <-events
	require.False(t, open)
	require.Equal(t, 1, client.calls)
}

func TestRunPreparedTaskGuardDenialAfterAdmissionStopsGeneration(t *testing.T) {
	f, e, client, p, req, admitter := preparedTaskRuntimeFixture(t)
	require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
	require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(func(context.Context, engine.TaskCallRequest) (engine.TaskCallLease, error) {
		return engine.TaskCallLease{}, errors.New("authority revoked after creation")
	}))(e))
	r, err := e.RunPreparedTask(f.ctx, p, req)
	require.Error(t, err)
	require.Nil(t, r)
	r, err = f.storage.GetRun(f.ctx, req.Run.ID)
	require.NoError(t, err)
	require.NotEqual(t, run.StateCompleted, r.State)
	require.Empty(t, r.Output)
	require.Zero(t, client.calls)
	require.Zero(t, f.adapter.evaluations)
	_, err = e.RunPreparedTask(f.ctx, p, req)
	require.ErrorIs(t, err, cortex.ErrTaskRunReconciliationRequired)
	require.Zero(t, client.calls)
}

func TestStreamPreparedTaskRejectsUnverifiedOutput(t *testing.T) {
	f, e, client, p, req, admitter := preparedTaskRuntimeFixture(t)
	require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
	require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
	f.adapter.status = verification.StatusFailed
	client.chunks = []llm.Chunk{{Content: "private candidate", FinishReason: "stop"}}
	events := make(chan engine.StreamEvent, 16)
	require.NoError(t, e.StreamPreparedTask(f.ctx, p, req, events))
	var last engine.StreamEvent
	for event := range events {
		require.NotEqual(t, engine.EventToken, event.Type)
		require.NotContains(t, event.Data, "output")
		last = event
	}
	require.Equal(t, engine.EventError, last.Type)
	require.Equal(t, "run_unverified", last.Data["code"])
	child, err := f.storage.GetRun(f.ctx, req.Run.ID)
	require.NoError(t, err)
	require.Empty(t, child.Output)
	require.Equal(t, verification.StatusFailed, child.VerificationStatus)
}

func TestTaskRunAdmitterRejectsNil(t *testing.T) {
	var typedNil runtimeTaskAdmitter
	_, err := engine.New(engine.WithTaskRunAdmitter(typedNil))
	require.Error(t, err)
	_, err = engine.New(engine.WithTaskRunAdmitter(nil))
	require.Error(t, err)
}

type runtimeTaskInputStore struct {
	*sqlite.Store
	change func(*evidence.Artifact)
}

func (s runtimeTaskInputStore) LoadEvidence(ctx context.Context, ref evidence.Ref) (evidence.Artifact, error) {
	artifact, err := s.Store.LoadEvidence(ctx, ref)
	if err == nil {
		artifact.Ref = ref
		s.change(&artifact)
	}
	return artifact, err
}

func TestRunPreparedTaskRejectsChangedInputArtifactBinding(t *testing.T) {
	for _, mode := range []string{"content", "scope", "producer", "retention"} {
		t.Run(mode, func(t *testing.T) {
			f, e, client, p, req, admitter := preparedTaskRuntimeFixture(t)
			require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
			require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
			require.NoError(t, engine.WithStore(runtimeTaskInputStore{Store: f.storage, change: func(a *evidence.Artifact) {
				switch mode {
				case "content":
					a.Data = []byte("changed")
				case "scope":
					a.Ref.Scope.Levels[0].Value = "other"
				case "producer":
					a.Ref.Producer = "other"
				case "retention":
					a.Ref.RetainUntil = a.Ref.RetainUntil.Add(time.Minute)
				}
			}})(e))
			r, err := e.RunPreparedTask(f.ctx, p, req)
			require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
			require.Nil(t, r)
			require.Zero(t, client.calls)
			_, err = f.storage.GetRun(f.ctx, req.Run.ID)
			require.ErrorIs(t, err, cortex.ErrRunNotFound)
		})
	}
}

func TestRequiredResumeRejectsAliasedInputScope(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredToolClient{}
	e := requiredToolEngine(t, f, client, &requiredToolAuthority{}, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	ag, overrides := requiredToolAgent(t, f)
	r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, r.State)
	require.NoError(t, engine.WithStore(runtimeTaskInputStore{Store: f.storage, change: func(a *evidence.Artifact) { a.Ref.Scope.Levels[0].Value = "other" }})(e))
	_, err = e.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "SECRET-ID", Content: "result"}}})
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	saved, err := f.storage.GetRun(f.ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, r.Revision, saved.Revision)
	require.Equal(t, run.StatePaused, saved.State)
	require.Equal(t, 1, client.calls)
}

type runtimeTaskJournalStore struct {
	*sqlite.Store
	loseStartAck  bool
	failReturn    bool
	loseReturnAck bool
}

func (s runtimeTaskJournalStore) BeginRunCall(ctx context.Context, r execution.RunCallRequest) (bool, error) {
	fresh, err := s.Store.BeginRunCall(ctx, r)
	if err == nil && s.loseStartAck {
		return fresh, errors.New("lost call start acknowledgment")
	}
	return fresh, err
}
func (s runtimeTaskJournalStore) CompleteRunCall(ctx context.Context, r execution.RunCallRequest, c execution.RunCallCompletion) error {
	if s.failReturn {
		return errors.New("call return unavailable")
	}
	err := s.Store.CompleteRunCall(ctx, r, c)
	if err == nil && s.loseReturnAck {
		return errors.New("lost call return acknowledgment")
	}
	return err
}

func TestPreparedTaskRuntimeJournalPersistence(t *testing.T) {
	for _, mode := range []string{"complete", "stream", "lost_start_ack", "failed_return", "lost_return_ack"} {
		t.Run(mode, func(t *testing.T) {
			f, e, client, p, req, admitter := preparedTaskRuntimeFixture(t)
			require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
			require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
			require.NoError(t, engine.WithStore(runtimeTaskJournalStore{Store: f.storage, loseStartAck: mode == "lost_start_ack", failReturn: mode == "failed_return", loseReturnAck: mode == "lost_return_ack"})(e))
			if mode == "stream" {
				client.chunks = []llm.Chunk{{Content: "done", FinishReason: "stop"}}
				events := make(chan engine.StreamEvent, 16)
				require.NoError(t, e.StreamPreparedTask(f.ctx, p, req, events))
				for event := range events {
					require.NotEqual(t, engine.EventError, event.Type)
				}
			} else {
				_, err := e.RunPreparedTask(f.ctx, p, req)
				if mode == "complete" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			}
			calls, err := f.storage.ListRunCalls(f.ctx, execution.RunCallListRequest{RunID: req.Run.ID, Limit: 10})
			require.NoError(t, err)
			require.Len(t, calls, 1)
			require.NoError(t, calls[0].Validate())
			require.Equal(t, req.Claim.ClaimID, calls[0].Request.ClaimID)
			require.Equal(t, req.Run.Provenance.PreparedDigest, calls[0].Request.PreparedDigest)
			if mode == "complete" || mode == "stream" {
				require.NotNil(t, calls[0].Completion)
			} else {
				if mode == "lost_return_ack" {
					require.NotNil(t, calls[0].Completion)
				} else {
					require.Nil(t, calls[0].Completion)
				}
				child, readErr := f.storage.GetRun(f.ctx, req.Run.ID)
				require.NoError(t, readErr)
				require.Empty(t, child.Output)
				require.Zero(t, f.adapter.evaluations)
			}
			if mode == "lost_start_ack" {
				require.Zero(t, client.calls)
			}
			fresh, err := f.storage.BeginRunCall(f.ctx, calls[0].Request)
			require.NoError(t, err)
			require.False(t, fresh)
		})
	}
}

func TestPreparedTaskJournalToolApprovalKeepsOriginSlot(t *testing.T) {
	var operation string
	for _, approval := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "approved_resume"}[approval], func(t *testing.T) {
			client := &requiredToolClient{}
			calls := 0
			var toolMetadata execution.RunCallRequest
			f, e, _, p, req, admitter := preparedTaskRuntimeFixture(t, func(runtime *engine.Engine, overrides *engine.RunOverrides) {
				overrides.Tools = []string{"probe"}
				require.NoError(t, engine.WithLLM(client)(runtime))
				require.NoError(t, engine.WithToolRevision("probe", "1")(runtime))
				require.NoError(t, engine.WithToolAuthorizer(&requiredToolAuthority{approval: approval})(runtime))
				require.NoError(t, engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]())(runtime))
				require.NoError(t, engine.WithTool(llm.Tool{Name: "probe"}, func(ctx context.Context, _ cortex.Invocation) (string, error) {
					calls++
					var ok bool
					toolMetadata, ok = execution.RunCallFromContext(ctx)
					require.True(t, ok)
					return "private result", nil
				})(runtime))
			})
			require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
			require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
			r, err := e.RunPreparedTask(f.ctx, p, req)
			require.NoError(t, err)
			if approval {
				require.Equal(t, run.StatePaused, r.State)
				require.Zero(t, calls)
				before, listErr := f.storage.ListRunCalls(f.ctx, execution.RunCallListRequest{RunID: r.ID, Limit: 10})
				require.NoError(t, listErr)
				require.Len(t, before, 1)
				pending, readErr := e.ListPendingCheckpoints(f.ctx, nil)
				require.NoError(t, readErr)
				require.Len(t, pending, 1)
				require.NoError(t, e.ResolveCheckpoint(f.ctx, pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"}))
				r, err = f.storage.GetRun(f.ctx, r.ID)
				require.NoError(t, err)
			}
			require.Equal(t, verification.StatusPassed, r.VerificationStatus)
			require.Equal(t, 1, calls)
			require.Equal(t, execution.RunCallTool, toolMetadata.Kind)
			if operation == "" {
				operation = toolMetadata.OperationDigest
			} else {
				require.Equal(t, operation, toolMetadata.OperationDigest, "approval resume must retain the original generation slot")
			}
			records, err := f.storage.ListRunCalls(f.ctx, execution.RunCallListRequest{RunID: r.ID, Limit: 10})
			require.NoError(t, err)
			require.Len(t, records, 3)
			for _, record := range records {
				require.NoError(t, record.Validate())
				require.NotNil(t, record.Completion)
			}
		})
	}
}

// A task-run child is created through coordination, whose request is
// frozen as JSON, and run.Run.Principal never reaches JSON. So the child
// carries no principal bytes, and after a pause it resumes with a nil
// principal: never the decider's, and not the worker's either. This pins
// that documented exception until coordination carries the bytes.
func TestPreparedTaskChildResumesWithoutStoredPrincipal(t *testing.T) {
	client := &requiredToolClient{}
	var seen []any
	f, e, _, p, req, admitter := preparedTaskRuntimeFixture(t, func(runtime *engine.Engine, overrides *engine.RunOverrides) {
		overrides.Tools = []string{"probe"}
		require.NoError(t, engine.WithLLM(client)(runtime))
		require.NoError(t, engine.WithToolRevision("probe", "1")(runtime))
		require.NoError(t, engine.WithToolAuthorizer(&requiredToolAuthority{approval: true})(runtime))
		require.NoError(t, engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]())(runtime))
		require.NoError(t, engine.WithTool(llm.Tool{Name: "probe"}, func(_ context.Context, inv cortex.Invocation) (string, error) {
			seen = append(seen, inv.Principal)
			return "private result", nil
		})(runtime))
	})
	require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
	require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
	r, err := e.RunPreparedTask(cortex.WithPrincipal(f.ctx, "worker"), p, req)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, r.State)
	stored, err := f.storage.GetRun(f.ctx, r.ID)
	require.NoError(t, err)
	require.Empty(t, stored.Principal)

	pending, err := e.ListPendingCheckpoints(f.ctx, nil)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, e.ResolveCheckpoint(cortex.WithPrincipal(f.ctx, "operator"), pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"}))
	require.Len(t, seen, 1)
	require.Nil(t, seen[0])
}
