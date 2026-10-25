package optimization

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

func orchestrationCaptureFixture(t *testing.T) (HarnessVersion, ExecutionRecord) {
	t.Helper()
	version := orchestrationVersionFixture(t)
	view, err := version.Orchestration.Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	end := start.Add(time.Second)
	teamID := id.NewOrchestrationID()
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: teamID, PreparedDigest: version.Orchestration.Prepared.Digest(), Scope: version.Scope, Limits: version.Policy.OrchestrationPresets[0].Limits, Deadline: start.Add(time.Hour), TokenOnly: true, Participants: map[string]execution.Resources{}, ParticipantDigests: map[string]string{}}
	receipt := EnvironmentReceipt{ID: "capture", Provider: "test", EnvironmentID: "isolated", Scope: version.Scope, AttemptID: id.New(PrefixAttempt), Snapshot: ContentRef{Ref: "snapshot", Revision: "1", Digest: digestBytes([]byte("snapshot"))}, Fence: Fence{Owner: "worker", Generation: 1}}
	capture := OrchestrationExecution{Run: orchestration.Run{ID: teamID, ConfigID: view.Config.ID, Scope: version.Scope, Strategy: view.Config.Strategy, Status: orchestration.StatusCompleted, Input: "task", Output: "answer", StartedAt: start, CompletedAt: &end}}
	for _, name := range []string{"worker", "second"} {
		prepared := view.Participants[name]
		limit := execution.Resources{Steps: 2, Tokens: 100, ToolCalls: 2, FanOut: 1}
		policy.Participants[name], policy.ParticipantDigests[name] = limit, prepared.Digest()
		childID := id.NewAgentRunID()
		request := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: teamID, PlanDigest: policy.PreparedDigest, Scope: version.Scope, TaskID: childID, OwnerRef: name, Kind: execution.ReservationLeaf, Allocation: limit, Deadline: policy.Deadline}
		_, digest, freezeErr := request.Freeze()
		if freezeErr != nil {
			t.Fatal(freezeErr)
		}
		settlement := execution.Settlement{ReservationID: request.ID, RequestDigest: digest, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 7}, Known: execution.KnownTokens}, Receipt: "trusted/" + childID.String()}
		_, settlementDigest, freezeErr := settlement.Freeze()
		if freezeErr != nil {
			t.Fatal(freezeErr)
		}
		reservation := execution.Reservation{Request: request, RequestDigest: digest, CreatedAt: start, Settlement: &execution.SettlementReceipt{Request: settlement, Digest: settlementDigest, At: end}}
		child, sealErr := SealExecution(ExecutionRecord{RunID: childID, PreparedDigest: prepared.Digest(), InputDigest: digestBytes([]byte("task")), Outcome: run.Outcome{Output: "answer", StopReason: run.StopFinalAnswer, CompletedAt: end}, State: run.StateCompleted, Usage: Usage{Tokens: 7, TokenQuality: UsageKnown, MoneyQuality: UsageUnknown, Receipt: settlement.Receipt}, StartedAt: start, CompletedAt: end, Duration: end.Sub(start), Environment: receipt})
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		capture.Run.AgentRunIDs = append(capture.Run.AgentRunIDs, childID)
		capture.Children = append(capture.Children, OrchestrationChildExecution{Reservation: reservation, Execution: &child})
	}
	capture.Budget = orchestration.BudgetSnapshot{Policy: policy, Charged: execution.Resources{Tokens: 14, Steps: 4, ToolCalls: 4, FanOut: 2}, Reservations: 2, Closed: true}
	record := ExecutionRecord{Orchestration: &capture, PreparedDigest: policy.PreparedDigest, InputDigest: digestBytes([]byte("task")), Outcome: run.Outcome{Output: "answer", StopReason: run.StopFinalAnswer, CompletedAt: end}, State: run.StateCompleted, StartedAt: start, CompletedAt: end, Duration: end.Sub(start), Environment: receipt}
	record.Usage, err = OrchestrationUsage(capture)
	if err != nil {
		t.Fatal(err)
	}
	return version, record
}

func TestTeamCaptureUsesOriginalReceipts(t *testing.T) {
	version, record := orchestrationCaptureFixture(t)
	sealed, err := SealExecution(record)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ExecutionID() != record.Orchestration.Run.ID || !sealed.RunID.IsNil() || sealed.Usage.Tokens != 14 || sealed.Usage.TokenQuality != UsageKnown || sealed.Usage.MoneyQuality != UsageUnknown {
		t.Fatalf("lost identity or measured usage: %+v", sealed)
	}
	if err = ValidateExecutionVersion(sealed, version); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	var restored ExecutionRecord
	if err = json.Unmarshal(wire, &restored); err != nil {
		t.Fatal(err)
	}
	roundtrip, err := SealExecution(restored)
	if err != nil || roundtrip.Digest != sealed.Digest {
		t.Fatalf("wire capture changed: %v", err)
	}
	sealed.Orchestration.Children[0].Reservation.Settlement.Request.Receipt = "tamper"
	if record.Orchestration.Children[0].Reservation.Settlement.Request.Receipt == "tamper" {
		t.Fatal("sealed capture aliases caller")
	}
	if _, err = SealExecution(sealed); err == nil {
		t.Fatal("accepted changed receipt")
	}
}

func TestTeamCaptureRejectsSubstitutedEvidence(t *testing.T) {
	_, record := orchestrationCaptureFixture(t)
	for name, mutate := range map[string]func(*ExecutionRecord){
		"agent-identity":      func(r *ExecutionRecord) { r.RunID = id.NewAgentRunID() },
		"budget-as-usage":     func(r *ExecutionRecord) { r.Usage.Tokens = 200 },
		"open-account":        func(r *ExecutionRecord) { r.Orchestration.Budget.Closed = false },
		"missing-reservation": func(r *ExecutionRecord) { r.Orchestration.Children = r.Orchestration.Children[:1] },
		"foreign-team":        func(r *ExecutionRecord) { r.Orchestration.Run.ID = id.NewOrchestrationID() },
		"repeated-child":      func(r *ExecutionRecord) { r.Orchestration.Children[1] = r.Orchestration.Children[0] },
		"changed-output":      func(r *ExecutionRecord) { r.Outcome.Output = "other" },
		"child-scope": func(r *ExecutionRecord) {
			r.Orchestration.Children[0].Execution.Environment.Scope.Levels[0].Value = "foreign"
		},
		"missing-child": func(r *ExecutionRecord) { r.Orchestration.Children[0].Execution = nil },
	} {
		t.Run(name, func(t *testing.T) {
			data, _ := json.Marshal(record)
			var bad ExecutionRecord
			if err := json.Unmarshal(data, &bad); err != nil {
				t.Fatal(err)
			}
			mutate(&bad)
			if _, err := SealExecution(bad); err == nil {
				t.Fatal("accepted substituted evidence")
			}
		})
	}
}

func TestTeamFailureKeepsUnknownAndPartialEvidence(t *testing.T) {
	_, record := orchestrationCaptureFixture(t)
	record.Orchestration.Run.Status = orchestration.StatusFailed
	record.State = run.StateFailed
	record.Outcome.StopReason = run.StopExecutionError
	record.Orchestration.Children[1].Execution = nil
	record.Orchestration.Children[1].Reservation.Settlement = nil
	record.Orchestration.Budget.Charged.Tokens = 107
	var err error
	record.Usage, err = OrchestrationUsage(*record.Orchestration)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealExecution(record)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Usage.Tokens != 7 || sealed.Usage.TokenQuality != UsageUnknown || sealed.Usage.MoneyQuality != UsageUnknown || len(sealed.Orchestration.Children) != 2 {
		t.Fatalf("partial evidence changed: %+v", sealed)
	}
	version, _ := orchestrationCaptureFixture(t)
	if err = ValidateExecutionVersion(sealed, version); !errors.Is(err, ErrIncompleteEvidence) {
		t.Fatalf("accepted foreign team version: %v", err)
	}
}

func TestTeamUsageOverflowRetainsOriginalReceipts(t *testing.T) {
	_, record := orchestrationCaptureFixture(t)
	capture := record.Orchestration
	for i := range capture.Children {
		receipt := capture.Children[i].Reservation.Settlement
		receipt.Request.Usage.Actual.Tokens = int64(^uint64(0) >> 1)
		_, digest, err := receipt.Request.Freeze()
		if err != nil {
			t.Fatal(err)
		}
		receipt.Digest = digest
	}
	usage, err := OrchestrationUsage(*capture)
	if err != nil || usage.TokenQuality != UsageUnknown || usage.Tokens != int64(^uint64(0)>>1) {
		t.Fatalf("overflow discarded evidence or claimed exact total: %+v %v", usage, err)
	}
	if capture.Children[0].Reservation.Settlement.Request.Usage.Actual.Tokens != int64(^uint64(0)>>1) {
		t.Fatal("overflow changed original receipt")
	}
}

func TestTeamArtifactsIncludeSupersededChildren(t *testing.T) {
	_, record := orchestrationCaptureFixture(t)
	ref := artifactFixture().Ref
	ref.Scope = record.Environment.Scope
	child := record.Orchestration.Children[0].Execution
	child.Artifacts = []ArtifactRef{ref}
	sealed, err := SealExecution(*child)
	if err != nil {
		t.Fatal(err)
	}
	record.Orchestration.Children[0].Execution = &sealed
	record, err = SealExecution(record)
	if err != nil {
		t.Fatal(err)
	}
	trial := Trial{Attempts: []Attempt{{Execution: &record}}}
	refs := TrialArtifactRefs(trial)
	if len(refs) != 1 || !equalControllerData(refs[0], ref) {
		t.Fatal("superseded child artifact lost")
	}
	if len(ExecutionRunIDs(record)) != 3 {
		t.Fatal("team child identities lost")
	}
	record.Orchestration.Children[0].Execution.State = run.StateFailed
	record.Orchestration.Children[0].Execution.Outcome.StopReason = run.StopExecutionError
	if ExecutionCompleted(record) {
		t.Fatal("partial team reported completed")
	}
}
