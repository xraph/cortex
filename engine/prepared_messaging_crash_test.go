package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	"github.com/xraph/cortex/store/sqlite"
)

const heldCrashBoundaryEnv = "CORTEX_TEST_HELD_CRASH_BOUNDARY"
const heldCrashManifestEnv = "CORTEX_TEST_HELD_CRASH_MANIFEST"

type heldCrashManifest struct {
	Database      string
	Scope         cortex.Scope
	Team          id.OrchestrationID
	Request       a2a.HeldAskRequest
	Receipt       a2a.HeldAskReceipt
	ProviderCalls int
}

type heldCrashStore struct {
	*sqlite.Store
	afterCommit bool
	park        func(a2a.HeldAskRequest, a2a.HeldAskReceipt)
}

func (s *heldCrashStore) AcquireOrchestrationCoordinator(ctx context.Context, req orchestration.CoordinatorRequest) (orchestration.CoordinatorClaim, error) {
	req.Duration = 6 * time.Second
	return s.Store.AcquireOrchestrationCoordinator(ctx, req)
}
func (s *heldCrashStore) ReleaseHeldAsk(ctx context.Context, req a2a.HeldAskRequest) (a2a.HeldAskReceipt, error) {
	var receipt a2a.HeldAskReceipt
	if s.afterCommit {
		result, err := s.Store.ReleaseHeldAsk(ctx, req)
		if err != nil {
			return result, err
		}
		receipt = result
	}
	s.park(req, receipt)
	return receipt, nil
}

// TestPreparedTeamHeldQuestionCrashWorker is entered only by the owned child.
func TestPreparedTeamHeldQuestionCrashWorker(t *testing.T) {
	boundary := os.Getenv(heldCrashBoundaryEnv)
	if boundary == "" {
		return
	}
	require.Contains(t, []string{"before_release", "after_commit"}, boundary)
	output := os.Getenv(heldCrashManifestEnv)
	require.NotEmpty(t, output)
	database := filepath.Join(filepath.Dir(output), "held-team.db")
	f := newPreparedTeamFixtureAt(t, orchestration.StrategySequential, database)
	wrapper := &heldCrashStore{Store: f.s, afterCommit: boundary == "after_commit"}
	wrapper.park = func(req a2a.HeldAskRequest, receipt a2a.HeldAskReceipt) {
		data, err := json.Marshal(heldCrashManifest{Database: database, Scope: f.policy.Scope, Team: f.policy.ExecutionID, Request: req, Receipt: receipt, ProviderCalls: f.client.calls})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(output+".tmp", data, 0600))
		require.NoError(t, os.Rename(output+".tmp", output))
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		<-timer.C
		t.Fatal("parent did not kill held question worker")
	}
	refreshPreparedTeam(t, &f, engine.WithStore(wrapper), engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(allowPreparedMessaging()))
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "ask-b", Name: "agent_ask", Arguments: `{"to":"b","content":"check the evidence"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	_, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.NoError(t, err)
	t.Fatal("worker did not stop at release boundary")
}

func TestPreparedTeamHeldQuestionCrashRecovery(t *testing.T) {
	for _, boundary := range []string{"before_release", "after_commit"} {
		t.Run(boundary, func(t *testing.T) {
			manifest := killCrashWorkerAtBoundary(t, boundary, crashWorkerConfig{Test: "TestPreparedTeamHeldQuestionCrashWorker", BoundaryEnv: heldCrashBoundaryEnv, ManifestEnv: heldCrashManifestEnv}, func(m heldCrashManifest) {
				require.Equal(t, 1, m.ProviderCalls)
				require.False(t, m.Team.IsNil())
				require.False(t, m.Request.SuspensionID.IsNil())
			})
			storage := openSQLiteStoreForTest(t, manifest.Database+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
			ctx := cortex.WithScope(t.Context(), manifest.Scope)
			current, err := storage.GetRun(ctx, manifest.Request.RunID)
			require.NoError(t, err)
			require.Equal(t, run.StatePaused, current.State)
			private, err := a2a.NewRoutingStore(storage, manifest.Team)
			require.NoError(t, err)
			queued, err := private.ListQueuedDeliveries(ctx, 10)
			require.NoError(t, err)
			if boundary == "before_release" {
				require.Empty(t, queued)
			} else {
				require.Len(t, queued, 1)
			}
			source, err := coordination.NewScopedOrchestrationSource(storage, []cortex.Scope{manifest.Scope}, func(context.Context, cortex.Scope) error { return nil })
			require.NoError(t, err)
			client := &preparedTeamClient{}
			client.complete = func(_ context.Context, req *llm.Request, _ int) (*llm.Response, error) {
				if req.System == "frozen a" {
					answered := false
					for _, message := range req.Messages {
						if message.Role == "tool" && strings.Contains(message.Content, "recovered answer") {
							answered = true
						}
					}
					require.True(t, answered, "the original paused asker must receive its saved question's answer")
				}
				return &llm.Response{Content: "recovered answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
			fresh, err := engine.New(engine.WithStore(storage), engine.WithLLM(client), engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(allowPreparedMessaging()), engine.WithOrchestrationRecovery(engine.OrchestrationRecoveryConfig{Service: coordination.OrchestrationRecoveryConfig{Discovery: source, Interval: 20 * time.Millisecond}, Authorize: func(context.Context, id.OrchestrationID) error { return nil }}))
			require.NoError(t, err)
			require.NoError(t, fresh.Start(ctx))
			t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
			require.Eventually(t, func() bool {
				parent, readErr := storage.GetOrchestrationRun(ctx, manifest.Team)
				return readErr == nil && parent.Status == orchestration.StatusCompleted
			}, 20*time.Second, 20*time.Millisecond)
			require.NoError(t, fresh.Stop(context.Background()))
			require.Equal(t, 3, client.calls)
			parent, err := storage.GetOrchestrationRun(ctx, manifest.Team)
			require.NoError(t, err)
			require.Len(t, parent.AgentRunIDs, 3)
			require.Contains(t, parent.AgentRunIDs, manifest.Request.RunID)
			reader, ok := private.(a2a.HeldAskStore)
			require.True(t, ok)
			receipt, err := reader.GetHeldAskRelease(ctx, manifest.Request.ReplyWith)
			require.NoError(t, err)
			require.Len(t, receipt.Deliveries, 1)
			if boundary == "after_commit" {
				require.Equal(t, manifest.Receipt.Deliveries, receipt.Deliveries)
			}
			global, err := storage.ListMessages(ctx, nil)
			require.NoError(t, err)
			require.Empty(t, global)
			budget, err := storage.GetOrchestrationBudgetSnapshot(ctx, manifest.Team)
			require.NoError(t, err)
			require.EqualValues(t, 3, budget.Charged.FanOut)
		})
	}
}
