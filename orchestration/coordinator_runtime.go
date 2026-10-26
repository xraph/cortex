package orchestration

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

const coordinatorDuration = 30 * time.Second

type coordinatorContextKey struct{}

// coordinatorPass holds only acknowledged ownership. Retained reads never update
// this receipt. Its independent watchdog still fires if a renewal call stalls.
type coordinatorPass struct {
	store     CoordinatorStore
	mu        sync.Mutex
	lease     CoordinatorLease
	lost      error
	cancel    context.CancelCauseFunc
	timer     *time.Timer
	stopRenew context.CancelFunc
	done      chan struct{}
}

func startCoordinatorPass(ctx context.Context, storage CoordinatorStore, lease CoordinatorLease) (context.Context, *coordinatorPass) {
	ctx, cancel := context.WithCancelCause(ctx)
	renewCtx, stop := context.WithCancel(ctx)
	p := &coordinatorPass{store: storage, lease: lease, cancel: cancel, stopRenew: stop, done: make(chan struct{})}
	p.timer = time.AfterFunc(time.Until(lease.ExpiresAt), p.expire)
	go p.renew(renewCtx)
	return context.WithValue(ctx, coordinatorContextKey{}, p), p
}

func coordinatorFromContext(ctx context.Context) *coordinatorPass {
	p, ok := ctx.Value(coordinatorContextKey{}).(*coordinatorPass)
	if !ok {
		return nil
	}
	return p
}

func (p *coordinatorPass) expire() {
	p.mu.Lock()
	if time.Now().Before(p.lease.ExpiresAt) {
		p.mu.Unlock()
		return
	}
	p.lost = errors.Join(p.lost, cortex.ErrOrchestrationRunConflict)
	p.mu.Unlock()
	p.cancel(cortex.ErrOrchestrationRunConflict)
}

func (p *coordinatorPass) fail(err error) {
	p.mu.Lock()
	p.lost = errors.Join(p.lost, err)
	p.mu.Unlock()
	p.cancel(err)
}

func (p *coordinatorPass) receipt() (CoordinatorLease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lost != nil {
		return CoordinatorLease{}, p.lost
	}
	if !time.Now().Before(p.lease.ExpiresAt) {
		return CoordinatorLease{}, cortex.ErrOrchestrationRunConflict
	}
	return p.lease, nil
}

func (p *coordinatorPass) renew(ctx context.Context) {
	defer close(p.done)
	for {
		lease, err := p.receipt()
		if err != nil {
			p.fail(err)
			return
		}
		delay := time.Until(lease.ExpiresAt) / 3
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		attempt, cancel := context.WithDeadline(ctx, lease.ExpiresAt)
		next, renewErr := p.store.RenewOrchestrationCoordinator(attempt, lease, coordinatorDuration)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if renewErr != nil {
			p.fail(renewErr)
			return
		}
		frozen, _, freezeErr := next.Freeze()
		if freezeErr != nil {
			p.fail(freezeErr)
			return
		}
		next = frozen
		_, priorDigest, priorErr := lease.Freeze()
		if priorErr != nil {
			p.fail(priorErr)
			return
		}
		// A delayed acknowledgment cannot resurrect a canceled pass.
		p.mu.Lock()
		if p.lost != nil || !time.Now().Before(lease.ExpiresAt) || !time.Now().Before(next.ExpiresAt) || next.Owner != lease.Owner || next.Generation != lease.Generation || next.TeamID != lease.TeamID || next.Scope.Canonical() != lease.Scope.Canonical() || next.Revision != lease.Revision+1 || next.PreviousDigest != priorDigest || next.ExpiresAt.Before(lease.ExpiresAt) || !next.AcquiredAt.Equal(lease.AcquiredAt) || next.Released {
			p.mu.Unlock()
			p.fail(cortex.ErrOrchestrationRunConflict)
			return
		}
		p.lease = next
		p.timer.Reset(time.Until(next.ExpiresAt))
		p.mu.Unlock()
		if next.ExpiresAt.Equal(lease.ExpiresAt) {
			// The immutable team deadline leaves no further extension.
			<-ctx.Done()
			return
		}
	}
}

func (p *coordinatorPass) stop() {
	p.stopRenew()
	<-p.done
	p.timer.Stop()
	p.cancel(context.Canceled)
}

// CheckCoordinator checks current acknowledged ownership when a service pass is
// attached. Independent child resumes retain their existing authorization path.
func CheckCoordinator(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, context.Cause(ctx))
	}
	p := coordinatorFromContext(ctx)
	if p == nil {
		return nil
	}
	lease, err := p.receipt()
	if err != nil {
		return err
	}
	_, err = p.store.CheckOrchestrationCoordinator(ctx, lease)
	if err != nil {
		p.fail(err)
	}
	return err
}

// ReserveCoordinatorChild charges and links a child with acknowledged ownership.
// The caller still checks ownership before activation and each external call.
func ReserveCoordinatorChild(ctx context.Context, req execution.ReservationRequest) (execution.Reservation, error) {
	p := coordinatorFromContext(ctx)
	if p == nil {
		return execution.Reservation{}, cortex.ErrOrchestrationContinuationUnavailable
	}
	lease, err := p.receipt()
	if err != nil {
		return execution.Reservation{}, err
	}
	return p.store.ReserveCoordinatedChild(ctx, lease, req)
}

func (p *coordinatorPass) begin(ctx context.Context, intent InvocationIntent) error {
	lease, err := p.receipt()
	if err != nil {
		return err
	}
	return p.store.BeginCoordinatedInvocation(ctx, lease, intent)
}

func (p *coordinatorPass) finish(ctx context.Context, rec *Run) error {
	lease, err := p.receipt()
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return p.store.FinishOrchestrationCoordinator(writeCtx, lease, rec)
}

// NewCoordinatedService adds expiring ownership to durable strategy replay.
// Its reserved runner must use ReserveCoordinatorChild for atomic admission.
func NewCoordinatedService(runner ReservedAgentRunner, observer ChildObserver, runs RunStore, hooks HookEmitter) *Service {
	s := NewContinuableService(runner, observer, runs, hooks)
	s.coordinated = true
	return s
}

func (s *Service) acquireCoordinator(ctx context.Context, rec *Run, initial bool) (context.Context, *coordinatorPass, *Run, error) {
	storage, ok := s.runs.(CoordinatorStore)
	if !ok || rec == nil {
		return ctx, nil, nil, cortex.ErrOrchestrationContinuationUnavailable
	}
	request := CoordinatorRequest{TeamID: rec.ID, Owner: id.New(PrefixCoordinator), ExpectedGeneration: rec.ContinuationGeneration, Duration: coordinatorDuration, Initial: initial}
	claim, err := storage.AcquireOrchestrationCoordinator(ctx, request)
	if err != nil {
		return ctx, nil, nil, err
	}
	lease, _, leaseErr := claim.Lease.Freeze()
	if leaseErr != nil || lease.Owner != request.Owner || lease.Generation != request.ExpectedGeneration+1 || claim.Run == nil || lease.TeamID != rec.ID || claim.Run.ID != rec.ID || claim.Run.ContinuationGeneration != lease.Generation || claim.Run.Scope.Canonical() != lease.Scope.Canonical() || lease.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() || lease.Released {
		return ctx, nil, nil, cortex.ErrInvalidContinuation
	}
	claim.Lease = lease

	ctx, pass := startCoordinatorPass(ctx, storage, claim.Lease)
	return ctx, pass, claim.Run, nil
}

func (s *Service) executeCoordinated(ctx context.Context, cfg Config, input string, rec *Run, replay bool) (out *Run, err error) {
	pass := coordinatorFromContext(ctx)
	defer func() {
		if recovered := recover(); recovered != nil {
			rec.Status = StatusFailed
			rec.Output = ""
			rec.Error = "coordinator execution panicked"
			if finishErr := pass.finish(ctx, rec); finishErr != nil {
				rec.Error = "coordinator panic cleanup uncertain"
			}
			panic(recovered)
		}
	}()
	if !replay {
		s.hooks.OrchestrationStarted(ctx, rec.ID, cfg.Strategy)
	}
	return s.executeConfig(ctx, cfg, input, rec, replay)
}

// CoordinatorWork reconciles host-owned child work while the coordinator lease
// is active. Waiting keeps the parent resumable without restarting a child.
type CoordinatorWork interface{ ReconcileOrchestration(context.Context) error }

// RequireCoordinator requires acknowledged ownership of this exact team.
func RequireCoordinator(ctx context.Context, team id.OrchestrationID) error {
	pass := coordinatorFromContext(ctx)
	if pass == nil {
		return cortex.ErrOrchestrationContinuationUnavailable
	}
	lease, err := pass.receipt()
	if err != nil {
		return err
	}
	if lease.TeamID != team {
		return cortex.ErrInvalidContinuation
	}
	return CheckCoordinator(ctx)
}
