package engine

import (
	"context"
	"errors"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/suspension"
)

type taskGuardFunc func(context.Context, TaskCallRequest) (TaskCallLease, error)

func (f taskGuardFunc) AcquireTaskCall(ctx context.Context, r TaskCallRequest) (TaskCallLease, error) {
	return f(ctx, r)
}

type taskCallClient struct {
	complete func(context.Context, *llm.Request) (*llm.Response, error)
	stream   func(context.Context, *llm.Request) (llm.Stream, error)
}

func (c taskCallClient) Complete(ctx context.Context, r *llm.Request) (*llm.Response, error) {
	return c.complete(ctx, r)
}
func (c taskCallClient) CompleteStream(ctx context.Context, r *llm.Request) (llm.Stream, error) {
	return c.stream(ctx, r)
}

type taskCallStream struct {
	next   func(context.Context) (*llm.Chunk, error)
	closes atomic.Int32
}

func (s *taskCallStream) Next(ctx context.Context) (*llm.Chunk, error) { return s.next(ctx) }
func (s *taskCallStream) Close() error                                 { s.closes.Add(1); return nil }
func (s *taskCallStream) Usage() *llm.Usage                            { return &llm.Usage{TotalTokens: 7} }

func taskCallFixture(t *testing.T) (context.Context, *run.Run) {
	t.Helper()
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), Scope: scopeA(), Budget: execution.Budget{Deadline: time.Now().Add(time.Minute)}, Provenance: run.ExecutionProvenance{TaskClaimID: id.New("tclaim").String(), AssignmentDigest: evidence.Digest([]byte("handoff")), ParentRunID: id.NewAgentRunID().String(), PreparedDigest: evidence.Digest([]byte("prepared"))}}
	ctx, cancel := context.WithDeadline(cortex.WithScope(context.Background(), r.Scope), r.Budget.Deadline)
	t.Cleanup(cancel)
	return withTaskCallIndex(withExecutionProvenance(ctx, r.Provenance), 0), r
}

func TestTaskCallGenerationRequiresLease(t *testing.T) {
	for _, mode := range []string{"missing", "denied", "expired", "unbounded", "wrong_binding", "missing_release", "missing_revocation", "revoked", "release_error", "valid"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r := taskCallFixture(t)
			calls, releases := 0, 0
			guard := taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
				_, digest, err := req.Freeze()
				require.NoError(t, err)
				require.Equal(t, "complete", string(req.Kind))
				revoked := make(chan struct{})
				lease := TaskCallLease{RequestDigest: digest, ExpiresAt: time.Now().Add(30 * time.Second), Revoked: revoked, Release: func() error {
					releases++
					if mode == "release_error" {
						return errors.New("failed release")
					}
					return nil
				}}
				switch mode {
				case "denied":
					return TaskCallLease{}, errors.New("private policy detail")
				case "expired":
					lease.ExpiresAt = time.Now().Add(-time.Second)
				case "unbounded":
					lease.ExpiresAt = r.Budget.Deadline.Add(time.Second)
				case "wrong_binding":
					lease.RequestDigest = evidence.Digest(nil)
				case "missing_release":
					lease.Release = nil
				case "missing_revocation":
					lease.Revoked = nil
				case "revoked":
					close(revoked)
				}
				req.Payload[0] = '!'
				return lease, nil
			})
			e := &Engine{store: newTaskCallJournalStub(), llm: taskCallClient{complete: func(_ context.Context, req *llm.Request) (*llm.Response, error) {
				calls++
				require.Equal(t, "work", req.Messages[0].Content)
				return &llm.Response{Content: "done", Usage: llm.Usage{TotalTokens: 7}}, nil
			}}, taskCallGuard: guard}
			if mode == "missing" {
				e.taskCallGuard = nil
			}
			response, err := e.completeTaskCall(ctx, r, &llm.Request{Model: "model", Messages: []llm.Message{{Role: "user", Content: "work"}}})
			if mode == "valid" {
				require.NoError(t, err)
				require.Equal(t, "done", response.Content)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "private policy detail")
			}
			if mode == "valid" || mode == "release_error" {
				require.Equal(t, 1, calls)
				require.NotNil(t, response)
				require.Equal(t, 7, response.Usage.TotalTokens)
			} else {
				require.Zero(t, calls)
			}
			if mode == "missing" || mode == "denied" || mode == "missing_release" {
				require.Zero(t, releases)
			} else {
				require.Equal(t, 1, releases)
			}
		})
	}
}

func TestTaskCallLeaseRevokesInflightGeneration(t *testing.T) {
	ctx, r := taskCallFixture(t)
	revoked, entered := make(chan struct{}), make(chan struct{})
	var releases atomic.Int32
	e := &Engine{store: newTaskCallJournalStub(), taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
		_, digest, err := req.Freeze()
		require.NoError(t, err)
		return TaskCallLease{RequestDigest: digest, ExpiresAt: r.Budget.Deadline, Revoked: revoked, Release: func() error { releases.Add(1); return nil }}, nil
	}), llm: taskCallClient{complete: func(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}}}
	done := make(chan error, 1)
	go func() { _, err := e.completeTaskCall(ctx, r, &llm.Request{Model: "model"}); done <- err }()
	<-entered
	close(revoked)
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("revocation did not cancel provider")
	}
	require.EqualValues(t, 1, releases.Load())
}

func TestTaskCallStreamRetainsLeaseThroughConsumption(t *testing.T) {
	ctx, r := taskCallFixture(t)
	releases := 0
	source := &taskCallStream{next: func(context.Context) (*llm.Chunk, error) { return nil, io.EOF }}
	e := &Engine{store: newTaskCallJournalStub(), taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
		_, digest, err := req.Freeze()
		require.NoError(t, err)
		return TaskCallLease{RequestDigest: digest, ExpiresAt: r.Budget.Deadline, Revoked: make(chan struct{}), Release: func() error { releases++; return nil }}, nil
	}), llm: taskCallClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return source, nil }}}
	stream, err := e.streamTaskCall(ctx, r, &llm.Request{Model: "model"})
	require.NoError(t, err)
	require.Zero(t, releases)
	_, err = stream.Next(ctx)
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, 1, releases)
	require.Equal(t, 7, stream.Usage().TotalTokens)
	require.NoError(t, stream.Close())
	require.Equal(t, 1, releases)
	require.EqualValues(t, 1, source.closes.Load())
}

func TestTaskCallStreamReleaseFailureIsNotEOF(t *testing.T) {
	ctx, r := taskCallFixture(t)
	source := &taskCallStream{next: func(context.Context) (*llm.Chunk, error) { return nil, io.EOF }}
	e := &Engine{store: newTaskCallJournalStub(), taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
		_, digest, err := req.Freeze()
		require.NoError(t, err)
		return TaskCallLease{RequestDigest: digest, ExpiresAt: r.Budget.Deadline, Revoked: make(chan struct{}), Release: func() error { return errors.New("release failed") }}, nil
	}), llm: taskCallClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return source, nil }}}
	stream, err := e.streamTaskCall(ctx, r, &llm.Request{Model: "model"})
	require.NoError(t, err)
	_, err = stream.Next(ctx)
	require.ErrorIs(t, err, cortex.ErrTaskCallUncertain)
	require.False(t, errors.Is(err, io.EOF), "the ReAct loop treats EOF as successful transport completion")
}

func TestTaskCallToolDispatchAndResumeRequireCurrentLease(t *testing.T) {
	for _, mode := range []string{"missing", "denied", "valid", "release_error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r := taskCallFixture(t)
			calls, releases := 0, 0
			e := &Engine{store: newTaskCallJournalStub(), tools: []registeredTool{{def: llm.Tool{Name: "probe"}, handler: func(context.Context, cortex.Invocation) (string, error) { calls++; return "result", nil }}}}
			if mode != "missing" {
				e.taskCallGuard = taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
					if mode == "denied" {
						return TaskCallLease{}, errors.New("revoked")
					}
					require.Equal(t, TaskCallTool, req.Kind)
					_, digest, err := req.Freeze()
					require.NoError(t, err)
					return TaskCallLease{RequestDigest: digest, ExpiresAt: r.Budget.Deadline, Revoked: make(chan struct{}), Release: func() error {
						releases++
						if mode == "release_error" {
							return errors.New("release failed")
						}
						return nil
					}}, nil
				})
			}
			result, outcome, err := e.dispatchTool(ctx, cortex.Subject{Scope: r.Scope, RunID: r.ID, AgentID: r.AgentID}, llm.ToolCall{ID: "call", Name: "probe", Arguments: `{}`})
			if mode == "valid" {
				require.NoError(t, err)
				require.Equal(t, "result", result)
				require.Equal(t, outcomeCompleted, outcome)
			} else {
				require.Error(t, err)
				require.Empty(t, result)
			}
			if mode == "release_error" {
				require.Equal(t, outcomeUncertain, outcome)
			}
			if mode == "valid" || mode == "release_error" {
				require.Equal(t, 1, calls)
				require.Equal(t, 1, releases)
			} else {
				require.Zero(t, calls)
				require.Zero(t, releases)
			}
		})
	}
}

func TestTaskCallStreamRevocationCancelsConsumption(t *testing.T) {
	ctx, r := taskCallFixture(t)
	revoked, entered := make(chan struct{}), make(chan struct{})
	var releases atomic.Int32
	source := &taskCallStream{next: func(ctx context.Context) (*llm.Chunk, error) { close(entered); <-ctx.Done(); return nil, io.EOF }}
	e := &Engine{store: newTaskCallJournalStub(), taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
		_, digest, err := req.Freeze()
		require.NoError(t, err)
		return TaskCallLease{RequestDigest: digest, ExpiresAt: r.Budget.Deadline, Revoked: revoked, Release: func() error { releases.Add(1); return nil }}, nil
	}), llm: taskCallClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return source, nil }}}
	stream, err := e.streamTaskCall(ctx, r, &llm.Request{Model: "model"})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, nextErr := stream.Next(ctx); done <- nextErr }()
	<-entered
	close(revoked)
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("stream did not cancel")
	}
	require.EqualValues(t, 1, releases.Load())
	require.EqualValues(t, 1, source.closes.Load())
}

func TestTaskCallLeaseReleasedOnProviderPanic(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		ctx, r := taskCallFixture(t)
		releases := 0
		e := &Engine{store: newTaskCallJournalStub(), taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
			_, digest, err := req.Freeze()
			require.NoError(t, err)
			return TaskCallLease{RequestDigest: digest, ExpiresAt: r.Budget.Deadline, Revoked: make(chan struct{}), Release: func() error { releases++; return nil }}, nil
		}), llm: taskCallClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) { panic("provider") }, stream: func(context.Context, *llm.Request) (llm.Stream, error) { panic("provider") }}}
		require.Panics(t, func() {
			if streaming {
				_, _ = e.streamTaskCall(ctx, r, &llm.Request{Model: "model"})
			} else {
				_, _ = e.completeTaskCall(ctx, r, &llm.Request{Model: "model"})
			}
		})
		require.Equal(t, 1, releases)
	}
}

func TestTaskCallLoopsDoNotBypassMissingGuard(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		ctx, r := taskCallFixture(t)
		storage := scopespy.New()
		calls := 0
		e, err := New(WithStore(storage), WithLLM(taskCallClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
			calls++
			return &llm.Response{Content: "unguarded", FinishReason: "stop"}, nil
		}, stream: func(context.Context, *llm.Request) (llm.Stream, error) {
			calls++
			return &taskCallStream{next: func(context.Context) (*llm.Chunk, error) { return nil, io.EOF }}, nil
		}}))
		require.NoError(t, err)
		require.NoError(t, storage.CreateRun(ctx, r))
		ag := &agent.Config{ID: r.AgentID, Name: "task"}
		cfg := resolvedConfig{MaxSteps: 1, Model: "model", Budget: r.Budget, Publication: e.frozenPublication()}
		state := &reactState{messages: []llm.Message{{Role: "user", Content: "work"}}, provenance: r.Provenance}
		if streaming {
			events := make(chan StreamEvent, 16)
			e.continueStreamReAct(ctx, ag, cfg, r, state, time.Now(), events)
		} else {
			_, err = e.continueReAct(ctx, ag, cfg, r, state, time.Now())
			require.Error(t, err)
		}
		require.Zero(t, calls)
		require.NotEqual(t, run.StateCompleted, r.State)
	}
}

func TestTaskCallRequestRejectsInvalidIdentity(t *testing.T) {
	ctx, r := taskCallFixture(t)
	valid, err := taskCallRequest(ctx, r.Scope, r.ID, r.AgentID, r.Provenance, r.Budget.Deadline, TaskCallComplete, &llm.Request{Model: "model"})
	require.NoError(t, err)
	for name, change := range map[string]func(*TaskCallRequest){
		"schema":   func(r *TaskCallRequest) { r.SchemaVersion++ },
		"scope":    func(r *TaskCallRequest) { r.Scope = cortex.Scope{} },
		"claim":    func(r *TaskCallRequest) { r.ClaimID = id.NewAgentID() },
		"run":      func(r *TaskCallRequest) { r.RunID = r.ParentRunID },
		"agent":    func(r *TaskCallRequest) { r.AgentID = id.NewAgentRunID() },
		"handoff":  func(r *TaskCallRequest) { r.AssignmentDigest = "" },
		"prepared": func(r *TaskCallRequest) { r.PreparedDigest = "" },
		"kind":     func(r *TaskCallRequest) { r.Kind = "other" },
		"payload":  func(r *TaskCallRequest) { r.Payload = []byte("invalid") },
		"deadline": func(r *TaskCallRequest) { r.Deadline = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			req, _, freezeErr := valid.Freeze()
			require.NoError(t, freezeErr)
			change(&req)
			frozen, digest, freezeErr := req.Freeze()
			require.Error(t, freezeErr)
			require.Zero(t, frozen)
			require.Empty(t, digest)
		})
	}
	var absent taskGuardFunc
	_, err = New(WithTaskCallGuard(absent))
	require.Error(t, err)
	_, err = New(WithTaskCallGuard(nil))
	require.Error(t, err)
}

func TestTaskCallLegacyContinuationCannotDropMarker(t *testing.T) {
	_, r := taskCallFixture(t)
	r.Provenance.PreparedDigest = ""
	require.ErrorIs(t, validateFrozenContinuation(suspension.Continuation{}, nil, r), cortex.ErrInvalidContinuation)
	_, _, err := decodeContinuation(suspension.Continuation{Provenance: run.ExecutionProvenance{TaskClaimID: r.Provenance.TaskClaimID}})
	require.ErrorIs(t, err, cortex.ErrInvalidContinuation)
}

func TestTaskCallStreamNextPanicReleasesLease(t *testing.T) {
	ctx, r := taskCallFixture(t)
	releases := 0
	source := &taskCallStream{next: func(context.Context) (*llm.Chunk, error) { panic("provider next") }}
	e := &Engine{store: newTaskCallJournalStub(), taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
		_, digest, err := req.Freeze()
		require.NoError(t, err)
		return TaskCallLease{RequestDigest: digest, ExpiresAt: r.Budget.Deadline, Revoked: make(chan struct{}), Release: func() error { releases++; return nil }}, nil
	}), llm: taskCallClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return source, nil }}}
	stream, err := e.streamTaskCall(ctx, r, &llm.Request{Model: "model"})
	require.NoError(t, err)
	require.Panics(t, func() { _, _ = stream.Next(ctx) })
	require.Equal(t, 1, releases)
	require.EqualValues(t, 1, source.closes.Load())
}

type taskCallPanicGate struct{ boundaryGate }

func (*taskCallPanicGate) NewSegmentChecker(context.Context, safety.PublicationRequest) (safety.SegmentChecker, error) {
	panic("segment checker opening")
}

func TestTaskCallStreamScannerPanicReleasesLease(t *testing.T) {
	ctx, r := taskCallFixture(t)
	releases := 0
	source := &taskCallStream{next: func(context.Context) (*llm.Chunk, error) { return nil, io.EOF }}
	e, err := New(WithStore(newTaskCallJournalStub()), WithPublicationGate(&taskCallPanicGate{}), WithPublicationPolicy(safety.Policy{Mode: safety.ModeCheckedSegments}), WithLLM(taskCallClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return source, nil }}), WithTaskCallGuard(taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
		_, digest, freezeErr := req.Freeze()
		require.NoError(t, freezeErr)
		return TaskCallLease{RequestDigest: digest, ExpiresAt: r.Budget.Deadline, Revoked: make(chan struct{}), Release: func() error { releases++; return nil }}, nil
	})))
	require.NoError(t, err)
	ag := &agent.Config{ID: r.AgentID, Name: "task"}
	cfg := resolvedConfig{MaxSteps: 1, Model: "model", Budget: r.Budget, Publication: e.frozenPublication()}
	state := &reactState{messages: []llm.Message{{Role: "user", Content: "work"}}, provenance: r.Provenance}
	require.Panics(t, func() { e.continueStreamReAct(ctx, ag, cfg, r, state, time.Now(), make(chan StreamEvent, 16)) })
	require.Equal(t, 1, releases)
	require.EqualValues(t, 1, source.closes.Load())
}

func TestTaskCallSynchronousRevocationCannotReturnSuccess(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	for _, kind := range []TaskCallKind{TaskCallComplete, TaskCallStream, TaskCallTool, "stream_next"} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, r := taskCallFixture(t)
			revoked := make(chan struct{})
			releases := 0
			e := &Engine{store: newTaskCallJournalStub(), taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
				_, digest, err := req.Freeze()
				require.NoError(t, err)
				return TaskCallLease{RequestDigest: digest, ExpiresAt: r.Budget.Deadline, Revoked: revoked, Release: func() error { releases++; return nil }}, nil
			}), llm: taskCallClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
				close(revoked)
				return &llm.Response{Content: "late success"}, nil
			}, stream: func(context.Context, *llm.Request) (llm.Stream, error) {
				if kind == TaskCallStream {
					close(revoked)
				}
				return &taskCallStream{next: func(context.Context) (*llm.Chunk, error) {
					close(revoked)
					return nil, io.EOF
				}}, nil
			}}, tools: []registeredTool{{def: llm.Tool{Name: "probe"}, handler: func(context.Context, cortex.Invocation) (string, error) {
				close(revoked)
				return "late success", nil
			}}}}
			var err error
			switch kind {
			case TaskCallComplete:
				_, err = e.completeTaskCall(ctx, r, &llm.Request{Model: "model"})
			case TaskCallStream, "stream_next":
				var stream llm.Stream
				stream, err = e.streamTaskCall(ctx, r, &llm.Request{Model: "model"})
				if stream != nil {
					if kind == "stream_next" {
						_, err = stream.Next(ctx)
					}
					_ = stream.Close()
				}
			case TaskCallTool:
				var result string
				var outcome toolOutcome
				result, outcome, err = e.dispatchTool(ctx, cortex.Subject{Scope: r.Scope, RunID: r.ID, AgentID: r.AgentID}, llm.ToolCall{ID: "call", Name: "probe", Arguments: `{}`})
				require.Empty(t, result)
				require.Equal(t, outcomeUncertain, outcome)
			}
			if kind == TaskCallTool {
				require.ErrorIs(t, err, cortex.ErrTaskCallUncertain)
			} else {
				require.ErrorIs(t, err, context.Canceled)
			}
			require.Equal(t, 1, releases)
		})
	}
}
