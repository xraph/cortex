package planning

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func fixturePlan(t *testing.T) Plan {
	t.Helper()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "one"}}}
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "produce a checked report", Criteria: []verification.Criterion{{ID: "checked", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	if err != nil {
		t.Fatal(err)
	}
	budget := execution.Budget{MaxSteps: 20, MaxTokens: 2000, MaxToolCalls: 10, Deadline: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)}
	p, err := Seal(Plan{SchemaVersion: 1, ID: id.New(PrefixPlan), Scope: scope, Revision: 1, Objective: contract.Objective, Contract: contract, Budget: budget, Owners: []string{"researcher"}, Resources: []ResourceGrant{{Resource: "source", Revision: "1", Access: AccessRead}}, Actor: "host", Reason: "initial request", Tasks: []Task{{ID: id.New(PrefixTask), Instruction: "produce report", Owner: "researcher", Criteria: []string{"checked"}, Budget: budget, Resources: []ResourceGrant{{Resource: "source", Revision: "1", Access: AccessRead}}, Output: ArtifactRequirement{Name: "report", MediaType: "text/plain", MaxBytes: 4096}}}}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPlanRejectsUnsafeGraphs(t *testing.T) {
	for _, mode := range []string{"cycle", "unknown_dependency", "duplicate_task", "missing_criterion", "unknown_criterion", "owner", "resource", "scope", "budget", "deadline", "artifact_owner"} {
		t.Run(mode, func(t *testing.T) {
			p := fixturePlan(t)
			switch mode {
			case "cycle":
				p.Tasks[0].DependsOn = []id.ID{p.Tasks[0].ID}
			case "unknown_dependency":
				p.Tasks[0].DependsOn = []id.ID{id.New(PrefixTask)}
			case "duplicate_task":
				p.Tasks = append(p.Tasks, p.Tasks[0])
			case "missing_criterion":
				p.Tasks[0].Criteria = nil
			case "unknown_criterion":
				p.Tasks[0].Criteria = []string{"invented"}
			case "owner":
				p.Tasks[0].Owner = "outsider"
			case "resource":
				p.Tasks[0].Resources[0].Access = AccessWrite
			case "scope":
				p.Scope.Levels[0].Value = "other"
			case "budget":
				p.Tasks[0].Budget.MaxTokens++
			case "deadline":
				p.Tasks[0].Budget.Deadline = p.Budget.Deadline.Add(time.Second)
			case "artifact_owner":
				task := p.Tasks[0]
				task.ID = id.New(PrefixTask)
				p.Tasks = append(p.Tasks, task)
			}
			if _, err := Seal(p, Limits{}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted %s: %v", mode, err)
			}
		})
	}
}

func TestPlanSealingDetachesAndDetectsMutation(t *testing.T) {
	p := fixturePlan(t)
	sealed, err := Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	p.Tasks[0].Resources[0].Revision = "changed"
	if err = Validate(sealed, Limits{}); err != nil {
		t.Fatal(err)
	}
	if err = Validate(p, Limits{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("mutation accepted")
	}
	data, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	var replay Plan
	if err = json.Unmarshal(data, &replay); err != nil {
		t.Fatal(err)
	}
	if err = Validate(replay, Limits{}); err != nil {
		t.Fatal(err)
	}
	if replay.Digest != sealed.Digest {
		t.Fatal("unstable replay")
	}
}

func TestRevisionConflictAndAcceptancePreserved(t *testing.T) {
	p := fixturePlan(t)
	detached, err := Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	proposal := Revision{Plan: detached, Actor: "host", Reason: "refine instructions"}
	if _, err = ApplyRevision(p, proposal, 0, Limits{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	proposal.Plan.Tasks[0].Instruction = "refined task"
	next, err := ApplyRevision(p, proposal, p.Revision, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 2 || next.PreviousDigest != p.Digest || next.Digest == p.Digest || len(next.AffectedTasks) != 1 {
		t.Fatalf("bad revision: %+v", next)
	}
	if err = Validate(p, Limits{}); err != nil {
		t.Fatal("changed current plan", err)
	}
	proposal.Plan.Contract.Objective = "weaker objective"
	proposal.Plan.Contract, err = verification.SealContract(proposal.Plan.Contract)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ApplyRevision(p, proposal, p.Revision, Limits{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("acceptance changed: %v", err)
	}
}

func TestChangedArtifactInvalidatesDependents(t *testing.T) {
	p := fixturePlan(t)
	second := p.Tasks[0]
	second.ID = id.New(PrefixTask)
	second.Output.Name = "summary"
	second.DependsOn = []id.ID{p.Tasks[0].ID}
	third := second
	third.ID = id.New(PrefixTask)
	third.Output.Name = "review"
	third.DependsOn = []id.ID{second.ID}
	p.Tasks = append(p.Tasks, second, third)
	var err error
	p, err = Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	next, err := Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	next.Tasks[0].Output.MediaType = "application/json"
	revised, err := ApplyRevision(p, Revision{Plan: next, Actor: "host", Reason: "change report format"}, 1, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(revised.AffectedTasks) != 3 {
		t.Fatalf("dependent assumptions retained: %v", revised.AffectedTasks)
	}
	if _, err = Seal(p, Limits{MaxDepth: 2}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("depth limit ignored: %v", err)
	}
	if _, err = Seal(p, Limits{MaxEdges: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("edge limit ignored: %v", err)
	}
}

func TestRevisionCannotWidenGrants(t *testing.T) {
	for _, mode := range []string{"owner", "resource", "budget", "deadline", "scope", "criterion"} {
		t.Run(mode, func(t *testing.T) {
			p := fixturePlan(t)
			next, err := Seal(p, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "owner":
				next.Owners = append(next.Owners, "outsider")
			case "resource":
				next.Resources[0].Access = AccessWrite
			case "budget":
				next.Budget.MaxTokens++
			case "deadline":
				next.Budget.Deadline = next.Budget.Deadline.Add(time.Hour)
			case "scope":
				next.Scope.Levels[0].Value = "other"
			case "criterion":
				next.Tasks[0].Criteria = nil
			}
			if _, err = ApplyRevision(p, Revision{Plan: next, Actor: "host", Reason: "proposal"}, p.Revision, Limits{}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("widened %s: %v", mode, err)
			}
		})
	}
}

func TestPlanRejectsLossyTextAndUnboundedInputs(t *testing.T) {
	for _, mode := range []string{"text", "zero_budget", "unsupported_schema", "limits", "size"} {
		t.Run(mode, func(t *testing.T) {
			p := fixturePlan(t)
			limits := Limits{}
			switch mode {
			case "text":
				p.Tasks[0].Instruction = string([]byte{0xff})
			case "zero_budget":
				p.Budget.MaxTokens = 0
			case "unsupported_schema":
				p.SchemaVersion++
			case "limits":
				limits.MaxTasks = 257
			case "size":
				p.Reason = string(make([]byte, 1<<20))
				p.Reason = strings.ReplaceAll(p.Reason, "\x00", "a")
			}
			if _, err := Seal(p, limits); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted %s: %v", mode, err)
			}
		})
	}
}

func TestDAGValidationAndRevisionReplayIgnoreTraversalOrder(t *testing.T) {
	p := fixturePlan(t)
	for i := 1; i < 32; i++ {
		task := p.Tasks[0]
		task.ID = id.New(PrefixTask)
		task.Output.Name = task.ID.String()
		task.DependsOn = []id.ID{p.Tasks[i-1].ID}
		p.Tasks = append(p.Tasks, task)
	}
	for rotation := 0; rotation < len(p.Tasks); rotation++ {
		p.Tasks = append(p.Tasks[1:], p.Tasks[0])
		sealed, err := Seal(p, Limits{})
		if err != nil {
			t.Fatal(rotation, err)
		}
		proposal, err := Seal(sealed, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		proposal.Tasks[0].Instruction = "refined"
		revision := Revision{Plan: proposal, Actor: "host", Reason: "refine"}
		first, err := ApplyRevision(sealed, revision, 1, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		second, err := ApplyRevision(sealed, revision, 1, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if first.Digest != second.Digest {
			t.Fatal("revision replay changed its digest")
		}
	}
}

func TestPlanRejectsCycleAcrossTasks(t *testing.T) {
	p := fixturePlan(t)
	second := p.Tasks[0]
	second.ID = id.New(PrefixTask)
	second.Output.Name = "second"
	second.DependsOn = []id.ID{p.Tasks[0].ID}
	p.Tasks[0].DependsOn = []id.ID{second.ID}
	p.Tasks = append(p.Tasks, second)
	if _, err := Seal(p, Limits{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cycle accepted: %v", err)
	}
}
