package orchestration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func teamBudgetFixture() (BudgetPolicy, execution.ReservationRequest) {
	allocation := execution.Resources{Steps: 2, Tokens: 600, ToolCalls: 2, FanOut: 1}
	p := BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: evidence.Digest([]byte("team")), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "team"}}}, Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 8}, Participants: map[string]execution.Resources{"worker": allocation}, Deadline: time.Now().Add(time.Hour), TokenOnly: true}
	r := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ExecutionID, PlanDigest: p.PreparedDigest, Scope: p.Scope, TaskID: id.NewAgentRunID(), OwnerRef: "worker", Kind: execution.ReservationLeaf, Allocation: allocation, Deadline: p.Deadline}
	return p, r
}

func TestOrchestrationBudgetPolicyBinding(t *testing.T) {
	p, request := teamBudgetFixture()
	frozen, digest, err := p.Freeze()
	require.NoError(t, err)
	require.True(t, evidence.ValidDigest(digest))
	require.NoError(t, frozen.ValidateReservation(request))
	delete(p.Participants, "worker")
	require.Contains(t, frozen.Participants, "worker")
	for _, mode := range []string{"account", "prepared", "scope", "participant", "run", "parent", "branch", "allocation", "deadline", "pricing"} {
		t.Run(mode, func(t *testing.T) {
			r := request
			switch mode {
			case "account":
				r.PlanID = id.NewOrchestrationID()
			case "prepared":
				r.PlanDigest = evidence.Digest([]byte("other"))
			case "scope":
				r.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}}
			case "participant":
				r.OwnerRef = "outsider"
			case "run":
				r.TaskID = id.New("ptask")
			case "parent":
				r.ParentID = id.New(execution.PrefixReservation)
			case "branch":
				r.Kind = execution.ReservationBranch
				r.Allocation.FanOut = 2
			case "allocation":
				r.Allocation.Tokens++
			case "deadline":
				r.Deadline = r.Deadline.Add(time.Second)
			case "pricing":
				r.MoneyBound = true
				r.PricingRevision = "price"
			}
			require.ErrorIs(t, frozen.ValidateReservation(r), execution.ErrInvalidReservation)
		})
	}
}

func TestOrchestrationBudgetPolicyRejectsInvalidBounds(t *testing.T) {
	for _, mode := range []string{"schema", "id", "digest", "scope", "roster", "name", "branch", "ceiling", "negative", "pricing", "replans"} {
		t.Run(mode, func(t *testing.T) {
			p, _ := teamBudgetFixture()
			switch mode {
			case "schema":
				p.SchemaVersion = 2
			case "id":
				p.ExecutionID = id.NewAgentRunID()
			case "digest":
				p.PreparedDigest = "bad"
			case "scope":
				p.Scope = cortex.Scope{}
			case "roster":
				p.Participants = nil
			case "name":
				p.Participants["worker@remote"] = p.Participants["worker"]
			case "branch":
				p.Participants["worker"] = execution.Resources{FanOut: 2}
			case "ceiling":
				p.Limits.Tokens = 1
			case "negative":
				p.Limits.Steps = -1
			case "pricing":
				p.PricingRevision = "price"
			case "replans":
				p.Limits.Replans = 1
			}
			_, _, err := p.Freeze()
			require.ErrorIs(t, err, execution.ErrInvalidReservation)
		})
	}
}

func TestOrchestrationBudgetParticipantDigests(t *testing.T) {
	p, _ := teamBudgetFixture()
	original := evidence.Digest([]byte("worker-v1"))
	p.ParticipantDigests = map[string]string{"worker": original}
	frozen, digest, err := p.Freeze()
	require.NoError(t, err)
	p.ParticipantDigests["worker"] = evidence.Digest([]byte("worker-v2"))
	require.Equal(t, original, frozen.ParticipantDigests["worker"])
	_, changed, err := p.Freeze()
	require.NoError(t, err)
	require.NotEqual(t, digest, changed)
	for _, pins := range []map[string]string{
		{"worker": "bad"},
		{"outsider": original},
		{"worker": original, "outsider": original},
	} {
		p.ParticipantDigests = pins
		_, _, err = p.Freeze()
		require.ErrorIs(t, err, execution.ErrInvalidReservation)
	}
}
