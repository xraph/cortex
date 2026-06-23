package orchestration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// --- fakes ---

type fakeConfigStore struct{ byName map[string]*Config }

func (f *fakeConfigStore) CreateOrchestration(context.Context, *Config) error { return nil }
func (f *fakeConfigStore) GetOrchestration(context.Context, id.OrchestrationConfigID) (*Config, error) {
	return nil, cortex.ErrOrchestrationNotFound
}
func (f *fakeConfigStore) GetOrchestrationByName(_ context.Context, _, name string) (*Config, error) {
	if c, ok := f.byName[name]; ok {
		return c, nil
	}
	return nil, cortex.ErrOrchestrationNotFound
}
func (f *fakeConfigStore) UpdateOrchestration(context.Context, *Config) error { return nil }
func (f *fakeConfigStore) DeleteOrchestration(context.Context, id.OrchestrationConfigID) error {
	return nil
}
func (f *fakeConfigStore) ListOrchestrations(context.Context, *ConfigListFilter) ([]*Config, error) {
	return nil, nil
}
func (f *fakeConfigStore) CountOrchestrations(context.Context, *ConfigListFilter) (int64, error) {
	return 0, nil
}

type fakeRunStore struct {
	created *Run
	updated *Run
}

func (f *fakeRunStore) CreateOrchestrationRun(_ context.Context, r *Run) error {
	f.created = r
	return nil
}
func (f *fakeRunStore) GetOrchestrationRun(context.Context, id.OrchestrationID) (*Run, error) {
	return f.created, nil
}
func (f *fakeRunStore) UpdateOrchestrationRun(_ context.Context, r *Run) error {
	f.updated = r
	return nil
}
func (f *fakeRunStore) ListOrchestrationRuns(context.Context, *RunListFilter) ([]*Run, error) {
	return nil, nil
}
func (f *fakeRunStore) CountOrchestrationRuns(context.Context, *RunListFilter) (int64, error) {
	return 0, nil
}

type recordingHooks struct {
	started, completed int
	handoffs           int
}

func (h *recordingHooks) OrchestrationStarted(context.Context, id.OrchestrationID, string) {
	h.started++
}
func (h *recordingHooks) OrchestrationCompleted(context.Context, id.OrchestrationID, time.Duration) {
	h.completed++
}
func (h *recordingHooks) AgentHandoff(context.Context, id.OrchestrationID, string, string, string) {
	h.handoffs++
}

type errRunner struct{}

func (errRunner) RunAgent(_ context.Context, _, _, _ string, _ *RunOpts) (*AgentResult, error) {
	return nil, errors.New("boom")
}

// --- test ---

func TestServiceRunSequentialPersistsAndEmits(t *testing.T) {
	runner := newFakeRunner()
	runner.outputs = map[string]string{"a": "AA", "b": "BB"}
	cfg := &Config{
		ID:           id.NewOrchestrationConfigID(),
		Name:         "team",
		AppID:        "app1",
		Strategy:     StrategySequential,
		Participants: []Participant{{AgentName: "a"}, {AgentName: "b"}},
	}
	configs := &fakeConfigStore{byName: map[string]*Config{"team": cfg}}
	runs := &fakeRunStore{}
	hooks := &recordingHooks{}
	svc := NewService(runner, configs, runs, hooks)

	out, err := svc.Run(context.Background(), "app1", "team", "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", out.Status)
	}
	if out.Output != "BB" {
		t.Fatalf("output = %q, want BB", out.Output)
	}
	if out.ConfigID.String() != cfg.ID.String() {
		t.Fatalf("config id not linked")
	}
	if hooks.started != 1 || hooks.completed != 1 {
		t.Fatalf("hooks started=%d completed=%d, want 1/1", hooks.started, hooks.completed)
	}
	if hooks.handoffs != 1 {
		t.Fatalf("handoffs = %d, want 1", hooks.handoffs)
	}
	if runs.created == nil || runs.updated == nil {
		t.Fatalf("run not persisted (create=%v update=%v)", runs.created, runs.updated)
	}
	if runs.updated.Status != StatusCompleted {
		t.Fatalf("persisted status = %q, want completed", runs.updated.Status)
	}
}

func TestServiceRunUnknownConfig(t *testing.T) {
	svc := NewService(newFakeRunner(), &fakeConfigStore{byName: map[string]*Config{}}, &fakeRunStore{}, &recordingHooks{})
	_, err := svc.Run(context.Background(), "app1", "missing", "go")
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}

func TestServiceRunFailed(t *testing.T) {
	runner := &errRunner{}
	cfg := &Config{
		ID:           id.NewOrchestrationConfigID(),
		Name:         "team",
		AppID:        "app1",
		Strategy:     StrategySequential,
		Participants: []Participant{{AgentName: "a"}},
	}
	configs := &fakeConfigStore{byName: map[string]*Config{"team": cfg}}
	runs := &fakeRunStore{}
	hooks := &recordingHooks{}
	svc := NewService(runner, configs, runs, hooks)

	rec, err := svc.Run(context.Background(), "app1", "team", "go")
	if err == nil {
		t.Fatal("expected error from failing runner")
	}
	if rec == nil {
		t.Fatal("expected record to be returned on failure")
	}
	if rec.Status != StatusFailed {
		t.Fatalf("status = %q, want %q", rec.Status, StatusFailed)
	}
	if rec.Error == "" {
		t.Fatal("error not captured in record")
	}
	if runs.updated == nil {
		t.Fatal("run not persisted on failure")
	}
	if runs.updated.Status != StatusFailed {
		t.Fatalf("persisted status = %q, want %q", runs.updated.Status, StatusFailed)
	}
	if hooks.started != 1 || hooks.completed != 1 {
		t.Fatalf("hooks started=%d completed=%d, want 1/1", hooks.started, hooks.completed)
	}
}
