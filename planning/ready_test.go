package planning

import (
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func acceptedState(t *testing.T, p Plan, task Task, at time.Time) TaskState {
	t.Helper()
	artifact, err := evidence.Seal(evidence.Artifact{Data: []byte("checked output"), Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: p.Scope, Version: "1", Producer: ArtifactProducer(p, task.ID), MediaType: task.Output.MediaType, Sensitivity: evidence.SensitivityInternal, CreatedAt: at.Add(-time.Minute), RetainUntil: at.Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	binding := verification.Binding{Scope: p.Scope, RunID: id.NewAgentRunID(), ContractDigest: p.Contract.Digest, Candidate: artifact.Ref, Context: verification.ContextSnapshot{PrincipalRef: "worker", AuthorityRevision: "1", PreparedDigest: evidence.Digest([]byte("prepared")), PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}
	report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: binding, CheckedAt: at.Add(-time.Second), ExpiresAt: at.Add(time.Minute), Outcomes: []verification.Outcome{{CriterionID: "checked", Verifier: "tests", VerifierRevision: "1", Status: verification.StatusPassed}}})
	if err != nil {
		t.Fatal(err)
	}
	return TaskState{TaskID: task.ID, PlanDigest: p.Digest, Status: StatusSucceeded, Current: binding, Reports: []verification.Report{report}}
}

func TestReadyRequiresVerifiedDependencies(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, mode := range []string{"valid", "missing", "failed", "stale", "changed_candidate", "revoked", "wrong_type", "oversized", "unverified"} {
		t.Run(mode, func(t *testing.T) {
			p := fixturePlan(t)
			child := p.Tasks[0]
			child.ID = id.New(PrefixTask)
			child.Output.Name = "summary"
			child.DependsOn = []id.ID{p.Tasks[0].ID}
			p.Tasks = append(p.Tasks, child)
			var err error
			p, err = Seal(p, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			state := acceptedState(t, p, p.Tasks[0], at)
			switch mode {
			case "missing":
				state.Reports = nil
			case "failed":
				state.Status = StatusFailed
			case "stale":
				at = at.Add(time.Hour)
			case "changed_candidate":
				state.Current.Candidate.SHA256 = evidence.Digest([]byte("changed"))
			case "revoked":
				state.Current.Context.AuthorityRevision = "revoked"
			case "wrong_type":
				state.Current.Candidate.MediaType = "application/json"
			case "oversized":
				state.Current.Candidate.Bytes = child.Output.MaxBytes + 1
			case "unverified":
				state.Status = StatusVerifying
			}
			ready, err := Ready(p, []TaskState{state}, at)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "valid" {
				if len(ready) != 1 || ready[0] != child.ID {
					t.Fatalf("verified dependency did not unlock child: %v", ready)
				}
			} else if len(ready) != 0 {
				t.Fatalf("unsafe dependency unlocked child: %v", ready)
			}
		})
	}
}

func TestReadyRejectsDuplicateOrSupersededState(t *testing.T) {
	p := fixturePlan(t)
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	state := acceptedState(t, p, p.Tasks[0], at)
	if _, err := Ready(p, []TaskState{state, state}, at); err == nil {
		t.Fatal("duplicate state accepted")
	}
	state.PlanDigest = evidence.Digest([]byte("older plan"))
	if _, err := Ready(p, []TaskState{state}, at); err == nil {
		t.Fatal("superseded state accepted")
	}
}

func TestTaskCannotReuseAnotherTasksVerifiedArtifact(t *testing.T) {
	p := fixturePlan(t)
	child := p.Tasks[0]
	child.ID = id.New(PrefixTask)
	child.Output.Name = "summary"
	child.DependsOn = []id.ID{p.Tasks[0].ID}
	p.Tasks = append(p.Tasks, child)
	var err error
	p, err = Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	wrong := acceptedState(t, p, child, at)
	wrong.TaskID = p.Tasks[0].ID
	ready, err := Ready(p, []TaskState{wrong}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("another task's artifact unlocked work: %v", ready)
	}
}

func TestReadyRequiresTransitiveArtifactProvenance(t *testing.T) {
	for _, includeInput := range []bool{false, true} {
		p := fixturePlan(t)
		child := p.Tasks[0]
		child.ID = id.New(PrefixTask)
		child.Output.Name = "summary"
		child.DependsOn = []id.ID{p.Tasks[0].ID}
		grandchild := child
		grandchild.ID = id.New(PrefixTask)
		grandchild.Output.Name = "review"
		grandchild.DependsOn = []id.ID{child.ID}
		p.Tasks = append(p.Tasks, child, grandchild)
		var err error
		p, err = Seal(p, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		parentState := acceptedState(t, p, p.Tasks[0], at)
		childState := acceptedState(t, p, child, at)
		if includeInput {
			childState.Current.Evidence = []evidence.Ref{parentState.Current.Candidate}
			childState.Reports[0].Binding = childState.Current
			childState.Reports[0], err = verification.SealReport(childState.Reports[0])
			if err != nil {
				t.Fatal(err)
			}
		}
		ready, err := Ready(p, []TaskState{parentState, childState}, at)
		if err != nil {
			t.Fatal(err)
		}
		if includeInput {
			if len(ready) != 1 || ready[0] != grandchild.ID {
				t.Fatalf("valid chain not ready: %v", ready)
			}
		} else if len(ready) != 0 {
			t.Fatalf("missing input provenance accepted: %v", ready)
		}
	}
}

func TestRevisionCannotRelabelOldEvidenceAsCurrent(t *testing.T) {
	p := fixturePlan(t)
	child := p.Tasks[0]
	child.ID = id.New(PrefixTask)
	child.Output.Name = "summary"
	child.DependsOn = []id.ID{p.Tasks[0].ID}
	p.Tasks = append(p.Tasks, child)
	var err error
	p, err = Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	old := acceptedState(t, p, p.Tasks[0], at)
	proposal, err := Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	proposal.Tasks[0].Instruction = "new requirements"
	next, err := ApplyRevision(p, Revision{Plan: proposal, Actor: "host", Reason: "changed task"}, p.Revision, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	old.PlanDigest = next.Digest
	ready, err := Ready(next, []TaskState{old}, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("old evidence relabeled as current: %v", ready)
	}
}

func TestTaskAcceptanceHonorsOwnDeadline(t *testing.T) {
	p := fixturePlan(t)
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	p.Tasks[0].Budget.Deadline = at.Add(time.Second)
	var err error
	p, err = Seal(p, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	state := acceptedState(t, p, p.Tasks[0], at)
	if err = ValidateTaskAcceptance(p, p.Tasks[0].ID, []TaskState{state}, at); err != nil {
		t.Fatal(err)
	}
	if err = ValidateTaskAcceptance(p, p.Tasks[0].ID, []TaskState{state}, at.Add(time.Second)); err == nil {
		t.Fatal("fresh reports cannot extend the task deadline")
	}
}
