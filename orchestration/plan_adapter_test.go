package orchestration

import (
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

func adapterFixture(t *testing.T, strategy string, names ...string) (Config, planning.Plan) {
	t.Helper()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "plans"}}}
	cfg := Config{ID: id.NewOrchestrationConfigID(), Scope: scope, Strategy: strategy}
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "checked report", Criteria: []verification.Criterion{{ID: "check", Kind: verification.KindTest, Required: true, Verifier: "test", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	if err != nil {
		t.Fatal(err)
	}
	budget := execution.Budget{MaxSteps: 10, MaxTokens: 1000, MaxToolCalls: 5, Deadline: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	template := planning.Plan{SchemaVersion: 1, ID: id.New(planning.PrefixPlan), Scope: scope, Revision: 1, Objective: contract.Objective, Contract: contract, Budget: budget, Owners: names, Actor: "host", Reason: "request"}
	for _, name := range names {
		cfg.Participants = append(cfg.Participants, Participant{AgentName: name})
		template.Tasks = append(template.Tasks, planning.Task{ID: id.New(planning.PrefixTask), Owner: name, Instruction: "perform assigned work", Criteria: []string{"check"}, Budget: budget, Output: planning.ArtifactRequirement{Name: name, MediaType: "text/plain", MaxBytes: 4096}})
	}
	return cfg, template
}

func TestPlanAdapterPreservesSequentialAndParallelDependencies(t *testing.T) {
	for _, strategy := range []string{StrategySequential, StrategyParallel} {
		cfg, template := adapterFixture(t, strategy, "a", "b")
		graph, err := AdaptPlan(cfg, PlanRequest{Template: template})
		if err != nil {
			t.Fatal(err)
		}
		if err = planning.Validate(graph, planning.Limits{}); err != nil {
			t.Fatal(err)
		}
		if graph.Origin.ID != cfg.ID || !evidence.ValidDigest(graph.Origin.Digest) {
			t.Fatal("missing config provenance")
		}
		if len(template.Tasks[1].DependsOn) != 0 {
			t.Fatal("mutated host template")
		}
		if strategy == StrategySequential {
			if len(graph.Tasks[1].DependsOn) != 1 || graph.Tasks[1].DependsOn[0] != graph.Tasks[0].ID {
				t.Fatal("sequence lost")
			}
		} else if len(graph.Tasks[1].DependsOn) != 0 {
			t.Fatal("parallel worker serialized")
		}
		cfg.Settings.Model = "different"
		changed, err := AdaptPlan(cfg, PlanRequest{Template: template})
		if err != nil {
			t.Fatal(err)
		}
		if changed.Origin.Digest == graph.Origin.Digest || changed.Digest == graph.Digest {
			t.Fatal("model change not bound")
		}
	}
}

func TestPlanAdapterAggregatorRequiresEveryWorker(t *testing.T) {
	cfg, template := adapterFixture(t, StrategyParallel, "a", "b", "boss")
	cfg.Participants = cfg.Participants[:2]
	cfg.Settings.Aggregator = "boss"
	graph, err := AdaptPlan(cfg, PlanRequest{Template: template})
	if err != nil {
		t.Fatal(err)
	}
	if graph.Tasks[2].Owner != "boss" || len(graph.Tasks[2].DependsOn) != 2 {
		t.Fatalf("aggregator prerequisites lost: %+v", graph.Tasks)
	}
}

func TestPlanAdapterUsesStrictHierarchicalParserAndCapturedSource(t *testing.T) {
	cfg, template := adapterFixture(t, StrategyHierarchical, "boss", "a", "b")
	cfg.Settings.Manager = "boss"
	proposal := `[{"agent":"a","task":"research"},{"agent":"b","task":"write"}]`
	source := verification.Source{Resource: "manager-plan", Revision: "1", Digest: evidence.Digest([]byte(proposal)), RetrievedAt: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)}
	grant := planning.ResourceGrant{Resource: source.Resource, Revision: source.Revision, Access: planning.AccessRead}
	template.Sources = []verification.Source{source}
	template.Resources = []planning.ResourceGrant{grant}
	for i := range template.Tasks {
		template.Tasks[i].Sources = []string{source.Resource}
		template.Tasks[i].Resources = []planning.ResourceGrant{grant}
	}
	graph, err := AdaptPlan(cfg, PlanRequest{Template: template, DelegationPlan: proposal, DelegationSource: source.Resource})
	if err != nil {
		t.Fatal(err)
	}
	if graph.Tasks[0].Instruction != "research" || graph.Tasks[1].Instruction != "write" || graph.Tasks[2].Owner != "boss" || len(graph.Tasks[2].DependsOn) != 2 {
		t.Fatalf("bad hierarchy: %+v", graph.Tasks)
	}
	if graph.Tasks[0].Budget != template.Tasks[1].Budget || graph.Tasks[0].Criteria[0] != "check" {
		t.Fatal("model changed host constraints")
	}
	for _, bad := range []string{`[{"agent":"a","task":"research"}]`, `[{"agent":"a","task":"research","tools":["write"]},{"agent":"b","task":"write"}]`} {
		template.Sources[0].Digest = evidence.Digest([]byte(bad))
		_, err = AdaptPlan(cfg, PlanRequest{Template: template, DelegationPlan: bad, DelegationSource: source.Resource})
		if !errors.Is(err, cortex.ErrInvalidDelegationPlan) {
			t.Fatalf("unsafe plan accepted: %v", err)
		}
	}
	template.Sources[0].Digest = evidence.Digest([]byte("other"))
	if _, err = AdaptPlan(cfg, PlanRequest{Template: template, DelegationPlan: proposal, DelegationSource: source.Resource}); err == nil {
		t.Fatal("uncaptured proposal accepted")
	}
}

func TestPlanAdapterRejectsImplicitAuthorityChanges(t *testing.T) {
	for _, mode := range []string{"scope", "extra_task", "dependency", "duplicate_owner", "lossy_config"} {
		cfg, template := adapterFixture(t, StrategySequential, "a", "b")
		switch mode {
		case "scope":
			cfg.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}}
		case "extra_task":
			cfg.Participants = cfg.Participants[:1]
		case "dependency":
			template.Tasks[1].DependsOn = []id.ID{template.Tasks[0].ID}
		case "duplicate_owner":
			template.Tasks[1].Owner = "a"
		case "lossy_config":
			cfg.Settings.Model = string([]byte{0xff})
		}
		if _, err := AdaptPlan(cfg, PlanRequest{Template: template}); err == nil {
			t.Fatalf("accepted %s", mode)
		}
	}
	for _, strategy := range []string{StrategyRouter, StrategyDebate} {
		cfg, template := adapterFixture(t, strategy, "a")
		if _, err := AdaptPlan(cfg, PlanRequest{Template: template}); !errors.Is(err, ErrUnknownStrategy) {
			t.Fatalf("unsupported static mapping: %v", err)
		}
	}
}

func TestExecutionConfigDigestRejectsLossyScope(t *testing.T) {
	cfg, _ := adapterFixture(t, StrategySequential, "a")
	cfg.Scope.Levels[0].Value = string([]byte{0xff})
	if _, err := ExecutionConfigDigest(cfg); err == nil {
		t.Fatal("lossy scope hashed as a valid configuration")
	}
}
