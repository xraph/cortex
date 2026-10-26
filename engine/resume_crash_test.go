package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

const resumeCrashBoundaryEnv = "CORTEX_TEST_RESUME_CRASH_BOUNDARY"
const resumeCrashManifestEnv = "CORTEX_TEST_RESUME_CRASH_MANIFEST"

type resumeCrashManifest struct {
	Database      string
	Scope         cortex.Scope
	RunID         id.AgentRunID
	PauseID       id.SuspensionID
	ProviderCalls int
}

// TestAgentReplyExecutionCrashWorker parks inside the resumed provider call.
func TestAgentReplyExecutionCrashWorker(t *testing.T) {
	if os.Getenv(resumeCrashBoundaryEnv) == "" {
		return
	}
	output := os.Getenv(resumeCrashManifestEnv)
	require.NotEmpty(t, output)
	database := filepath.Join(filepath.Dir(output), "resume.db")
	f := newPreparedTeamFixtureAt(t, orchestration.StrategySequential, database)
	e, err := engine.New(engine.WithStore(f.s), engine.WithLLM(f.client), engine.WithA2A(a2a.Options{}))
	require.NoError(t, err)
	var original id.AgentRunID
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		if f.client.calls == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "ask-b", Name: "agent_ask", Arguments: `{"to":"b","content":"check"}`}}, FinishReason: "tool_calls"}, nil
		}
		if f.client.calls == 3 {
			admitted, readErr := f.s.LatestResumeAdmission(f.ctx, original)
			require.NoError(t, readErr)
			_, readErr = f.s.GetSuspension(f.ctx, original)
			require.ErrorIs(t, readErr, cortex.ErrNotSuspended)
			data, encodeErr := json.Marshal(resumeCrashManifest{Database: database, Scope: cortex.ScopeFromContext(f.ctx), RunID: original, PauseID: admitted.Request.Version.SuspensionID, ProviderCalls: f.client.calls})
			require.NoError(t, encodeErr)
			require.NoError(t, os.WriteFile(output+".tmp", data, 0600))
			require.NoError(t, os.Rename(output+".tmp", output))
			timer := time.NewTimer(time.Minute)
			defer timer.Stop()
			<-timer.C
			t.Fatal("parent did not kill resumed provider worker")
		}
		return &llm.Response{Content: "answer", FinishReason: "stop"}, nil
	}
	paused, err := e.RunAgent(f.ctx, "a", "ask", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)
	original = paused.ID
	_, err = e.A2A().Drain(f.ctx)
	require.NoError(t, err)
	t.Fatal("resumed worker did not reach provider boundary")
}

func TestAgentReplyRecoveryAfterExecutionProcessCrash(t *testing.T) {
	manifest := killCrashWorkerAtBoundary(t, "provider_pending", crashWorkerConfig{Test: "TestAgentReplyExecutionCrashWorker", BoundaryEnv: resumeCrashBoundaryEnv, ManifestEnv: resumeCrashManifestEnv}, func(m resumeCrashManifest) {
		require.Equal(t, 3, m.ProviderCalls)
		require.False(t, m.RunID.IsNil())
		require.False(t, m.PauseID.IsNil())
	})
	storage := openSQLiteStoreForTest(t, manifest.Database+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	ctx := cortex.WithScope(t.Context(), manifest.Scope)
	before, err := storage.GetRun(ctx, manifest.RunID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, before.State)
	_, err = storage.GetSuspension(ctx, manifest.RunID)
	require.ErrorIs(t, err, cortex.ErrNotSuspended)
	start, err := storage.GetResumeStart(ctx, manifest.RunID, manifest.PauseID)
	require.NoError(t, err)
	source, err := coordination.NewScopedAgentReplySource(storage, []cortex.Scope{manifest.Scope}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	var calls, uncertain atomic.Int32
	client := &preparedTeamClient{complete: func(context.Context, *llm.Request, int) (*llm.Response, error) {
		calls.Add(1)
		return &llm.Response{Content: "unexpected replay"}, nil
	}}
	fresh, err := engine.New(engine.WithStore(storage), engine.WithLLM(client), engine.WithAgentReplyRecovery(engine.AgentReplyRecoveryConfig{
		Service: coordination.AgentReplyRecoveryConfig{Discovery: source, Interval: 10 * time.Millisecond, ReportError: func(recoveryErr error) {
			if errors.Is(recoveryErr, suspension.ErrResumeAlreadyStarted) {
				uncertain.Add(1)
			}
		}},
		Authorize: func(_ context.Context, key id.AgentRunID) error { require.Equal(t, manifest.RunID, key); return nil },
	}))
	require.NoError(t, err)
	require.NoError(t, fresh.Start(ctx))
	t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
	require.Eventually(t, func() bool { return uncertain.Load() > 0 }, 10*time.Second, 10*time.Millisecond)
	require.NoError(t, fresh.Stop(context.Background()))
	require.Zero(t, calls.Load())
	after, err := storage.GetRun(ctx, manifest.RunID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	retained, err := storage.GetResumeStart(ctx, manifest.RunID, manifest.PauseID)
	require.NoError(t, err)
	require.Equal(t, start, retained)
}
