package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
)

// PlanReplanResolver supplies trusted progress for new attempts and authenticated
// recovery/commit proof for retained attempts. Nil means pending. It must honor
// cancellation and never invoke models, effects or task dispatch itself.
type PlanReplanResolver interface {
	ResolvePlanReplan(context.Context, id.ID) (*PlanReplanRequest, error)
	ResolvePlanReplanControl(context.Context, PlanReplanAdmission, *PlanReplanOutcome) (*PlanReplanControl, error)
}

// PlanReplanServiceConfig connects shared discovery to bounded proposal work.
// AuthorizeScan runs before reading retained attempts and outcomes. Discovery
// must retain plans with pending attempts even after their deadlines expire.
type PlanReplanServiceConfig struct {
	Store            PlanReplanStore
	Discovery        TaskPlanSource
	AuthorizeScan    func(context.Context, id.ID) error
	Resolver         PlanReplanResolver
	Fence            PlanReplanFence
	Authorizer       PlanReplanAuthorizer
	Runtime          PlanReplanRuntime
	AttemptTimeout   time.Duration
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	MaxInFlight      int
	MaxPerScope      int
	ReportError      func(error)
}

// PlanReplanService owns proposal recovery and dispatch until callbacks drain.
// It does not dispatch tasks or reuse artifacts after committing a revision.
type PlanReplanService struct{ service *TaskService }

// NewPlanReplanService validates ports without starting background work.
func NewPlanReplanService(config PlanReplanServiceConfig) (*PlanReplanService, error) {
	if hostport.IsNil(config.Resolver) || config.AuthorizeScan == nil {
		return nil, ErrInvalidScheduler
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = 30 * time.Second
	}
	if config.AttemptTimeout < time.Millisecond || config.AttemptTimeout > 30*time.Second {
		return nil, ErrInvalidScheduler
	}
	controller, err := NewPlanReplanner(config.Store, config.Fence, config.Authorizer, config.Runtime)
	if err != nil {
		return nil, err
	}
	service, err := newIdentityOperation(planning.PrefixPlan, TaskServiceConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, ReportError: config.ReportError, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}}, config.AttemptTimeout, func(ctx context.Context, planID id.ID) error {
		return processPlanReplan(ctx, config, controller, planID)
	})
	if err != nil {
		return nil, err
	}
	return &PlanReplanService{service: service}, nil
}
func processPlanReplan(ctx context.Context, c PlanReplanServiceConfig, controller *PlanReplanner, planID id.ID) error {
	if err := c.AuthorizeScan(ctx, planID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := c.Store.GetPlan(ctx, planID)
	if err != nil {
		return err
	}
	if planning.Validate(current, planning.Limits{}) != nil {
		return planning.ErrInvalid
	}
	attempts, err := c.Store.ListPlanReplans(ctx, planID)
	if err != nil {
		return err
	}
	if len(attempts) > planning.MaxReplanAttempts {
		return planning.ErrInvalid
	}
	for _, attempt := range attempts {
		if attempt.Validate() != nil || attempt.Request.Progress.Plan.ID != planID {
			return planning.ErrConflict
		}
		outcome, readErr := c.Store.GetPlanReplanOutcome(ctx, attempt.Request.ID)
		if readErr != nil {
			return readErr
		}
		if outcome != nil {
			if outcome.Validate() != nil || outcome.AdmissionDigest != attempt.RequestDigest {
				return planning.ErrConflict
			}
			if outcome.Proposed == nil || current.Digest != attempt.Request.Progress.Plan.Digest {
				continue
			}
			committed, commitErr := c.Store.GetPlanReplanCommit(ctx, attempt.Request.ID)
			if commitErr != nil {
				return commitErr
			}
			if committed != nil {
				if planning.Validate(*committed, planning.Limits{}) != nil || committed.Digest != outcome.Proposed.Digest {
					return planning.ErrConflict
				}
				continue
			}
		}
		// Resolve from detached retained state. A nil control keeps this admission pending.
		request, _, freezeErr := attempt.Request.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		offered := attempt
		offered.Request = request
		offered.Fresh = false
		if outcome != nil {
			frozen, sealErr := SealPlanReplanOutcome(*outcome)
			if sealErr != nil {
				return sealErr
			}
			outcome = &frozen
		}
		control, resolveErr := c.Resolver.ResolvePlanReplanControl(ctx, offered, outcome)
		if resolveErr != nil {
			return resolveErr
		}
		if checkErr := ctx.Err(); checkErr != nil {
			return checkErr
		}
		if control == nil {
			return nil
		}
		frozen, _, freezeErr := control.Freeze()
		if freezeErr != nil {
			return freezeErr
		}
		_, digest, freezeErr := frozen.Request.Freeze()
		if freezeErr != nil || digest != attempt.RequestDigest {
			return planning.ErrConflict
		}
		if outcome == nil {
			if frozen.Operation != "observe" {
				return planning.ErrInvalid
			}
			_, err = controller.Observe(ctx, frozen)
			return err
		}
		if frozen.Operation != "commit" {
			return planning.ErrInvalid
		}
		_, err = controller.Commit(ctx, frozen)
		return err
	}
	if len(attempts) >= planning.MaxReplanAttempts {
		return nil
	}
	req, err := c.Resolver.ResolvePlanReplan(ctx, planID)
	if err != nil {
		return err
	}
	if checkErr := ctx.Err(); checkErr != nil {
		return checkErr
	}
	if req == nil {
		return nil
	}
	if req.Progress.Plan.ID != planID {
		return planning.ErrConflict
	}
	_, err = controller.Execute(ctx, *req)
	return err
}

// Start creates the service's independently owned lifetime.
func (s *PlanReplanService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop cancels work without claiming callback drainage.
func (s *PlanReplanService) RequestStop() { s.service.RequestStop() }

// Stop waits for discovery, authority, model and recovery callbacks to return.
func (s *PlanReplanService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status reports local counters without model or evidence content.
func (s *PlanReplanService) Status() TaskServiceStatus { return s.service.Status() }
