package coordination

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
)

// ScheduledPlan is a host-discovered identity, never an execution grant.
type ScheduledPlan struct {
	Scope  cortex.Scope `json:"scope"`
	PlanID id.ID        `json:"plan_id"`
}

// TaskPlanListRequest bounds discovery. After is an opaque host cursor. A source
// must eventually finish a pass so new work before an earlier cursor is revisited.
type TaskPlanListRequest struct {
	After string
	Limit int
}

// TaskPlanPage returns at most the requested limit. Empty Next ends the pass.
// A new pass starts on a later poll, including after a process restart.
type TaskPlanPage struct {
	Plans []ScheduledPlan `json:"plans"`
	Next  string          `json:"next,omitempty"`
}

// TaskPlanSource discovers plans under the host service identity. The source
// authorizes discovery across scopes; ordinary store calls still require scope.
// Every resulting Tick repeats its own current admission checks. Honor context
// cancellation and avoid provider/tool execution in this port.
type TaskPlanSource interface {
	ListScheduledPlans(context.Context, TaskPlanListRequest) (TaskPlanPage, error)
}

// TaskServiceConfig connects bounded discovery to the guarded scheduler. Limits
// cover this process only. ReportError is optional and must return promptly; the
// engine reports error types without logging their potentially private text.
type TaskServiceConfig struct {
	Scheduler        SchedulerConfig
	Discovery        TaskPlanSource
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	MaxInFlight      int
	ReportError      func(error)
}

// TaskServiceStatus contains process counters, without plan or scope content.
// Active includes queued and executing ticks. Stopping remains true after a
// timed-out Stop until every callback returns.
type TaskServiceStatus struct {
	Running, Stopping bool
	Active            int
	Failures          uint64
}

// TaskService polls one bounded page per interval and owns all resulting ticks.
// It never retries an uncertain invocation, accepts outputs or refunds capacity.
// Stop it before disposing stores. You may restart after complete drainage.
type TaskService struct {
	config  TaskServiceConfig
	mu      sync.Mutex
	current *taskServiceRun
}

type taskServiceRun struct {
	scheduler *TaskScheduler
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	stopping  bool // Protected by TaskService.mu.
	active    atomic.Int64
	failures  atomic.Uint64
}

// NewTaskService validates ports and finite bounds without starting work. Defaults
// are one-second polling, a five-second discovery timeout and 64 in-flight ticks.
func NewTaskService(config TaskServiceConfig) (*TaskService, error) {
	if config.Interval == 0 {
		config.Interval = time.Second
	}
	if config.DiscoveryTimeout == 0 {
		config.DiscoveryTimeout = 5 * time.Second
	}
	if config.MaxInFlight == 0 {
		config.MaxInFlight = 64
	}
	if hostport.IsNil(config.Discovery) || config.Interval < time.Millisecond || config.Interval > time.Hour || config.DiscoveryTimeout < time.Millisecond || config.DiscoveryTimeout > time.Minute || config.MaxInFlight < 1 || config.MaxInFlight > 4096 {
		return nil, ErrInvalidScheduler
	}
	validated, err := NewScheduler(config.Scheduler)
	if err != nil {
		return nil, err
	}
	config.Scheduler = validated.config
	if shutdownErr := validated.Shutdown(context.Background()); shutdownErr != nil {
		return nil, shutdownErr
	}
	return &TaskService{config: config}, nil
}

// Start preserves startup context values for the host service identity while
// giving the service its own lifetime. It is idempotent while running. An earlier
// timed-out Stop must finish before a new scheduler can start.
func (s *TaskService) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current := s.current; current != nil {
		select {
		case <-current.done:
		default:
			if current.stopping {
				return ErrSchedulerStopped
			}
			return nil
		}
	}
	scheduler, err := NewScheduler(s.config.Scheduler)
	if err != nil {
		return err
	}
	lifetime, cancel := context.WithCancel(context.WithoutCancel(ctx))
	current := &taskServiceRun{scheduler: scheduler, ctx: lifetime, cancel: cancel, done: make(chan struct{})}
	s.current = current
	go s.poll(current)
	return nil
}

// Stop cancels discovery and all owned ticks, then waits for complete drainage.
// A timeout leaves the service stopping, with stores and reservations retained.
// Call Stop again to wait; the caller deadline is not permission to detach work.
func (s *TaskService) Stop(ctx context.Context) error {
	s.mu.Lock()
	current := s.current
	if current != nil {
		current.stopping = true
		current.cancel()
	}
	s.mu.Unlock()
	if current == nil {
		return nil
	}
	select {
	case <-current.done:
		return nil
	default:
	}
	select {
	case <-current.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status reports the current lifecycle incarnation's counters.
func (s *TaskService) Status() TaskServiceStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.current
	if current == nil {
		return TaskServiceStatus{}
	}
	status := TaskServiceStatus{Running: !current.stopping, Stopping: current.stopping, Active: int(current.active.Load()), Failures: current.failures.Load()}
	select {
	case <-current.done:
		status.Running, status.Stopping = false, false
	default:
	}
	return status
}

func (s *TaskService) recordFailure(current *taskServiceRun, err error) {
	if err == nil || current.ctx.Err() != nil {
		return
	}
	current.failures.Add(1)
	if s.config.ReportError != nil {
		// A reporting hook cannot bypass drainage by panicking.
		defer func() {
			if recover() != nil {
				current.failures.Add(1)
			}
		}()
		s.config.ReportError(err)
	}
}

func (s *TaskService) poll(current *taskServiceRun) {
	var workers sync.WaitGroup
	defer close(current.done)
	defer workers.Wait()
	defer func() {
		current.cancel()
		s.recordFailure(current, current.scheduler.Shutdown(context.Background()))
	}()
	ticker := time.NewTicker(s.config.Interval)
	defer ticker.Stop()
	completed := make(chan string, s.config.MaxInFlight)
	active := make(map[string]string)
	activeScopes := make(map[string]int)
	cursor := ""
	for {
		if current.ctx.Err() != nil {
			return
		}
		if available := s.config.MaxInFlight - len(active); available > 0 {
			page, err := s.discover(current.ctx, TaskPlanListRequest{After: cursor, Limit: min(available, 256)})
			if err != nil {
				s.recordFailure(current, err)
			} else {
				cursor = page.Next
				for _, plan := range page.Plans {
					if current.ctx.Err() != nil {
						return
					}
					scopeKey := plan.Scope.Canonical()
					key := scopeKey + "/" + plan.PlanID.String()
					if _, exists := active[key]; exists {
						continue
					}
					// Do not fill discovery capacity with ticks queued behind one scope.
					// Skipped identities remain eligible on the next complete source pass.
					if activeScopes[scopeKey] >= s.config.Scheduler.MaxPerScope {
						continue
					}
					active[key] = scopeKey
					activeScopes[scopeKey]++
					workers.Add(1)
					current.active.Add(1)
					go func() {
						defer workers.Done()
						defer func() { current.active.Add(-1); completed <- key }()
						defer func() {
							if recover() != nil {
								s.recordFailure(current, ErrSchedulerReconciliationRequired)
							}
						}()
						tickCtx := cortex.WithScope(current.ctx, plan.Scope)
						s.recordFailure(current, current.scheduler.Tick(tickCtx, plan.PlanID))
					}()
				}
			}
		}
	wait:
		for {
			select {
			case <-current.ctx.Done():
				return
			case key := <-completed:
				if scopeKey, exists := active[key]; exists {
					activeScopes[scopeKey]--
					if activeScopes[scopeKey] == 0 {
						delete(activeScopes, scopeKey)
					}
					delete(active, key)
				}
			case <-ticker.C:
				break wait
			}
		}
	}
}

func (s *TaskService) discover(ctx context.Context, request TaskPlanListRequest) (page TaskPlanPage, err error) {
	defer func() {
		if recover() != nil {
			page = TaskPlanPage{}
			err = ErrInvalidScheduler
		}
	}()
	readCtx, cancel := context.WithTimeout(ctx, s.config.DiscoveryTimeout)
	defer cancel()
	page, err = s.config.Discovery.ListScheduledPlans(readCtx, request)
	if err != nil {
		return TaskPlanPage{}, err
	}
	if contextErr := readCtx.Err(); contextErr != nil {
		return TaskPlanPage{}, contextErr
	}
	if len(page.Plans) > request.Limit || len(page.Next) > 4096 || !utf8.ValidString(page.Next) || (page.Next != "" && page.Next == request.After) {
		return TaskPlanPage{}, ErrInvalidScheduler
	}
	// Bound identity bytes before JSON can allocate or repair invalid UTF-8.
	remaining := (2 << 20) - len(page.Next)
	seen := make(map[string]struct{}, len(page.Plans))
	for _, plan := range page.Plans {
		if len(plan.Scope.Levels) > remaining/32 {
			return TaskPlanPage{}, ErrInvalidScheduler
		}
		remaining -= len(plan.Scope.Levels) * 32
		for _, level := range plan.Scope.Levels {
			for _, value := range []string{level.Key, level.Value} {
				if len(value) > remaining {
					return TaskPlanPage{}, ErrInvalidScheduler
				}
				remaining -= len(value)
			}
		}
		if cortex.ValidateScope(plan.Scope) != nil || plan.PlanID.IsNil() || plan.PlanID.Prefix() != planning.PrefixPlan {
			return TaskPlanPage{}, ErrInvalidScheduler
		}
		key := plan.Scope.Canonical() + "/" + plan.PlanID.String()
		if _, exists := seen[key]; exists {
			return TaskPlanPage{}, ErrInvalidScheduler
		}
		seen[key] = struct{}{}
	}
	data, err := json.Marshal(page)
	if err != nil || len(data) > 2<<20 {
		return TaskPlanPage{}, ErrInvalidScheduler
	}
	var frozen TaskPlanPage
	if json.Unmarshal(data, &frozen) != nil {
		return TaskPlanPage{}, ErrInvalidScheduler
	}
	return frozen, nil
}
