package storetest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/orchestration"
	rootsqlite "github.com/xraph/cortex/store/sqlite"
)

// OrchestrationFixture prepares a real parallel team in a disposable runtime store.
// It is shared test infrastructure, not a qualified isolation provider.
func OrchestrationFixture(t *testing.T, model llm.Client) (optimization.ExperimentBundle, *engine.Engine, *rootsqlite.Store, func() *rootsqlite.Store) {
	t.Helper()
	bundle := Fixture(t, "team")
	ctx := scoped(bundle.Experiment.Spec.Scope)
	path := filepath.Join(t.TempDir(), "runtime.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"
	reopen := func() *rootsqlite.Store {
		driver := sqlitedriver.New()
		if openErr := driver.Open(context.Background(), path); openErr != nil {
			t.Fatal(openErr)
		}
		db, openErr := grove.Open(driver)
		if openErr != nil {
			t.Fatal(openErr)
		}
		source := rootsqlite.New(db)
		t.Cleanup(func() {
			if closeErr := source.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
		return source
	}
	sources := reopen()
	if migrateErr := sources.Migrate(ctx); migrateErr != nil {
		t.Fatal(migrateErr)
	}
	var err error
	for _, name := range []string{"worker", "reviewer"} {
		key := id.NewAgentID()
		if name == "worker" {
			key = bundle.Experiment.Spec.AgentID
		}
		if err = sources.Create(ctx, &agent.Config{ID: key, Name: name, Model: "fixture", SystemPrompt: "Answer the task."}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "parallel", Strategy: orchestration.StrategyParallel, Participants: []orchestration.Participant{{AgentName: "worker"}, {AgentName: "reviewer"}}}
	if err = sources.CreateOrchestration(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	runtime, err := engine.New(engine.WithStore(sources), engine.WithLLM(model), engine.WithModelRevision("fixture", "1"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour)
	budgets := map[string]execution.Budget{"worker": {MaxSteps: 2, MaxTokens: 100, Deadline: deadline}, "reviewer": {MaxSteps: 2, MaxTokens: 100, Deadline: deadline}}
	prepared, err := runtime.PrepareOrchestrationWithBudgets(ctx, cfg.ID, budgets)
	if err != nil {
		t.Fatal(err)
	}
	preset, err := optimization.SealOrchestrationPreset(optimization.OrchestrationPreset{Name: "parallel", Prepared: prepared, Limits: execution.Resources{Steps: 4, Tokens: 200, FanOut: 2}, Guidance: map[string][]string{"worker": {"role"}}})
	if err != nil {
		t.Fatal(err)
	}
	base := bundle.Versions[0]
	base.Prepared = nil
	base.Orchestration = &optimization.OrchestrationSelection{Preset: preset.Name, Prepared: prepared}
	base.Policy = optimization.EditPolicy{OrchestrationPresets: []optimization.OrchestrationPreset{preset}, MaxBodyBytes: 128, MaxPatchBytes: 256, MaxEdits: 2}
	base, err = optimization.SealVersion(base)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Versions[0] = base
	bundle.Experiment.Spec.Baseline = optimization.VersionRef{ID: base.ID, Digest: base.Digest}
	return bundle, runtime, sources, reopen
}
