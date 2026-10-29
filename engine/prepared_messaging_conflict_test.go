package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
	"github.com/xraph/cortex/suspension"
)

const deadAdmitterBoundaryEnv = "CORTEX_TEST_DEAD_ADMITTER_BOUNDARY"
const deadAdmitterManifestEnv = "CORTEX_TEST_DEAD_ADMITTER_MANIFEST"

type deadAdmitterManifest struct {
	Database string
	Scope    cortex.Scope
	Team     id.OrchestrationID
	Asker    id.AgentRunID
}

// parkingAdmitStore records the asker's reply admission and then parks
// before the start, so the parent can kill the worker there: an admitter
// that died between its admission and its start.
type parkingAdmitStore struct {
	*sqlite.Store
	park func(suspension.ResumeAdmission)
}

func (s *parkingAdmitStore) AcquireOrchestrationCoordinator(ctx context.Context, req orchestration.CoordinatorRequest) (orchestration.CoordinatorClaim, error) {
	req.Duration = 2 * time.Second
	return s.Store.AcquireOrchestrationCoordinator(ctx, req)
}

func (s *parkingAdmitStore) AdmitResumeLeased(ctx context.Context, req suspension.ResumeAdmissionRequest, lease run.Lease, first *suspension.Progress) (suspension.ResumeAdmission, error) {
	admitted, err := s.Store.AdmitResumeLeased(ctx, req, lease, first)
	if err != nil || s.park == nil {
		return admitted, err
	}
	s.park(admitted)
	return admitted, nil
}

// conflictOnceStore answers the first resume start with
// cortex.ErrRunConflict, the way a start loses to the sweep marking the
// same admitted run reclaimable, and lets every later start through.
type conflictOnceStore struct {
	*sqlite.Store
	mu        sync.Mutex
	conflicts int
	starts    int
}

func (s *conflictOnceStore) StartResumeLeased(ctx context.Context, runID id.AgentRunID, key id.SuspensionID, digest string, lease run.Lease, first *suspension.Progress) (suspension.ResumeStart, error) {
	s.mu.Lock()
	conflict := s.conflicts == 0
	if conflict {
		s.conflicts++
	} else {
		s.starts++
	}
	s.mu.Unlock()
	if conflict {
		return suspension.ResumeStart{}, cortex.ErrRunConflict
	}
	return s.Store.StartResumeLeased(ctx, runID, key, digest, lease, first)
}

func (s *conflictOnceStore) counts() (conflicts, starts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conflicts, s.starts
}

func deadAdmitterOptions() []engine.Option {
	return []engine.Option{engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(allowPreparedMessaging()), engine.WithExternalTool(llm.Tool{Name: "await"})}
}

// TestPreparedTeamDeadAdmitterWorker is entered only by the killed child.
// The asker asks b, b pauses on an external tool, the host answers it,
// and the team's recovery admits b's reply to the asker and parks before
// the start.
func TestPreparedTeamDeadAdmitterWorker(t *testing.T) {
	if os.Getenv(deadAdmitterBoundaryEnv) == "" {
		return
	}
	output := os.Getenv(deadAdmitterManifestEnv)
	require.NotEmpty(t, output)
	database := filepath.Join(filepath.Dir(output), "admitter-team.db")
	f := newPreparedTeamFixtureAt(t, orchestration.StrategySequential, database)
	wrapper := &parkingAdmitStore{Store: f.s}
	refreshPreparedTeam(t, &f, append([]engine.Option{engine.WithStore(wrapper)}, deadAdmitterOptions()...)...)
	aCalls, bCalls := 0, 0
	f.client.complete = func(_ context.Context, req *llm.Request, _ int) (*llm.Response, error) {
		if req.System == "frozen a" {
			aCalls++
			if aCalls == 1 {
				return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "ask-b", Name: "agent_ask", Arguments: `{"to":"b","content":"check"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
		} else {
			bCalls++
			if bCalls == 1 {
				return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "external", Name: "await", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
		}
		return &llm.Response{Content: "retained answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	parent, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	var recipient id.AgentRunID
	for _, key := range parent.AgentRunIDs {
		pause, readErr := f.s.GetSuspension(f.ctx, key)
		require.NoError(t, readErr)
		if pause.Pending[0].Name == "await" {
			recipient = key
		}
	}
	require.False(t, recipient.IsNil())
	wrapper.park = func(admitted suspension.ResumeAdmission) {
		data, marshalErr := json.Marshal(deadAdmitterManifest{Database: database, Scope: f.policy.Scope, Team: parent.ID, Asker: admitted.Request.Version.RunID})
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(output+".tmp", data, 0600))
		require.NoError(t, os.Rename(output+".tmp", output))
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		<-timer.C
		t.Fatal("parent did not kill the admitter")
	}
	source, err := coordination.NewScopedOrchestrationSource(f.s, []cortex.Scope{f.policy.Scope}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	recovery := engine.WithOrchestrationRecovery(engine.OrchestrationRecoveryConfig{Service: coordination.OrchestrationRecoveryConfig{Discovery: source, Interval: 10 * time.Millisecond}, Authorize: func(context.Context, id.OrchestrationID) error { return nil }})
	admitter, err := engine.New(append([]engine.Option{engine.WithStore(wrapper), engine.WithLLM(f.client), recovery}, deadAdmitterOptions()...)...)
	require.NoError(t, err)
	_, err = admitter.Resume(f.ctx, recipient, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "external", Content: "done"}}})
	require.NoError(t, err)
	require.NoError(t, admitter.Start(f.ctx))
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	<-timer.C
	t.Fatal("the team's recovery never admitted the asker's reply")
}

// TestPreparedTeamRecoverySkipsAnAskerThatLostTheSweepRace: the admitter
// died between its admission and its start, so the team's recovery scan
// is the one to start the asker, and that start loses to the sweep once.
// The scan skips the asker like one already started, the team is not
// failed for it, and the next pass starts the asker and finishes the team.
func TestPreparedTeamRecoverySkipsAnAskerThatLostTheSweepRace(t *testing.T) {
	manifest := killCrashWorkerAtBoundary(t, "after_admission", crashWorkerConfig{Test: "TestPreparedTeamDeadAdmitterWorker", BoundaryEnv: deadAdmitterBoundaryEnv, ManifestEnv: deadAdmitterManifestEnv}, func(m deadAdmitterManifest) {
		require.False(t, m.Team.IsNil())
		require.False(t, m.Asker.IsNil())
	})
	storage := openSQLiteStoreForTest(t, manifest.Database+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	ctx := cortex.WithScope(t.Context(), manifest.Scope)
	asker, err := storage.GetRun(ctx, manifest.Asker)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, asker.State, "the admission claims the asker")

	wrapped := &conflictOnceStore{Store: storage}
	client := &preparedTeamClient{}
	client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{Content: "retained answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	var mu sync.Mutex
	var reported []error
	source, err := coordination.NewScopedOrchestrationSource(storage, []cortex.Scope{manifest.Scope}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	recovery := engine.WithOrchestrationRecovery(engine.OrchestrationRecoveryConfig{
		Service: coordination.OrchestrationRecoveryConfig{Discovery: source, Interval: 20 * time.Millisecond, ReportError: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, err)
		}},
		Authorize: func(context.Context, id.OrchestrationID) error { return nil },
	})
	logger := newCapturingLogger()
	fresh, err := engine.New(append([]engine.Option{engine.WithStore(wrapped), engine.WithLLM(client), engine.WithLogger(logger), recovery}, deadAdmitterOptions()...)...)
	require.NoError(t, err)
	require.NoError(t, fresh.Start(ctx))
	t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
	settled := func() bool {
		parent, readErr := storage.GetOrchestrationRun(ctx, manifest.Team)
		return readErr == nil && parent.Status != orchestration.StatusWaiting && parent.Status != orchestration.StatusRunning
	}
	if !assert.Eventually(t, settled, 30*time.Second, 20*time.Millisecond) {
		parent, _ := storage.GetOrchestrationRun(ctx, manifest.Team)
		current, _ := storage.GetRun(ctx, manifest.Asker)
		conflicts, starts := wrapped.counts()
		mu.Lock()
		t.Fatalf("team never settled: status %s, asker %s, conflicts %d, starts %d, reported %v", parent.Status, current.State, conflicts, starts, reported)
	}
	require.NoError(t, fresh.Stop(context.Background()))

	parent, err := storage.GetOrchestrationRun(ctx, manifest.Team)
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	for _, err := range reported {
		require.False(t, errors.Is(err, cortex.ErrRunConflict), "a lost race with the sweep failed the recovery pass: %v", err)
	}
	require.Equal(t, orchestration.StatusCompleted, parent.Status, parent.Error)
	conflicts, starts := wrapped.counts()
	require.Equal(t, 1, conflicts, "the asker's recovery start was never refused")
	require.Equal(t, 1, starts, "the asker was started more than once after the refusal")

	// The skip is logged with the run id only: no content, no scope.
	skipped := 0
	for _, warn := range logger.Warns() {
		if !strings.Contains(warn.msg, "lost to the sweep") {
			continue
		}
		skipped++
		require.Equal(t, manifest.Asker.String(), warn.fields["run_id"])
		for key := range warn.fields {
			require.Contains(t, []string{"run_id", "error_type", "suppressed"}, key, "the skip logged %q", key)
		}
	}
	require.Equal(t, 1, skipped, "the skipped asker was not logged")
}
