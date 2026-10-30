package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

const (
	runReclaimBoundaryEnv = "CORTEX_TEST_RUN_RECLAIM_BOUNDARY"
	runReclaimManifestEnv = "CORTEX_TEST_RUN_RECLAIM_MANIFEST"
)

// runReclaimManifest is what the child writes once it is parked at its
// boundary, before the parent kills it.
type runReclaimManifest struct {
	Boundary      string
	Database      string
	RunID         string
	ProviderCalls int
	EffectLog     string
}

// crashModel answers step 0 with one call to tool, and every later step
// with a final answer. Before answering call park it runs park instead,
// which never returns.
type crashModel struct {
	mu     sync.Mutex
	calls  int
	tool   string
	park   int
	onPark func(calls int)
}

func (m *crashModel) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	n := m.calls
	m.calls++
	m.mu.Unlock()
	if m.onPark != nil && n == m.park {
		m.onPark(n + 1)
	}
	if n == 0 {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call-1", Name: m.tool, Arguments: `{}`}}, FinishReason: "tool_calls"}, nil
	}
	return &llm.Response{Content: "finished", FinishReason: "stop"}, nil
}

func (m *crashModel) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	resp, err := m.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &admissionStream{chunk: &llm.Chunk{Content: resp.Content, FinishReason: resp.FinishReason, ToolCalls: resp.ToolCalls}}, nil
}

func (m *crashModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// crashTool is the tool each boundary runs at step 0: "lookup" is
// retry-safe, "write" is not.
func crashTool(boundary string) string {
	if boundary == "effect_tool" {
		return "write"
	}
	return "lookup"
}

// TestRunReclaimCrashWorker runs only in the test-owned child. Its database
// lives under the parent's TMPDIR, so SIGKILL cannot orphan it. It runs one
// leased run and parks at its boundary after writing the manifest.
func TestRunReclaimCrashWorker(t *testing.T) {
	boundary := os.Getenv(runReclaimBoundaryEnv)
	if boundary == "" {
		return
	}
	require.Contains(t, []string{"model_call", "retry_safe_tool", "effect_tool"}, boundary)
	output := os.Getenv(runReclaimManifestEnv)
	require.NotEmpty(t, output)
	database := filepath.Join(os.TempDir(), "reclaim.db")
	st := openSQLiteStoreForTest(t, database+"?_pragma=busy_timeout(10000)")
	runID := id.NewAgentRunID()
	effectLog := output + ".effect"
	model := &crashModel{tool: crashTool(boundary), park: -1}
	park := func(calls int) {
		data, err := json.Marshal(runReclaimManifest{Boundary: boundary, Database: database, RunID: runID.String(), ProviderCalls: calls, EffectLog: effectLog})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(output+".tmp", data, 0o600))
		require.NoError(t, os.Rename(output+".tmp", output))
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		<-timer.C
		t.Fatal("parent did not kill the crash worker")
	}
	if boundary == "model_call" {
		// Parked inside Complete of step 1.
		model.park, model.onPark = 1, park
	}
	lookup := func(context.Context, cortex.Invocation) (string, error) {
		if boundary == "retry_safe_tool" {
			park(model.count())
		}
		return "looked up", nil
	}
	write := func(context.Context, cortex.Invocation) (string, error) {
		f, err := os.OpenFile(effectLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = f.WriteString("written\n")
		require.NoError(t, err)
		require.NoError(t, f.Sync())
		require.NoError(t, f.Close())
		park(model.count())
		return "written", nil
	}
	eng, err := engine.New(engine.WithStore(st), engine.WithLLM(model), engine.WithRunLease(time.Second), engine.WithWorkerID("crash-child"),
		engine.WithTool(llm.Tool{Name: "lookup"}, lookup), engine.WithToolRetrySafe("lookup"),
		engine.WithTool(llm.Tool{Name: "write"}, write))
	require.NoError(t, err)
	require.NoError(t, eng.CreateAgent(leaseScope(), &agent.Config{ID: id.NewAgentID(), Name: "assistant", MaxSteps: 5}))
	_, err = eng.RunAgent(leaseScope(), "assistant", "work", &engine.RunOverrides{RunID: runID})
	t.Fatalf("the run finished (%v) instead of parking at %s", err, boundary)
}

func TestRunReclaimAfterWorkerKilled(t *testing.T) {
	for _, boundary := range []string{"model_call", "retry_safe_tool", "effect_tool"} {
		t.Run(boundary, func(t *testing.T) {
			m := killCrashWorkerAtBoundary(t, boundary, crashWorkerConfig{Test: "TestRunReclaimCrashWorker", BoundaryEnv: runReclaimBoundaryEnv, ManifestEnv: runReclaimManifestEnv}, func(m runReclaimManifest) {
				require.Equal(t, boundary, m.Boundary)
				require.NotEmpty(t, m.RunID)
			})
			runID, err := id.ParseAgentRunID(m.RunID)
			require.NoError(t, err)
			// A fresh connection owned by the parent, never the killed
			// worker's memory.
			st := openSQLiteStoreForTest(t, m.Database+"?_pragma=busy_timeout(10000)")
			model := &crashModel{tool: crashTool(boundary), park: -1}
			// The parent's model continues the run: its first call is the
			// run's next step, never step 0 again.
			model.calls = 1
			var lookups, writes atomic.Int32
			hooks := newReclaimHooks()
			eng, err := engine.New(engine.WithStore(st), engine.WithLLM(model), engine.WithRunLease(time.Second), engine.WithWorkerID("crash-parent"),
				engine.WithSuspensionSweepInterval(100*time.Millisecond), engine.WithExtension(hooks),
				engine.WithTool(llm.Tool{Name: "lookup"}, func(context.Context, cortex.Invocation) (string, error) {
					lookups.Add(1)
					return "looked up", nil
				}), engine.WithToolRetrySafe("lookup"),
				engine.WithTool(llm.Tool{Name: "write"}, func(context.Context, cortex.Invocation) (string, error) {
					// The same external effect the child's handler makes, so
					// the file says whether the call ran a second time.
					writes.Add(1)
					f, openErr := os.OpenFile(m.EffectLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
					if openErr != nil {
						return "", openErr
					}
					defer func() { _ = f.Close() }()
					if _, writeErr := f.WriteString("written\n"); writeErr != nil {
						return "", writeErr
					}
					return "written", nil
				}))
			require.NoError(t, err)
			require.NoError(t, eng.Start(context.Background()))
			t.Cleanup(func() { _ = eng.Stop(context.Background()) })

			select {
			case notice := <-hooks.reclaimedCh:
				require.Equal(t, "crash-child", notice.PreviousOwner)
				require.Equal(t, 1, notice.Reclaims)
			case <-time.After(10 * time.Second):
				t.Fatal("the sweeper never reclaimed the killed worker's run")
			}

			continued, err := eng.ContinueRun(leaseScope(), runID)
			switch boundary {
			case "model_call":
				require.NoError(t, err)
				require.Equal(t, run.StateCompleted, continued.State)
				require.Zero(t, lookups.Load(), "a call the killed worker finished ran again")
				parentCalls := model.count() - 1
				require.Equal(t, continued.StepCount+1, m.ProviderCalls+parentCalls, "the interrupted model call ran once more, and nothing else did")
			case "retry_safe_tool":
				require.NoError(t, err)
				require.Equal(t, run.StateCompleted, continued.State)
				require.Equal(t, int32(1), lookups.Load(), "the retry-safe tool did not run again")
			case "effect_tool":
				data, readErr := os.ReadFile(m.EffectLog)
				require.NoError(t, readErr)
				require.Equal(t, 1, strings.Count(string(data), "\n"), "the external effect happened more than once")
				require.ErrorIs(t, err, cortex.ErrEffectUncertain)
				require.Zero(t, writes.Load(), "a call that may have changed something ran again")
				stored, readErr := st.GetRun(leaseScope(), runID)
				require.NoError(t, readErr)
				require.Equal(t, run.StateIncomplete, stored.State)
				require.Equal(t, run.StopToolError, stored.StopReason)
				require.Equal(t, "write", stored.Metadata["uncertain_tool"])
			}
		})
	}
}
