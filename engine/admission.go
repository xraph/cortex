package engine

import (
	"context"
	"fmt"
	"sync"

	"github.com/xraph/cortex"
)

// admission decides whether a run may start executing now. It caps how
// many runs execute at once (WithRunConcurrency), refuses new runs while
// Stop is draining, and counts the runs in flight so Stop knows when the
// last one has finished.
//
// The zero value admits everything and counts nothing until a run starts,
// so an Engine built without New still works.
type admission struct {
	// slots is the semaphore, nil when the limit is zero (unlimited).
	// It is sized once in New and never replaced.
	slots chan struct{}
	limit int

	mu       sync.Mutex
	stopping bool
	// stopped is closed when stopping starts, so runs waiting for a slot
	// give up at once instead of after a slot frees.
	stopped chan struct{}
	active  int
	// idle is closed when active drops to zero. A fresh one is made each
	// time active leaves zero.
	idle chan struct{}
}

// admissionTicket is what an admitted run carries on its context. A run
// started from inside it (a tool calling RunAgent on the ctx it was
// given, say) finds the ticket and runs under the parent's slot instead
// of waiting for a second one, which with a limit of one would never
// come.
type admissionTicket struct {
	adm      *admission
	holdSlot bool
	once     sync.Once
	// released is guarded by adm.mu, so a child cannot join a parent in
	// the moment between the parent leaving and drain seeing idle.
	released bool
	// hooks run when the ticket is released, before its slot is given
	// back: the run's lease ends with the call that ran it. Guarded by
	// adm.mu.
	hooks []func()
}

type admissionTicketKey struct{}

func (a *admission) configure(limit int) {
	a.limit = limit
	if limit > 0 {
		a.slots = make(chan struct{}, limit)
	}
}

// admit lets a run start, or says why it cannot. The returned context
// carries the run's ticket; release gives the slot back and is safe to
// call more than once.
//
// A run whose context already carries a live ticket from this engine is
// nested inside an admitted run. It takes no slot and is let through
// even while stopping: its parent is one of the runs Stop is waiting for,
// and refusing the child would fail the parent's tool call halfway
// through the drain. It is still counted, so Stop waits for it too.
func (a *admission) admit(ctx context.Context) (context.Context, func(), error) {
	a.mu.Lock()
	if parent, ok := ctx.Value(admissionTicketKey{}).(*admissionTicket); ok && parent.adm == a && !parent.released {
		a.enterLocked()
		a.mu.Unlock()
		return a.issue(ctx, false)
	}
	if a.stopping {
		a.mu.Unlock()
		return ctx, func() {}, cortex.ErrEngineStopping
	}
	stopped := a.stoppedLocked()
	a.mu.Unlock()

	if a.slots != nil {
		select {
		case a.slots <- struct{}{}:
		case <-stopped:
			return ctx, func() {}, cortex.ErrEngineStopping
		case <-ctx.Done():
			return ctx, func() {}, fmt.Errorf("cortex: wait for a run slot (run_concurrency %d): %w", a.limit, ctx.Err())
		}
	}

	// Checked again under the lock that Stop takes, so a run is either
	// counted before Stop looks or refused after it started stopping.
	a.mu.Lock()
	if a.stopping {
		a.mu.Unlock()
		if a.slots != nil {
			<-a.slots
		}
		return ctx, func() {}, cortex.ErrEngineStopping
	}
	a.enterLocked()
	a.mu.Unlock()
	return a.issue(ctx, true)
}

func (a *admission) issue(ctx context.Context, holdSlot bool) (context.Context, func(), error) {
	t := &admissionTicket{adm: a, holdSlot: holdSlot}
	return context.WithValue(ctx, admissionTicketKey{}, t), t.release, nil
}

func (t *admissionTicket) release() {
	t.once.Do(func() {
		t.adm.mu.Lock()
		hooks := t.hooks
		t.hooks = nil
		t.adm.mu.Unlock()
		for _, hook := range hooks {
			hook()
		}
		if t.holdSlot && t.adm.slots != nil {
			<-t.adm.slots
		}
		t.adm.mu.Lock()
		t.released = true
		t.adm.leaveLocked()
		t.adm.mu.Unlock()
	})
}

// onRelease runs hook when the ticket is released, or now if it already
// was.
func (t *admissionTicket) onRelease(hook func()) {
	t.adm.mu.Lock()
	if !t.released {
		t.hooks = append(t.hooks, hook)
		t.adm.mu.Unlock()
		return
	}
	t.adm.mu.Unlock()
	hook()
}

func (a *admission) enterLocked() {
	if a.active == 0 {
		a.idle = make(chan struct{})
	}
	a.active++
}

func (a *admission) leaveLocked() {
	a.active--
	if a.active == 0 {
		close(a.idle)
	}
}

func (a *admission) stoppedLocked() chan struct{} {
	if a.stopped == nil {
		a.stopped = make(chan struct{})
	}
	return a.stopped
}

// drain refuses new runs from now on and waits for the ones in flight to
// finish, or for ctx to end. Runs still executing at the deadline are left
// running: they write their own outcome when they finish.
func (a *admission) drain(ctx context.Context) error {
	a.mu.Lock()
	if !a.stopping {
		a.stopping = true
		close(a.stoppedLocked())
	}
	if a.active == 0 {
		a.mu.Unlock()
		return nil
	}
	idle := a.idle
	a.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("cortex: wait for in-flight runs: %w", ctx.Err())
	}
}

// draining reports whether Stop has started refusing runs.
func (a *admission) draining() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stopping
}

// reopen admits runs again after a Stop.
func (a *admission) reopen() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		a.stopping = false
		a.stopped = nil
	}
}
