package coordination_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

type planningCancelStore struct {
	coordination.PlanningCancellationStore
	producer   *run.Run
	assignment coordination.Assignment
	missing    bool
}

func (s *planningCancelStore) GetTaskDispatch(context.Context, id.ID) (coordination.TaskDispatch, error) {
	return coordination.TaskDispatch{Assignment: s.assignment}, nil
}

func (s *planningCancelStore) GetRun(context.Context, id.AgentRunID) (*run.Run, error) {
	if s.missing {
		return nil, cortex.ErrRunNotFound
	}
	return s.producer, nil
}

type planningCancelPorts struct {
	denied    bool
	fenced    bool
	escape    bool
	calls     int
	cancelErr error
	actor     string
}

func (p *planningCancelPorts) WithPlanningCancellationFence(_ context.Context, _ coordination.PlanningCommand, next func() error) error {
	if p.escape {
		return nil
	}
	p.fenced = true
	defer func() { p.fenced = false }()
	return next()
}
func (p *planningCancelPorts) AuthorizePlanningCancellation(context.Context, coordination.PlanningCommand, coordination.Assignment) (string, error) {
	if p.denied || !p.fenced {
		return "", errors.New("denied")
	}
	return p.actor, nil
}
func (p *planningCancelPorts) CancelRun(context.Context, id.AgentRunID) error {
	if !p.fenced {
		return errors.New("missing fence")
	}
	p.calls++
	return p.cancelErr
}

func TestPlanningCancellationPreservesOwnershipAndCharges(t *testing.T) {
	for _, mode := range []string{"running", "absent", "runtime_error", "denied", "escaped_fence"} {
		t.Run(mode, func(t *testing.T) {
			storage, f, _, _, _ := planningOperatorFixture(t)
			claim, err := storage.GetTaskClaim(f.Context, f.Plan.ID, f.Plan.Tasks[0].ID)
			require.NoError(t, err)
			before, err := storage.GetBudgetSnapshot(f.Context, f.Plan.ID)
			require.NoError(t, err)
			ports := &planningCancelPorts{actor: f.ProducerAssignment.Handoff.Sender.Principal}
			proxy := &planningCancelStore{PlanningCancellationStore: storage, assignment: f.ProducerAssignment, producer: &run.Run{ID: claim.RunID, Scope: f.Plan.Scope, State: run.StateRunning}}
			switch mode {
			case "absent":
				proxy.missing = true
			case "runtime_error":
				ports.cancelErr = errors.New("cancellation uncertain")
			case "denied":
				ports.denied = true
			case "escaped_fence":
				ports.escape = true
			}
			cancellation, err := coordination.NewPlanningCancellation(proxy, ports, ports)
			require.NoError(t, err)
			command := coordination.PlanningCommand{Operation: "cancel", OperationID: f.ProducerAssignment.Handoff.AssignmentID, PlanID: f.Plan.ID, ExpectedRevision: f.Plan.Revision, ExpectedDigest: f.Plan.Digest, TaskID: claim.TaskID, ExpectedClaimRevision: claim.Revision, ExpectedAssignmentRevision: f.ProducerAssignment.Revision, Reason: "operator cancellation"}
			cancelled, err := cancellation.CancelPlanningAssignment(f.Context, command)
			if mode == "running" || mode == "absent" {
				require.NoError(t, err)
				require.Equal(t, coordination.AssignmentCancelled, cancelled.State)
				again, retryErr := cancellation.CancelPlanningAssignment(f.Context, command)
				require.NoError(t, retryErr)
				require.Equal(t, cancelled, again)
				if mode == "running" {
					require.Equal(t, 1, ports.calls)
				} else {
					require.Zero(t, ports.calls)
				}
				ports.denied = true
				_, retryErr = cancellation.CancelPlanningAssignment(f.Context, command)
				require.ErrorIs(t, retryErr, coordination.ErrPlanningDenied)
			} else {
				require.Error(t, err)
				unchanged, readErr := storage.GetAssignment(f.Context, command.OperationID)
				require.NoError(t, readErr)
				require.Equal(t, f.ProducerAssignment, unchanged)
				if mode == "runtime_error" {
					require.ErrorIs(t, err, ports.cancelErr)
					require.Equal(t, 1, ports.calls)
				} else {
					require.Zero(t, ports.calls)
				}
			}
			afterClaim, err := storage.GetTaskClaim(f.Context, f.Plan.ID, claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, claim, afterClaim)
			after, err := storage.GetBudgetSnapshot(f.Context, f.Plan.ID)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}
