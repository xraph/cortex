package coordination_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

type planningReplanCommandStore struct {
	coordination.PlanningOperatorStore
	current planning.Plan
	next    planning.Plan
	digest  string
	commits int
	calls   int
}

func (s *planningReplanCommandStore) GetPlan(context.Context, id.ID) (planning.Plan, error) {
	return s.current, nil
}
func (s *planningReplanCommandStore) CommitPlanReplan(_ context.Context, c coordination.PlanReplanControl) (planning.Plan, error) {
	s.calls++
	_, digest, err := c.Freeze()
	if err != nil {
		return planning.Plan{}, err
	}
	if s.digest != "" {
		if s.digest != digest {
			return planning.Plan{}, planning.ErrConflict
		}
		return s.next, nil
	}
	s.digest = digest
	s.current = s.next
	s.commits++
	return s.next, nil
}

type planningReplanCommandResolver struct {
	control coordination.PlanReplanControl
}

func (r *planningReplanCommandResolver) ResolvePlanningReplan(context.Context, coordination.PlanningCommand) (coordination.PlanReplanControl, error) {
	return r.control, nil
}

func TestPlanningOperatorReplanLostAcknowledgment(t *testing.T) {
	a := liveReplanAdmission(t)
	p := a.Request.Progress.Plan
	next := p
	next.Revision++
	next.PreviousDigest = p.Digest
	var err error
	next, err = planning.Seal(next, planning.Limits{})
	require.NoError(t, err)
	storage := &planningReplanCommandStore{current: p, next: next}
	ports := &planReplanPorts{}
	runtime, err := coordination.NewPlanReplanner(storage, ports, ports, ports)
	require.NoError(t, err)
	resolver := &planningReplanCommandResolver{control: coordination.PlanReplanControl{Request: a.Request, Operation: "commit"}}
	auth := &planningOperatorAuthority{denyAt: 2}
	operator, err := coordination.NewPlanningOperator(coordination.PlanningOperatorConfig{Store: storage, Source: &planningOperatorSource{plan: p}, Authority: auth, Clock: planningOperatorClock{}, Replans: resolver, Replanner: runtime})
	require.NoError(t, err)
	ctx := cortex.WithScope(context.Background(), p.Scope)
	command := coordination.PlanningCommand{Operation: "accept_revision", OperationID: a.Request.ID, PlanID: p.ID, ExpectedRevision: p.Revision, ExpectedDigest: p.Digest, Reason: "accept reviewed proposal"}
	_, err = operator.Command(ctx, command)
	require.ErrorIs(t, err, coordination.ErrPlanningDenied)
	require.Equal(t, 1, storage.commits)
	auth.denyAt = 0
	receipt, err := operator.Command(ctx, command)
	require.NoError(t, err)
	require.Equal(t, next.Revision, receipt.PlanRevision)
	require.Equal(t, next.Digest, receipt.PlanDigest)
	require.Equal(t, 1, storage.commits)
	require.Equal(t, 2, storage.calls)
	resolver.control.Request.Reason = "changed retained control"
	_, err = operator.Command(ctx, command)
	require.ErrorIs(t, err, planning.ErrConflict)
	require.Equal(t, 1, storage.commits)
	auth.denyAt = auth.calls + 1
	before := storage.calls
	_, err = operator.Command(ctx, command)
	require.ErrorIs(t, err, coordination.ErrPlanningDenied)
	require.Equal(t, before, storage.calls)
}
