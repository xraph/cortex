package coordination

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

// Scheduler advances at most one currently ready task per tick. A nil return
// means the tick finished; only TaskAcceptanceStore can establish task success.
type Scheduler interface {
	Tick(context.Context, id.ID) error
}

var (
	ErrInvalidScheduler                = cortex.ErrInvalidScheduler
	ErrSchedulerFull                   = cortex.ErrSchedulerFull
	ErrSchedulerStopped                = cortex.ErrSchedulerStopped
	ErrSchedulerReconciliationRequired = cortex.ErrSchedulerReconciliationRequired
)

// SchedulingSource loads a retained plan and current authorized task states.
// Reports must come from trusted storage. Reconstruct Current from live grants
// and source revisions. Missing states mean pending, as in planning.Ready.
// This read does not replace the atomic checks in TaskClaimAdmitter.
type SchedulingSource interface {
	LoadSchedulingState(context.Context, id.ID) (planning.Plan, []planning.TaskState, error)
}

// TaskClaimResolver resolves current worker identity and an attenuated allocation
// for one ready task. Persist stable request identities before returning if they
// must survive process failure. Returned references are never credentials.
type TaskClaimResolver interface {
	ResolveTaskClaim(context.Context, planning.Plan, id.ID) (ReadyTaskClaimRequest, error)
}

// ClaimedTaskWorker runs one fresh ownership admission. Implementations must use
// guarded dispatch, accepted assignments and RunPreparedTask (or an equivalent
// fenced runtime). The receipt alone never authorizes a provider or tool call.
// Return when work suspends or enters asynchronous verification, preserving its
// durable state. A return releases local capacity, never shared reservations.
// Honor cancellation and perform no detached work after returning.
type ClaimedTaskWorker interface {
	RunClaimedTask(context.Context, ReadyTaskClaimRequest, ReadyTaskClaim) error
}

// SchedulerConfig requires explicit host services. Limits apply to this process;
// shared capacity and ownership remain enforced by durable guarded admission.
// TickTimeout bounds queueing, state resolution and execution together. A tick
// ends at its original claim expiry unless guarded Renewal is configured.
type SchedulerConfig struct {
	Renewal       *TaskRenewalConfig
	Source        SchedulingSource
	Resolver      TaskClaimResolver
	Admitter      TaskClaimAdmitter
	Worker        ClaimedTaskWorker
	Clock         verification.Clock
	MaxConcurrent int
	MaxPerScope   int
	MaxQueued     int
	TickTimeout   time.Duration
}

// SchedulerStatus exposes local capacity without scope or task content.
type SchedulerStatus struct {
	Active, Queued int
	Stopped        bool
}

// TaskScheduler uses FIFO order within an exact scope and round-robin admission
// across queued scopes. It starts no polling loop; renewed ticks supervise their
// own ownership. Call Tick from the host lifecycle and Shutdown before disposing
// its stores or worker services.
type TaskScheduler struct {
	config      SchedulerConfig
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	active      int
	activeScope map[string]int
	queued      int
	queues      map[string][]*schedulerTicket
	order       []string
	stopped     bool
	drained     chan struct{}
	drainOnce   sync.Once
}

// NewScheduler validates finite local bounds and all required host ports.
// Zero limits use 4 concurrent tasks, 1 per scope, 64 queued ticks and one minute.
func NewScheduler(config SchedulerConfig) (*TaskScheduler, error) {
	if hostport.IsNil(config.Source) || hostport.IsNil(config.Resolver) || hostport.IsNil(config.Admitter) || hostport.IsNil(config.Worker) || hostport.IsNil(config.Clock) {
		return nil, ErrInvalidScheduler
	}
	if config.Renewal != nil {
		policy := *config.Renewal
		if err := policy.validate(config.Worker); err != nil {
			return nil, err
		}
		config.Renewal = &policy
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = 4
	}
	if config.MaxPerScope == 0 {
		config.MaxPerScope = 1
	}
	if config.MaxQueued == 0 {
		config.MaxQueued = 64
	}
	if config.TickTimeout == 0 {
		config.TickTimeout = time.Minute
	}
	if config.MaxConcurrent < 1 || config.MaxConcurrent > 256 || config.MaxPerScope < 1 || config.MaxPerScope > config.MaxConcurrent || config.MaxQueued < 1 || config.MaxQueued > 4096 || config.TickTimeout <= 0 || config.TickTimeout > MaxClaimDuration {
		return nil, ErrInvalidScheduler
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &TaskScheduler{config: config, ctx: ctx, cancel: cancel, activeScope: make(map[string]int), queues: make(map[string][]*schedulerTicket), drained: make(chan struct{})}, nil
}

// Tick waits for local capacity, reloads readiness and resolves fresh authority,
// then attempts the first ready task in plan definition order. Every failure
// returns to the host; this method never retries an ambiguous admission or call.
func (s *TaskScheduler) Tick(ctx context.Context, planID id.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if planID.IsNil() || planID.Prefix() != planning.PrefixPlan {
		return ErrInvalidScheduler
	}
	// Preserve exact scope even if a caller reuses its original slice.
	scopeKey := scope.Canonical()
	ctx = cortex.WithScope(ctx, scope)
	ctx, cancel := context.WithTimeout(ctx, s.config.TickTimeout)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	ctx = schedulerContext{Context: ctx, stopped: s.ctx}
	ticket, err := s.acquire(ctx, scopeKey)
	if err != nil {
		return err
	}
	defer s.release(ticket)
	if s.ctx.Err() != nil {
		return ErrSchedulerStopped
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	p, states, err := s.config.Source.LoadSchedulingState(ctx, planID)
	if err != nil {
		return err
	}
	// Freeze host-owned slices before passing any data to another host callback.
	data, err := json.Marshal(struct {
		Plan   planning.Plan
		States []planning.TaskState
	}{p, states})
	if err != nil || len(data) > 8<<20 {
		return ErrInvalidScheduler
	}
	var snapshot struct {
		Plan   planning.Plan
		States []planning.TaskState
	}
	if json.Unmarshal(data, &snapshot) != nil {
		return ErrInvalidScheduler
	}
	p, states = snapshot.Plan, snapshot.States
	if p.ID != planID || p.Scope.Canonical() != scopeKey {
		return ErrInvalidScheduler
	}
	ready, err := planning.Ready(p, states, s.config.Clock.Now())
	if err != nil {
		return err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if len(ready) == 0 {
		return nil
	}
	taskID := ready[0]
	// Resolver input is separate from the snapshot used to validate its output.
	offered, err := planning.Seal(p, planning.Limits{})
	if err != nil {
		return err
	}
	req, err := s.config.Resolver.ResolveTaskClaim(ctx, offered, taskID)
	if err != nil {
		return err
	}
	req, _, err = req.Freeze()
	if err != nil {
		return err
	}
	claim := req.Acquisition.Claim
	if claim.PlanID != p.ID || claim.PlanRevision != p.Revision || claim.PlanDigest != p.Digest || claim.TaskID != taskID || req.Acquisition.Reservation.Scope.Canonical() != scopeKey {
		return ErrInvalidScheduler
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	admissionInput, _, err := req.Freeze()
	if err != nil {
		return err
	}
	receipt, err := s.config.Admitter.Admit(ctx, admissionInput)
	if err != nil || !receipt.Fresh || receipt.Validate(req) != nil {
		return ErrSchedulerReconciliationRequired
	}
	if err = ctx.Err(); err != nil {
		return errors.Join(ErrSchedulerReconciliationRequired, err)
	}
	// A malicious or buggy host port cannot widen the snapshot's time bounds.
	deadline := p.Budget.Deadline
	if s.config.Renewal == nil {
		deadline = minTime(deadline, receipt.Acquisition.Claim.ExpiresAt)
	}
	for _, task := range p.Tasks {
		if task.ID == taskID {
			deadline = minTime(deadline, task.Budget.Deadline)
			break
		}
	}
	remaining := deadline.Sub(s.config.Clock.Now())
	if remaining <= 0 {
		return ErrSchedulerReconciliationRequired
	}
	workerCtx, workerCancel := context.WithTimeout(ctx, remaining)
	defer workerCancel()
	workerCtx = schedulerContext{Context: workerCtx, stopped: s.ctx}
	// Detach the validated receipt, retaining its nonserialized fresh marker.
	data, err = json.Marshal(receipt)
	if err != nil {
		return ErrSchedulerReconciliationRequired
	}
	var detached ReadyTaskClaim
	if json.Unmarshal(data, &detached) != nil {
		return ErrSchedulerReconciliationRequired
	}
	detached.Fresh = true
	if err = workerCtx.Err(); err != nil {
		return errors.Join(ErrSchedulerReconciliationRequired, err)
	}
	if s.config.Renewal != nil {
		err = s.runRenewing(workerCtx, req, detached, deadline)
	} else {
		err = s.config.Worker.RunClaimedTask(workerCtx, req, detached)
	}
	return errors.Join(err, workerCtx.Err())
}

// Status returns current local queue and worker counts.
func (s *TaskScheduler) Status() SchedulerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SchedulerStatus{Active: s.active, Queued: s.queued, Stopped: s.stopped}
}

// Shutdown rejects new ticks, cancels queued and owned work, then waits until
// callbacks return or ctx expires. A timeout leaves the scheduler stopped; call
// again to wait for drainage. No budget is refunded and no durable claim is
// released on an uncertain worker outcome. Processes recover those records.
func (s *TaskScheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		s.cancel()
		for _, queue := range s.queues {
			for _, ticket := range queue {
				ticket.err = ErrSchedulerStopped
				close(ticket.ready)
			}
		}
		s.queues = make(map[string][]*schedulerTicket)
		s.order = nil
		s.queued = 0
	}
	s.finishDrain()
	s.mu.Unlock()
	select {
	case <-s.drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Err observes shutdown synchronously even before AfterFunc closes Done.
// The callback remains necessary to wake blocking host operations.
type schedulerContext struct {
	context.Context
	stopped context.Context
}

func (c schedulerContext) Err() error {
	if err := c.Context.Err(); err != nil {
		return err
	}
	return c.stopped.Err()
}
