package coordination_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

func handoffFixture(t *testing.T) (coordination.Handoff, planning.Plan, coordination.HandoffAuthority, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "checked report", Criteria: []verification.Criterion{{ID: "checked", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	budget := execution.Budget{MaxSteps: 10, MaxTokens: 1000, MaxToolCalls: 5, Deadline: now.Add(time.Hour)}
	resources := []planning.ResourceGrant{{Resource: "report", Revision: "1", Access: planning.AccessWrite}}
	effect := execution.EffectRequirement{Tool: "publish", Revision: "1", Class: execution.EffectNonIdempotent, ReconcileRevision: "1", Retry: execution.RetryPolicy{MaxAttempts: 1}}
	p, err := planning.Seal(planning.Plan{SchemaVersion: 1, ID: id.New(planning.PrefixPlan), Scope: scope, Revision: 1, Objective: contract.Objective, Contract: contract, Budget: budget, Owners: []string{"worker"}, Resources: resources, Effects: []execution.EffectRequirement{effect}, Actor: "host", Reason: "requested", Tasks: []planning.Task{{ID: id.New(planning.PrefixTask), Instruction: "produce checked report", Owner: "worker", Criteria: []string{"checked"}, Resources: resources, Effects: []string{"publish"}, Budget: budget, Output: planning.ArtifactRequirement{Name: "report", MediaType: "text/plain", MaxBytes: 4096}}}}, planning.Limits{})
	require.NoError(t, err)
	authority := planning.Authority{Resources: resources, Effects: []execution.EffectRequirement{effect}, Budget: budget}
	h := coordination.Handoff{SchemaVersion: 1, ID: id.New(coordination.PrefixHandoff), AssignmentID: id.New(coordination.PrefixAssignment), Fence: 1, Scope: scope, PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: p.Tasks[0].ID, ParentRunID: id.NewAgentRunID(), Sender: coordination.Identity{Agent: "planner", Principal: "coordinator"}, Receiver: coordination.Identity{Agent: "worker", Principal: "worker-1"}, Purpose: coordination.HandoffWork, GrantRef: "grant-1", Authority: authority, ContractDigest: contract.Digest, Criteria: []string{"checked"}, Deadline: budget.Deadline}
	current := coordination.HandoffAuthority{Sender: h.Sender, Receiver: h.Receiver, AssignmentID: h.AssignmentID, ParentRunID: h.ParentRunID, Fence: h.Fence, GrantRef: h.GrantRef, Ceiling: authority, ProducerPrincipal: h.Sender.Principal}
	return h, p, current, now
}
func TestHandoffPreservesScopeAndDeadline(t *testing.T) {
	h, p, current, now := handoffFixture(t)
	sealed, err := coordination.SealHandoff(h)
	require.NoError(t, err)
	require.NoError(t, coordination.ValidateHandoff(sealed, p, current, now))
	h.Scope.Levels[0].Value = "changed"
	require.NoError(t, sealed.Validate())
	require.Equal(t, "one", sealed.Scope.Levels[0].Value)
	require.Equal(t, current.Ceiling.Budget.Deadline, sealed.Deadline)
}
func TestChildCannotEscalateAuthority(t *testing.T) {
	for _, mode := range []string{"scope", "plan", "revision", "task", "contract", "criterion", "grant", "sender", "receiver", "assignment", "fence", "tokens", "deadline", "resource", "effect", "expired", "review_write", "review_effect", "read_only_mutation", "self_review"} {
		t.Run(mode, func(t *testing.T) {
			h, p, current, now := handoffFixture(t)
			// Detach the proposal so tests cannot accidentally modify the host ceiling.
			h, err := coordination.SealHandoff(h)
			require.NoError(t, err)
			switch mode {
			case "scope":
				h.Scope.Levels[0].Value = "other"
			case "plan":
				h.PlanID = id.New(planning.PrefixPlan)
			case "revision":
				h.PlanRevision++
			case "task":
				h.TaskID = id.New(planning.PrefixTask)
			case "contract":
				h.ContractDigest = h.PlanDigest
			case "criterion":
				h.Criteria = []string{"easier"}
			case "grant":
				h.GrantRef = "invented"
			case "sender":
				h.Sender.Principal = "admin"
			case "receiver":
				h.Receiver.Principal = "admin"
			case "assignment":
				h.AssignmentID = id.New(coordination.PrefixAssignment)
			case "fence":
				h.Fence++
			case "tokens":
				h.Authority.Budget.MaxTokens++
			case "deadline":
				h.Deadline = h.Deadline.Add(time.Hour)
				h.Authority.Budget.Deadline = h.Deadline
			case "resource":
				h.Authority.Resources[0].Revision = "2"
			case "effect":
				h.Authority.Effects[0].Revision = "2"
			case "expired":
				now = h.Deadline
			case "read_only_mutation":
				h.Authority.Resources[0].Access = planning.AccessRead
			case "review_write":
				h.Purpose = coordination.HandoffReview
				h.Authority.Effects = nil
			case "review_effect":
				h.Purpose = coordination.HandoffReview
				h.Authority.Resources[0].Access = planning.AccessRead
			case "self_review":
				h.Purpose = coordination.HandoffReview
				h.Authority.Effects = nil
				h.Authority.Resources = nil
				h.Receiver.Principal = h.Sender.Principal
				current.Receiver = h.Receiver
			}
			sealed, err := coordination.SealHandoff(h)
			if err == nil {
				err = coordination.ValidateHandoff(sealed, p, current, now)
			}
			require.ErrorIs(t, err, coordination.ErrInvalidHandoff)
		})
	}
}
func TestHandoffRejectsUnknownVersionAndMutation(t *testing.T) {
	h, _, _, _ := handoffFixture(t)
	h.SchemaVersion = 2
	_, err := coordination.SealHandoff(h)
	require.ErrorIs(t, err, coordination.ErrInvalidHandoff)
	h.SchemaVersion = 1
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	h.Criteria[0] = "ignore verification"
	require.ErrorIs(t, h.Validate(), coordination.ErrInvalidHandoff)
}
func TestReviewHandoffAttenuatesToolsAndResources(t *testing.T) {
	h, p, current, now := handoffFixture(t)
	h.Purpose = coordination.HandoffReview
	h.Authority.Effects = nil
	h.Authority.Resources = []planning.ResourceGrant{{Resource: "report", Revision: "1", Access: planning.AccessRead}}
	h.Authority.Budget.MaxTokens = 100
	h, err := coordination.SealHandoff(h)
	require.NoError(t, err)
	require.NoError(t, coordination.ValidateHandoff(h, p, current, now))
}

func TestHandoffDecodeRejectsUnknownFieldsAndTrailingContent(t *testing.T) {
	h, _, _, _ := handoffFixture(t)
	h, err := coordination.SealHandoff(h)
	require.NoError(t, err)
	encoded, err := json.Marshal(h)
	require.NoError(t, err)
	decoded, err := coordination.DecodeHandoff(encoded)
	require.NoError(t, err)
	require.Equal(t, h, decoded)
	for _, input := range [][]byte{append(append([]byte(nil), encoded...), []byte(" {}")...), []byte(strings.Replace(string(encoded), `"schema_version":1`, `"schema_version":1,"ignore_policy":true`, 1)), bytes.Repeat([]byte(" "), coordination.MaxHandoffBytes+1)} {
		_, err = coordination.DecodeHandoff(input)
		require.ErrorIs(t, err, coordination.ErrInvalidHandoff)
	}
}
