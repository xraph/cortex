package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/store/scopespy"
)

// Lease unit tests use an explicit journal port. Real persistence is exercised
// by the prepared-task runtime tests and the three backend conformance suites.
type taskCallJournalStub struct {
	*scopespy.Spy
	execution.RunCallStore
	begin    func(context.Context, execution.RunCallRequest) (bool, error)
	complete func(context.Context, execution.RunCallRequest, execution.RunCallCompletion) error
}

func newTaskCallJournalStub() *taskCallJournalStub { return &taskCallJournalStub{Spy: scopespy.New()} }
func (s *taskCallJournalStub) BeginRunCall(ctx context.Context, r execution.RunCallRequest) (bool, error) {
	if s.begin != nil {
		return s.begin(ctx, r)
	}
	return true, nil
}
func (s *taskCallJournalStub) CompleteRunCall(ctx context.Context, r execution.RunCallRequest, c execution.RunCallCompletion) error {
	if s.complete != nil {
		return s.complete(ctx, r, c)
	}
	return nil
}

func TestTaskCallJournalRequiresConfirmedFreshStart(t *testing.T) {
	for _, mode := range []string{"missing", "historical", "write_error", "return_error", "success"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r := taskCallFixture(t)
			calls, releases, completions := 0, 0, 0
			journal := newTaskCallJournalStub()
			journal.begin = func(_ context.Context, req execution.RunCallRequest) (bool, error) {
				require.Equal(t, r.ID, req.RunID)
				if mode == "write_error" {
					return true, errors.New("private storage failure")
				}
				return mode != "historical", nil
			}
			journal.complete = func(_ context.Context, req execution.RunCallRequest, c execution.RunCallCompletion) error {
				completions++
				require.NoError(t, c.Validate(req))
				require.EqualValues(t, 7, c.ReportedTokens)
				if mode == "return_error" {
					return errors.New("private storage failure")
				}
				return nil
			}
			e := &Engine{store: journal, taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
				_, digest, err := req.Freeze()
				require.NoError(t, err)
				return TaskCallLease{RequestDigest: digest, ExpiresAt: req.Deadline, Revoked: make(chan struct{}), Release: func() error { releases++; return nil }}, nil
			}), llm: taskCallClient{complete: func(callCtx context.Context, _ *llm.Request) (*llm.Response, error) {
				calls++
				metadata, ok := execution.RunCallFromContext(callCtx)
				require.True(t, ok)
				require.Equal(t, r.ID, metadata.RunID)
				return &llm.Response{Content: "done", Usage: llm.Usage{TotalTokens: 7}}, nil
			}}}
			if mode == "missing" {
				e.store = scopespy.New()
			}
			_, err := e.completeTaskCall(ctx, r, &llm.Request{Model: "model"})
			if mode == "success" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "private")
			}
			if mode == "success" || mode == "return_error" {
				require.Equal(t, 1, calls)
				require.Equal(t, 1, completions)
			} else {
				require.Zero(t, calls)
				require.Zero(t, completions)
			}
			if mode != "missing" {
				require.Equal(t, 1, releases)
			}
			if mode == "return_error" {
				require.ErrorIs(t, err, cortex.ErrTaskCallUncertain)
			}
		})
	}
}

func TestTaskCallJournalStableSlots(t *testing.T) {
	ctx, _ := taskCallFixture(t)
	complete, err := taskCallOperation(ctx, TaskCallComplete, &llm.Request{})
	require.NoError(t, err)
	stream, err := taskCallOperation(ctx, TaskCallStream, &llm.Request{})
	require.NoError(t, err)
	require.Equal(t, complete, stream)
	next, err := taskCallOperation(withTaskCallIndex(ctx, 1), TaskCallComplete, &llm.Request{})
	require.NoError(t, err)
	require.NotEqual(t, complete, next)
	tool, err := taskCallOperation(ctx, TaskCallTool, llm.ToolCall{ID: "call-1", Name: "first"})
	require.NoError(t, err)
	changed, err := taskCallOperation(ctx, TaskCallTool, llm.ToolCall{ID: "call-1", Name: "changed", Arguments: "changed"})
	require.NoError(t, err)
	require.Equal(t, tool, changed, "changing payload cannot bypass a prior reservation")
	require.NotEqual(t, complete, tool)
	_, err = taskCallOperation(context.Background(), TaskCallComplete, &llm.Request{})
	require.ErrorIs(t, err, cortex.ErrTaskCallUnavailable)
	_, err = taskCallOperation(withTaskCallIndex(ctx, -1), TaskCallTool, llm.ToolCall{ID: "call-1"})
	require.ErrorIs(t, err, cortex.ErrTaskCallUnavailable)
	_, err = taskCallOperation(ctx, TaskCallTool, llm.ToolCall{})
	require.ErrorIs(t, err, cortex.ErrTaskCallUnavailable)
}

type journalCloseStream struct {
	*taskCallStream
	closeError bool
}

func (s *journalCloseStream) Close() error {
	_ = s.taskCallStream.Close()
	if s.closeError {
		return errors.New("private close failure")
	}
	return nil
}

func TestTaskCallJournalStreamCompletionBoundary(t *testing.T) {
	for _, mode := range []string{"eof", "early_close", "provider_error", "joined_eof", "cancel", "close_error", "persist_error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r := taskCallFixture(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			completions, releases, reads := 0, 0, 0
			journal := newTaskCallJournalStub()
			chunk := &llm.Chunk{Content: "private candidate"}
			digest := sha256.New()
			require.NoError(t, json.NewEncoder(digest).Encode(chunk))
			journal.complete = func(_ context.Context, req execution.RunCallRequest, c execution.RunCallCompletion) error {
				completions++
				require.NoError(t, c.Validate(req))
				require.Equal(t, hex.EncodeToString(digest.Sum(nil)), c.ResultDigest)
				if mode == "persist_error" {
					return errors.New("private write failure")
				}
				return nil
			}
			source := &journalCloseStream{taskCallStream: &taskCallStream{next: func(context.Context) (*llm.Chunk, error) {
				reads++
				if reads == 1 {
					return chunk, nil
				}
				if mode == "provider_error" {
					return nil, errors.New("provider failed")
				}
				if mode == "joined_eof" {
					return nil, errors.Join(io.EOF, errors.New("failed"))
				}
				return nil, io.EOF
			}}, closeError: mode == "close_error"}
			e := &Engine{store: journal, taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
				_, binding, err := req.Freeze()
				require.NoError(t, err)
				return TaskCallLease{RequestDigest: binding, ExpiresAt: req.Deadline, Revoked: make(chan struct{}), Release: func() error { releases++; return nil }}, nil
			}), llm: taskCallClient{stream: func(context.Context, *llm.Request) (llm.Stream, error) { return source, nil }}}
			stream, err := e.streamTaskCall(ctx, r, &llm.Request{Model: "model"})
			require.NoError(t, err)
			_, err = stream.Next(ctx)
			require.NoError(t, err)
			require.Zero(t, completions)
			if mode == "early_close" {
				require.NoError(t, stream.Close())
			} else {
				if mode == "cancel" {
					cancel()
				}
				_, err = stream.Next(ctx)
				if mode == "eof" {
					require.ErrorIs(t, err, io.EOF)
				} else {
					require.Error(t, err)
					require.NotErrorIs(t, err, io.EOF)
				}
			}
			_ = stream.Close()
			require.Equal(t, 1, releases)
			require.EqualValues(t, 1, source.closes.Load())
			if mode == "eof" || mode == "persist_error" {
				require.Equal(t, 1, completions)
			} else {
				require.Zero(t, completions)
			}
		})
	}
}

func TestTaskCallJournalWriteWithdrawalAndPanicReleaseLease(t *testing.T) {
	for _, mode := range []string{"cancel", "revoke", "panic"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r := taskCallFixture(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			revoked := make(chan struct{})
			releases, calls := 0, 0
			journal := newTaskCallJournalStub()
			journal.begin = func(context.Context, execution.RunCallRequest) (bool, error) {
				switch mode {
				case "cancel":
					cancel()
				case "revoke":
					close(revoked)
				case "panic":
					panic("journal panic")
				}
				return true, nil
			}
			e := &Engine{store: journal, taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
				_, digest, err := req.Freeze()
				require.NoError(t, err)
				return TaskCallLease{RequestDigest: digest, ExpiresAt: req.Deadline, Revoked: revoked, Release: func() error { releases++; return nil }}, nil
			}), llm: taskCallClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) { calls++; return &llm.Response{}, nil }}}
			invoke := func() { _, err := e.completeTaskCall(ctx, r, &llm.Request{Model: "model"}); require.Error(t, err) }
			if mode == "panic" {
				require.Panics(t, invoke)
			} else {
				invoke()
			}
			require.Zero(t, calls)
			require.Equal(t, 1, releases)
		})
	}
}

func TestTaskCallJournalToolFailureStopsFurtherEffects(t *testing.T) {
	for _, mode := range []string{"historical", "return_failure", "tool_failure", "success"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r := taskCallFixture(t)
			calls := 0
			journal := newTaskCallJournalStub()
			journal.begin = func(context.Context, execution.RunCallRequest) (bool, error) { return mode != "historical", nil }
			journal.complete = func(_ context.Context, req execution.RunCallRequest, c execution.RunCallCompletion) error {
				require.NoError(t, c.Validate(req))
				require.False(t, c.TokensReported)
				if mode == "return_failure" {
					return errors.New("private write failure")
				}
				return nil
			}
			e := &Engine{store: journal, taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
				_, digest, err := req.Freeze()
				require.NoError(t, err)
				return TaskCallLease{RequestDigest: digest, ExpiresAt: req.Deadline, Revoked: make(chan struct{}), Release: func() error { return nil }}, nil
			}), tools: []registeredTool{{def: llm.Tool{Name: "probe"}, handler: func(context.Context, cortex.Invocation) (string, error) {
				calls++
				if mode == "tool_failure" {
					return "partial result", errors.New("failed")
				}
				return "result", nil
			}}}}
			result, outcome, err := e.dispatchTool(ctx, cortex.Subject{Scope: r.Scope, RunID: r.ID, AgentID: r.AgentID}, llm.ToolCall{ID: "tool", Name: "probe", Arguments: `{}`})
			if mode == "success" {
				require.NoError(t, err)
				require.Equal(t, "result", result)
				require.Equal(t, outcomeCompleted, outcome)
			} else {
				require.ErrorIs(t, err, cortex.ErrTaskCallUncertain)
				require.Empty(t, result)
				require.Equal(t, outcomeUncertain, outcome)
			}
			if mode == "historical" {
				require.Zero(t, calls)
			} else {
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestTaskCallJournalBindsExactLeaseAndDetachesStorageInput(t *testing.T) {
	ctx, r := taskCallFixture(t)
	deadline := time.Now().Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	journal := newTaskCallJournalStub()
	var leaseDigest string
	var started execution.RunCallRequest
	journal.begin = func(_ context.Context, req execution.RunCallRequest) (bool, error) {
		require.Equal(t, leaseDigest, req.RequestDigest)
		require.Equal(t, deadline.UnixNano(), req.Deadline.UnixNano())
		var err error
		started, _, err = req.Freeze()
		require.NoError(t, err)
		req.Scope.Levels[0].Value = "storage mutation"
		return true, nil
	}
	journal.complete = func(_ context.Context, req execution.RunCallRequest, completion execution.RunCallCompletion) error {
		require.Equal(t, started, req)
		require.NoError(t, completion.Validate(req))
		return nil
	}
	e := &Engine{store: journal, taskCallGuard: taskGuardFunc(func(_ context.Context, req TaskCallRequest) (TaskCallLease, error) {
		require.Equal(t, deadline.UnixNano(), req.Deadline.UnixNano())
		var err error
		_, leaseDigest, err = req.Freeze()
		require.NoError(t, err)
		return TaskCallLease{RequestDigest: leaseDigest, ExpiresAt: req.Deadline, Revoked: make(chan struct{}), Release: func() error { return nil }}, nil
	}), llm: taskCallClient{complete: func(callCtx context.Context, _ *llm.Request) (*llm.Response, error) {
		metadata, ok := execution.RunCallFromContext(callCtx)
		require.True(t, ok)
		require.Equal(t, started, metadata)
		raw, err := json.Marshal(metadata)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "PRIVATE-PROMPT")
		return &llm.Response{Content: "done"}, nil
	}}}
	_, err := e.completeTaskCall(ctx, r, &llm.Request{Model: "model", System: "PRIVATE-PROMPT"})
	require.NoError(t, err)
}
