package coordination_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

type planReplanStore struct {
	coordination.PlanReplanStore
	admission coordination.PlanReplanAdmission
	admitErr  error
	fresh     bool
	pending   bool
	observed  atomic.Int32
}

func (s *planReplanStore) AdmitPlanReplan(context.Context, coordination.PlanReplanRequest) (coordination.PlanReplanAdmission, error) {
	a := s.admission
	a.Fresh = s.fresh
	return a, s.admitErr
}
func (s *planReplanStore) GetPlan(context.Context, id.ID) (planning.Plan, error) {
	return s.admission.Request.Progress.Plan, nil
}
func (s *planReplanStore) ListPlanReplans(context.Context, id.ID) ([]coordination.PlanReplanAdmission, error) {
	if s.pending {
		return []coordination.PlanReplanAdmission{s.admission}, nil
	}
	return nil, nil
}
func (s *planReplanStore) GetPlanReplanOutcome(context.Context, id.ID) (*coordination.PlanReplanOutcome, error) {
	return nil, nil
}
func (s *planReplanStore) ObservePlanReplan(context.Context, coordination.PlanReplanControl) (*coordination.PlanReplanOutcome, error) {
	s.observed.Add(1)
	return nil, nil
}

type planReplanPorts struct {
	deny      bool
	execute   func(context.Context) error
	authorize atomic.Int32
}

func (p *planReplanPorts) WithPlanReplanFence(_ context.Context, c coordination.PlanReplanControl, next func(coordination.PlanReplanControl) error) error {
	return next(c)
}
func (p *planReplanPorts) AuthorizePlanReplan(context.Context, coordination.PlanReplanControl) error {
	p.authorize.Add(1)
	if p.deny {
		return errors.New("revoked")
	}
	return nil
}
func (p *planReplanPorts) ExecutePlanReplan(ctx context.Context, _ coordination.PlanReplanAdmission) error {
	return p.execute(ctx)
}

type planReplanResolver struct {
	request coordination.PlanReplanRequest
	resolve func(context.Context) error
}

func (r planReplanResolver) ResolvePlanReplan(ctx context.Context, _ id.ID) (*coordination.PlanReplanRequest, error) {
	if r.resolve != nil {
		if err := r.resolve(ctx); err != nil {
			return nil, err
		}
	}
	return &r.request, nil
}
func (r planReplanResolver) ResolvePlanReplanControl(_ context.Context, a coordination.PlanReplanAdmission, _ *coordination.PlanReplanOutcome) (*coordination.PlanReplanControl, error) {
	return &coordination.PlanReplanControl{Request: a.Request, Operation: "observe"}, nil
}

func liveReplanAdmission(t *testing.T) coordination.PlanReplanAdmission {
	t.Helper()
	req := replanRequestFixture(t)
	now := time.Now().UTC()
	p := req.Progress.Plan
	p.Budget.Deadline = now.Add(time.Hour)
	for i := range p.Tasks {
		p.Tasks[i].Budget.Deadline = p.Budget.Deadline
	}
	var err error
	p, err = planning.Seal(p, planning.Limits{})
	require.NoError(t, err)
	req.Progress.Plan = p
	req.Progress.At = now
	req.Progress.StartedAt = now.Add(-time.Hour)
	req.Progress.LastProgressAt = now.Add(-time.Hour)
	req.Progress.Account.Policy.Deadline = p.Budget.Deadline
	req.Reservation.PlanDigest = p.Digest
	req.Reservation.Deadline = p.Budget.Deadline
	req, digest, err := req.Freeze()
	require.NoError(t, err)
	_, reservationDigest, err := req.Reservation.Freeze()
	require.NoError(t, err)
	a := coordination.PlanReplanAdmission{SchemaVersion: 1, Request: req, RequestDigest: digest, At: now, Reservation: execution.Reservation{Request: req.Reservation, RequestDigest: reservationDigest, CreatedAt: now}}
	require.NoError(t, a.Validate())
	return a
}
func TestPlanReplanGuardDeniesUnknownHistoricalAndRevokedDispatch(t *testing.T) {
	a := liveReplanAdmission(t)
	ctx := cortex.WithScope(context.Background(), a.Request.Progress.Plan.Scope)
	for _, mode := range []string{"ambiguous", "historical", "revoked", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			s := &planReplanStore{admission: a, fresh: true}
			calls := 0
			ports := &planReplanPorts{execute: func(context.Context) error { calls++; return nil }}
			switch mode {
			case "ambiguous":
				s.admitErr = errors.New("unknown acknowledgment")
			case "historical":
				s.fresh = false
			case "revoked":
				ports.deny = true
			}
			controller, err := coordination.NewPlanReplanner(s, ports, ports, ports)
			require.NoError(t, err)
			_, err = controller.Execute(ctx, a.Request)
			if mode == "ambiguous" || mode == "revoked" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if mode == "fresh" {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
			require.EqualValues(t, 1, ports.authorize.Load())
		})
	}
}
func TestPlanReplanServiceRestartOnlyObservesPendingAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := liveReplanAdmission(t)
		s := &planReplanStore{admission: a, pending: true}
		ports := &planReplanPorts{execute: func(context.Context) error { t.Error("pending admission dispatched"); return nil }}
		var scans atomic.Int32
		cfg := coordination.PlanReplanServiceConfig{Store: s, Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: a.Request.Progress.Plan.Scope, PlanID: a.Request.Progress.Plan.ID}}}, nil
		}), AuthorizeScan: func(context.Context, id.ID) error { scans.Add(1); return nil }, Resolver: planReplanResolver{request: a.Request}, Fence: ports, Authorizer: ports, Runtime: ports, Interval: time.Second}
		service, err := coordination.NewPlanReplanService(cfg)
		require.NoError(t, err)
		for range 2 {
			require.NoError(t, service.Start(context.Background()))
			synctest.Wait()
			require.NoError(t, service.Stop(context.Background()))
		}
		require.GreaterOrEqual(t, s.observed.Load(), int32(2))
		require.GreaterOrEqual(t, scans.Load(), int32(2))
	})
}
func TestPlanReplanServiceWaitsForModelCallbackDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := liveReplanAdmission(t)
		s := &planReplanStore{admission: a, fresh: true}
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		ports := &planReplanPorts{execute: func(context.Context) error { close(entered); <-release; return nil }}
		cfg := coordination.PlanReplanServiceConfig{Store: s, Discovery: taskPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
			return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: a.Request.Progress.Plan.Scope, PlanID: a.Request.Progress.Plan.ID}}}, nil
		}), AuthorizeScan: func(context.Context, id.ID) error { return nil }, Resolver: planReplanResolver{request: a.Request}, Fence: ports, Authorizer: ports, Runtime: ports, Interval: time.Second}
		service, err := coordination.NewPlanReplanService(cfg)
		require.NoError(t, err)
		require.NoError(t, service.Start(context.Background()))
		<-entered
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.ErrorIs(t, service.Stop(stop), context.DeadlineExceeded)
		once.Do(func() { close(release) })
		require.NoError(t, service.Stop(context.Background()))
	})
}
