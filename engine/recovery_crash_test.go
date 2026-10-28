package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

const crashBoundaryEnv = "CORTEX_TEST_TASK_CRASH_BOUNDARY"
const crashManifestEnv = "CORTEX_TEST_TASK_CRASH_MANIFEST"

type taskCrashManifest struct {
	Boundary      string
	Database      string
	Request       coordination.ReadyTaskClaimRequest
	Dispatch      coordination.TaskDispatchRequest
	Claim         coordination.Claim
	ProviderCalls int
	EffectLog     string
}

type crashRecoveryClock struct{ at time.Time }

func (c crashRecoveryClock) Now() time.Time { return c.at }

// TestClaimRecoveryCrashWorker runs only in the test-owned child. Its fixtures
// and database live under the parent's TMPDIR, so SIGKILL cannot orphan them.
func TestClaimRecoveryCrashWorker(t *testing.T) {
	boundary := os.Getenv(crashBoundaryEnv)
	if boundary == "" {
		return
	}
	require.Contains(t, []string{"held", "dispatch", "provider_pending", "completed", "effect_pending"}, boundary)
	output := os.Getenv(crashManifestEnv)
	require.NotEmpty(t, output)
	var handler engine.ToolHandler
	var configure []func(*engine.Engine, *engine.RunOverrides)
	if boundary == "effect_pending" {
		configure = append(configure, func(e *engine.Engine, o *engine.RunOverrides) {
			o.Tools = []string{"probe"}
			require.NoError(t, engine.WithLLM(&requiredToolClient{})(e))
			require.NoError(t, engine.WithToolRevision("probe", "1")(e))
			require.NoError(t, engine.WithToolAuthorizer(&requiredToolAuthority{})(e))
			require.NoError(t, engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]())(e))
			require.NoError(t, engine.WithTool(llm.Tool{Name: "probe"}, func(ctx context.Context, inv cortex.Invocation) (string, error) { return handler(ctx, inv) })(e))
		})
	}
	f := preparedTaskClaimFixture(t, configure...)
	park := func() {
		claim, err := f.f.storage.GetTaskClaim(f.f.ctx, f.request.Acquisition.Claim.PlanID, f.request.Acquisition.Claim.TaskID)
		require.NoError(t, err)
		data, err := json.Marshal(taskCrashManifest{Boundary: boundary, Database: f.f.path, Request: f.request, Dispatch: f.dispatch, Claim: claim, ProviderCalls: f.client.calls, EffectLog: output + ".effect"})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(output+".tmp", data, 0600))
		require.NoError(t, os.Rename(output+".tmp", output))
		timer := time.NewTimer(5 * time.Minute)
		defer timer.Stop()
		<-timer.C
		t.Fatal("parent did not kill the crash worker")
	}
	if boundary == "effect_pending" {
		requirement := engineEffectRequirement()
		requirement.Tool = "probe"
		executor, makeErr := execution.NewEffectExecutor(execution.EffectExecutorConfig{Store: f.f.storage, Adapters: []execution.EffectRegistration{{Requirement: requirement, Adapter: &crashEffectAdapter{path: output + ".effect", park: park}}}, Authorize: func(context.Context, execution.EffectRequest, execution.EffectAction) error { return nil }, MaxConcurrent: 1})
		require.NoError(t, makeErr)
		handler, makeErr = engine.NewEffectToolHandler(executor, func(ctx context.Context, inv cortex.Invocation) (execution.EffectRequest, error) {
			call, ok := execution.RunCallFromContext(ctx)
			require.True(t, ok)
			return execution.EffectRequest{SchemaVersion: 1, Scope: inv.Scope, RunID: inv.RunID, OperationKey: inv.RunID.String() + "/probe", AttemptID: id.New(execution.PrefixEffectAttempt), ArgsDigest: evidence.Digest([]byte(inv.Call.Arguments)), PrincipalRef: "worker", AuthorityRef: "fixture-grant", Deadline: call.Deadline, Requirement: requirement}, nil
		})
		require.NoError(t, makeErr)
	}
	if boundary == "held" {
		park()
		return
	}
	cfg := taskWorkerConfig(t, f)
	if boundary == "dispatch" {
		base := cfg.Dispatch
		cfg.Dispatch = taskWorkerDispatchAdmitter(func(ctx context.Context, r coordination.TaskDispatchRequest) (coordination.TaskDispatch, error) {
			result, err := base.Admit(ctx, r)
			require.NoError(t, err)
			require.True(t, result.Fresh)
			park()
			return result, nil
		})
	}
	if boundary == "provider_pending" {
		f.client.before = park
	}
	worker, err := f.engine.NewTaskWorker(cfg)
	require.NoError(t, err)
	require.NoError(t, worker.RunClaimedTask(f.f.ctx, f.request, f.ready))
	require.Equal(t, "completed", boundary)
	park()
}

func killTaskWorkerAtBoundary(t *testing.T, boundary string) taskCrashManifest {
	return killCrashWorkerAtBoundary(t, boundary, crashWorkerConfig{Test: "TestClaimRecoveryCrashWorker", BoundaryEnv: crashBoundaryEnv, ManifestEnv: crashManifestEnv}, func(manifest taskCrashManifest) {
		require.Equal(t, boundary, manifest.Boundary)
		require.NoError(t, manifest.Claim.Validate())
	})
}

type crashWorkerConfig struct{ Test, BoundaryEnv, ManifestEnv string }

func killCrashWorkerAtBoundary[T any](t *testing.T, boundary string, config crashWorkerConfig, validate func(T)) T {
	t.Helper()
	root := t.TempDir()
	manifestPath := filepath.Join(root, "boundary.json")
	logPath := filepath.Join(root, "child.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, logFile.Close()) })
	binary, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), binary, "-test.run=^"+config.Test+"$", "-test.count=1", "-test.timeout=2m")
	cmd.Env = append(os.Environ(), config.BoundaryEnv+"="+boundary, config.ManifestEnv+"="+manifestPath, "TMPDIR="+root, "TMP="+root, "TEMP="+root)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		killErr := cmd.Process.Kill()
		if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			t.Errorf("kill test child: %v", killErr)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("test child did not exit after kill")
		}
	})
	deadline := time.NewTimer(time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var manifest T
	for {
		select {
		case childErr := <-done:
			waited = true
			output, readErr := os.ReadFile(logPath)
			require.NoError(t, readErr)
			t.Fatalf("worker exited before %s: %v\n%s", boundary, childErr, output)
		case <-deadline.C:
			output, readErr := os.ReadFile(logPath)
			require.NoError(t, readErr)
			t.Fatalf("worker did not reach %s\n%s", boundary, output)
		case <-ticker.C:
			data, readErr := os.ReadFile(manifestPath)
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			require.NoError(t, readErr)
			require.NoError(t, json.Unmarshal(data, &manifest))
			validate(manifest)
			// Kill only this test's process, after it confirms the durable boundary.
			require.NoError(t, cmd.Process.Kill())
			select {
			case childErr := <-done:
				waited = true
				var killed *exec.ExitError
				require.ErrorAs(t, childErr, &killed)
				require.False(t, cmd.ProcessState.Success())
			case <-time.After(10 * time.Second):
				t.Fatal("killed worker did not exit")
			}
			return manifest
		}
	}
}

func TestClaimRecoveryAfterWorkerProcessCrash(t *testing.T) {
	for _, boundary := range []string{"held", "dispatch", "provider_pending", "completed"} {
		t.Run(boundary, func(t *testing.T) {
			m := killTaskWorkerAtBoundary(t, boundary)
			// This is a fresh connection owned by the parent, never the killed worker's
			// in-memory state. Advance the injected lease clock, not the system clock.
			storage := openSQLiteStoreForTest(t, m.Database)
			ctx := cortex.WithScope(context.Background(), m.Claim.Scope)
			current, err := storage.GetTaskClaim(ctx, m.Claim.PlanID, m.Claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, m.Claim, current)
			clock := crashRecoveryClock{at: m.Claim.ExpiresAt}
			repo := storage.WithClock(clock)
			cfg := coordination.ClaimRecoveryConfig{Store: repo, Clock: clock, Interval: 10 * time.Millisecond, PageSize: 1, Discovery: lifecyclePlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
				return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: m.Claim.Scope, PlanID: m.Claim.PlanID}}}, nil
			})}
			recovered, err := engine.New(engine.WithStore(storage), engine.WithClaimRecovery(cfg), engine.WithSuspensionTTL(0))
			require.NoError(t, err)
			require.NoError(t, recovered.Start(ctx))
			t.Cleanup(func() { require.NoError(t, recovered.Stop(context.Background())) })
			expected := coordination.ClaimRecovering
			if boundary == "held" {
				expected = coordination.ClaimReleased
			}
			require.Eventually(t, func() bool {
				observed, readErr := repo.GetTaskClaim(ctx, m.Claim.PlanID, m.Claim.TaskID)
				return readErr == nil && observed.State == expected
			}, 10*time.Second, 10*time.Millisecond)
			require.NoError(t, recovered.Stop(context.Background()))
			after, err := repo.GetTaskClaim(ctx, m.Claim.PlanID, m.Claim.TaskID)
			require.NoError(t, err)
			require.NoError(t, coordination.ValidateClaimSuccessor(m.Claim, after))
			require.Equal(t, m.Claim.RunID, after.RunID)
			require.Equal(t, m.Claim.Fence, after.Fence)
			require.Nil(t, after.AcceptanceID)
			budget, err := storage.GetBudgetSnapshot(ctx, m.Claim.PlanID)
			require.NoError(t, err)
			require.Equal(t, m.Request.Acquisition.Reservation.Allocation, budget.Charged)
			history, err := repo.TaskClaimHistory(ctx, coordination.ClaimHistoryRequest{PlanID: m.Claim.PlanID, TaskID: m.Claim.TaskID, Limit: 10})
			require.NoError(t, err)
			require.Equal(t, int(after.Revision), len(history))
			_, err = repo.RenewTaskClaim(ctx, m.Claim.Token(), time.Minute)
			require.ErrorIs(t, err, coordination.ErrClaimConflict)
			replay, err := repo.ClaimReadyTask(ctx, m.Request)
			require.NoError(t, err)
			require.False(t, replay.Fresh)
			// Historical admission cannot reach even the work resolver after restart.
			require.NoError(t, engine.WithTaskRunAdmitter(struct{ coordination.TaskRunAdmitter }{})(recovered))
			require.NoError(t, engine.WithTaskCallGuard(struct{ engine.TaskCallGuard }{})(recovered))
			worker, err := recovered.NewTaskWorker(engine.TaskWorkerConfig{Resolver: taskWorkerResolver(func(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) (engine.TaskWork, error) {
				t.Fatal("historical admission reached work resolution")
				return engine.TaskWork{}, nil
			}), Dispatch: struct {
				coordination.TaskDispatchAdmitter
			}{}})
			require.NoError(t, err)
			require.ErrorIs(t, worker.RunClaimedTask(ctx, m.Request, replay), cortex.ErrTaskRunReconciliationRequired)
			calls, err := storage.ListRunCalls(ctx, execution.RunCallListRequest{RunID: m.Dispatch.RunID, Limit: 10})
			require.NoError(t, err)
			child, childErr := storage.GetRun(ctx, m.Dispatch.RunID)
			switch boundary {
			case "held", "dispatch":
				require.Zero(t, m.ProviderCalls)
				require.Empty(t, calls)
				require.ErrorIs(t, childErr, cortex.ErrRunNotFound)
			case "provider_pending":
				require.Equal(t, 1, m.ProviderCalls)
				require.NoError(t, childErr)
				require.Equal(t, run.StateRunning, child.State)
				require.Len(t, calls, 1)
				require.Nil(t, calls[0].Completion)
			case "completed":
				require.Equal(t, 1, m.ProviderCalls)
				require.NoError(t, childErr)
				require.Equal(t, run.StateCompleted, child.State)
				require.Equal(t, verification.StatusPassed, child.VerificationStatus)
				require.Len(t, calls, 1)
				require.NotNil(t, calls[0].Completion)
			}
			for _, call := range calls {
				fresh, beginErr := repo.BeginRunCall(ctx, call.Request)
				require.NoError(t, beginErr)
				require.False(t, fresh)
				changed := call.Request
				changed.ID = id.New(execution.PrefixRunCall)
				_, beginErr = repo.BeginRunCall(ctx, changed)
				require.ErrorIs(t, beginErr, execution.ErrRunCallConflict)
			}
			if boundary != "held" {
				dispatch, dispatchErr := repo.DispatchReadyTask(ctx, m.Dispatch)
				require.NoError(t, dispatchErr)
				require.False(t, dispatch.Fresh)
				require.Equal(t, m.Dispatch.RunID, dispatch.Claim.RunID)
			}
			require.NoError(t, recovered.Start(ctx))
			require.NoError(t, recovered.Stop(context.Background()))
			retained, err := repo.GetTaskClaim(ctx, m.Claim.PlanID, m.Claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, after, retained)
		})
	}
}
