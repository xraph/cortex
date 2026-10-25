package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

const adoptionCrashBoundaryEnv = "CORTEX_TEST_ADOPTION_CRASH_BOUNDARY"
const adoptionCrashRootEnv = "CORTEX_TEST_ADOPTION_CRASH_ROOT"

type adoptionCrashManifest struct {
	Request coordination.TaskAcceptanceRequest
	Claim   coordination.Claim
	Model   execution.RunCallRequest
	Run     *run.Run
}

type adoptionCrashDriver struct {
	assurance.Driver
	plans    assurance.PlanDriver
	boundary string
	park     func()
}

func (d *adoptionCrashDriver) TransactPlan(ctx context.Context, plan id.ID, fn func(context.Context, assurance.Driver) error) error {
	err := d.plans.TransactPlan(ctx, plan, func(tc context.Context, tx assurance.Driver) error {
		if callErr := fn(tc, tx); callErr != nil {
			return callErr
		}
		if d.boundary == "before_commit" {
			d.park()
		}
		return nil
	})
	if err == nil && d.boundary == "after_commit" {
		d.park()
	}
	return err
}

type adoptionCrashStore struct {
	coordination.TaskAdoptionStore
	writer *assurance.Repository
}

func (s adoptionCrashStore) AdoptTaskResult(ctx context.Context, r coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
	return s.writer.AdoptTaskResult(ctx, r)
}

func startCrashAdoption(t *testing.T, backend store.Store, repo coordination.TaskAdoptionStore, m adoptionCrashManifest) *engine.Engine {
	t.Helper()
	cfg := coordination.TaskAdoptionServiceConfig{Store: repo, Interval: 10 * time.Millisecond,
		Discovery: adoptionPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: m.Claim.Scope, PlanID: m.Claim.PlanID}}}, nil
		}),
		AuthorizeScan: func(context.Context, coordination.Claim, execution.RunCallSeal) error { return nil },
		Fence:         adoptionFence{}, Authorizer: adoptionAuthorizer{},
		Resolver: adoptionResolver(func(context.Context, coordination.Claim, execution.RunRecoverySnapshot) (*coordination.TaskAcceptanceRequest, error) {
			request, _, err := m.Request.Freeze()
			return &request, err
		}),
	}
	e, err := engine.New(engine.WithStore(backend), engine.WithTaskAdoption(cfg), engine.WithSuspensionTTL(0))
	require.NoError(t, err)
	require.NoError(t, e.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, e.Stop(context.Background())) })
	return e
}

// TaskAdoptionProcessCrash kills a test-owned coordinator at the real backend's
// commit boundary, then reopens its database. The opener must use the supplied
// path so every child resource remains owned by the parent test directory.
// Fixture authority qualifies persistence and lifecycle, not remote quiescence.
func TaskAdoptionProcessCrash(t *testing.T, open func(*testing.T, string) (store.Store, assurance.Driver)) {
	t.Helper()
	if boundary := os.Getenv(adoptionCrashBoundaryEnv); boundary != "" {
		require.Contains(t, []string{"before_commit", "after_commit"}, boundary)
		root := os.Getenv(adoptionCrashRootEnv)
		require.NotEmpty(t, root)
		directory, err := os.OpenRoot(root)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, directory.Close()) })
		backend, driver := open(t, filepath.Join(root, "adoption.db"))
		f := newTaskRecoveryFixture(t, backend, driver, "")
		claim, err := f.repo.GetTaskClaim(f.ctx, f.request.Claim.PlanID, f.request.Claim.TaskID)
		require.NoError(t, err)
		original, err := backend.GetRun(f.ctx, claim.RunID)
		require.NoError(t, err)
		m := adoptionCrashManifest{Request: f.request, Claim: claim, Model: f.model, Run: original}
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		park := func() {
			data, marshalErr := json.Marshal(m)
			require.NoError(t, marshalErr)
			require.NoError(t, directory.WriteFile("boundary.tmp", data, 0o600))
			require.NoError(t, directory.Rename("boundary.tmp", "boundary.json"))
			<-t.Context().Done()
		}
		writer := assurance.New(&adoptionCrashDriver{Driver: driver, plans: plans, boundary: boundary, park: park}).WithClock(f.clock)
		startCrashAdoption(t, backend, adoptionCrashStore{TaskAdoptionStore: f.repo, writer: writer}, m)
		<-t.Context().Done()
		return
	}
	for _, boundary := range []string{"before_commit", "after_commit"} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			m := killAdoptionCoordinator(t, root, boundary)
			backend, driver := open(t, filepath.Join(root, "adoption.db"))
			ctx := cortex.WithScope(context.Background(), m.Claim.Scope)
			repo := assurance.New(driver).WithClock(&jobClock{at: m.Request.Recovery.ObservedAt})
			current, err := repo.GetTaskClaim(ctx, m.Claim.PlanID, m.Claim.TaskID)
			require.NoError(t, err)
			accepted, err := repo.GetTaskAcceptance(ctx, m.Request.ID)
			if boundary == "before_commit" {
				require.ErrorIs(t, err, coordination.ErrTaskAcceptanceNotFound)
				require.Equal(t, m.Claim, current)
				items, listErr := repo.ListTaskAcceptances(ctx, coordination.TaskAcceptanceListRequest{PlanID: m.Claim.PlanID, Limit: 2})
				require.NoError(t, listErr)
				require.Empty(t, items, "rolled-back acceptance must not enter outbox discovery")
			} else {
				require.NoError(t, err)
				require.Equal(t, coordination.ClaimCompleted, current.State)
				require.Equal(t, accepted.Claim, current)
			}
			observed := &adoptionObservedStore{TaskAdoptionStore: repo}
			service := startCrashAdoption(t, backend, observed, m)
			require.Eventually(t, func() bool {
				got, readErr := repo.GetTaskClaim(ctx, m.Claim.PlanID, m.Claim.TaskID)
				return readErr == nil && got.State == coordination.ClaimCompleted && observed.reads.Load() > 0
			}, 10*time.Second, 10*time.Millisecond)
			require.NoError(t, service.Stop(context.Background()))
			status, configured := service.TaskAdoptionStatus()
			require.True(t, configured)
			require.Zero(t, status.Failures)
			require.Zero(t, status.Active)
			persisted, err := repo.GetTaskAcceptance(ctx, m.Request.ID)
			require.NoError(t, err)
			require.NoError(t, persisted.Validate())
			if boundary == "after_commit" {
				require.Equal(t, accepted, persisted)
			}
			require.Equal(t, m.Claim.ExpiresAt, persisted.Claim.ExpiresAt)
			require.Equal(t, m.Claim.Fence, persisted.Claim.Fence)
			require.Equal(t, m.Claim.RunID, persisted.Claim.RunID)
			adopter, err := coordination.NewTaskRecoveryAdopter(repo, adoptionFence{}, adoptionAuthorizer{})
			require.NoError(t, err)
			replay, err := adopter.Adopt(ctx, m.Request)
			require.NoError(t, err)
			require.False(t, replay.Fresh)
			require.Equal(t, persisted, replay)
			items, err := repo.ListTaskAcceptances(ctx, coordination.TaskAcceptanceListRequest{PlanID: m.Claim.PlanID, Limit: 2})
			require.NoError(t, err)
			require.Equal(t, []coordination.TaskAcceptance{persisted}, items)
			original, err := backend.GetRun(ctx, m.Claim.RunID)
			require.NoError(t, err)
			require.Equal(t, m.Run, original)
			call, err := repo.GetRunCall(ctx, m.Model.RunID, m.Model.OperationDigest)
			require.NoError(t, err)
			require.Nil(t, call.Completion)
		})
	}
}

func killAdoptionCoordinator(t *testing.T, root, boundary string) adoptionCrashManifest {
	t.Helper()
	binary, err := os.Executable()
	require.NoError(t, err)
	directory, err := os.OpenRoot(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, directory.Close()) })
	logFile, err := directory.OpenFile("child.log", os.O_CREATE|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, logFile.Close()) })
	// Run the parent test entry point; the child environment selects worker mode.
	name := t.Name()[:len(t.Name())-len(boundary)-1]
	cmd := exec.CommandContext(t.Context(), binary, "-test.run=^"+regexp.QuoteMeta(name)+"$", "-test.count=1", "-test.timeout=1m") // #nosec G204 -- executes this test binary directly with an escaped test name; no shell or external command input.
	cmd.Env = append(os.Environ(), adoptionCrashBoundaryEnv+"="+boundary, adoptionCrashRootEnv+"="+root, "TMPDIR="+root, "TMP="+root, "TEMP="+root)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			killErr := cmd.Process.Kill()
			require.True(t, killErr == nil || errors.Is(killErr, os.ErrProcessDone))
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("adoption child did not exit after kill")
			}
		}
	})
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case childErr := <-done:
			waited = true
			output, readErr := directory.ReadFile("child.log")
			require.NoError(t, readErr)
			t.Fatalf("coordinator exited before %s: %v\n%s", boundary, childErr, output)
		case <-deadline.C:
			output, readErr := directory.ReadFile("child.log")
			require.NoError(t, readErr)
			t.Fatalf("coordinator did not reach %s\n%s", boundary, output)
		case <-ticker.C:
			data, readErr := directory.ReadFile("boundary.json")
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			require.NoError(t, readErr)
			var m adoptionCrashManifest
			require.NoError(t, json.Unmarshal(data, &m))
			require.NoError(t, m.Claim.Validate())
			require.NoError(t, cmd.Process.Kill())
			select {
			case childErr := <-done:
				waited = true
				var killed *exec.ExitError
				require.ErrorAs(t, childErr, &killed)
			case <-time.After(10 * time.Second):
				t.Fatal("killed adoption coordinator did not exit")
			}
			return m
		}
	}
}
