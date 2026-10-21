//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// ControllerStatus is a detached lifecycle snapshot. Err reports a recovery or
// shutdown error to the trusted daemon; it is not a public job-result endpoint.
type ControllerStatus struct {
	Running, Ready bool
	Err            error
}

// Controller owns launch admission and recovery for one daemon lifetime. Use
// Launch through this controller exclusively after constructing it. The host
// must not call its underlying launcher or scanner concurrently from elsewhere.
// It does not expose a control transport or runtime adapter, or qualify Linux.
type Controller struct {
	launcher    *Launcher
	scanner     *RecoveryScanner
	interval    time.Duration
	mu          sync.Mutex
	started     bool
	status      ControllerStatus
	lifetime    context.Context
	ops         sync.WaitGroup
	checkWorker func(context.Context, LaunchProfile, id.ID, uint32) error
}

// NewController joins a launcher and scanner that share the exact journal and
// reaper. The interval is between 10ms and one second; slow cleanup can still
// delay a scan, so independent worker and service deadline limits are required.
func NewController(launcher *Launcher, scanner *RecoveryScanner, interval time.Duration) (*Controller, error) {
	if launcher == nil || scanner == nil || launcher.store == nil || launcher.reaper == nil || launcher.store != scanner.store || launcher.reaper != scanner.reaper || scanner.revalidate == nil || scanner.now == nil || scanner.pageSize < 1 || scanner.pageSize > 256 || interval < 10*time.Millisecond || interval > time.Second {
		return nil, fmt.Errorf("cortex: invalid sandbox controller")
	}
	return &Controller{launcher: launcher, scanner: scanner, interval: interval, checkWorker: launcher.reaper.CheckWorker}, nil
}

// Status reports readiness without granting admission; Launch checks it again.
func (c *Controller) Status() ControllerStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

// Run recovers every old acceptance before admitting launches, then scans on a
// bounded timer. Any scan error closes admission and stops the controller. A
// stopped controller cannot be restarted; resolve its error and create another.
// Cancellation closes admission, cancels and joins in-flight operations, then
// recovers outstanding claims with an independent 30-second cleanup context.
// Host callbacks must honor their contexts for shutdown to remain bounded.
func (c *Controller) Run(ctx context.Context) (err error) {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return workerstore.ErrConflict
	}
	c.started = true
	lifetime, cancel := context.WithCancel(ctx)
	c.lifetime = lifetime
	c.status.Running = true
	c.mu.Unlock()
	served := false
	defer func() {
		c.mu.Lock()
		c.status.Ready = false
		c.status.Err = err
		cancel()
		c.mu.Unlock()
		// Admission and Add share the same lock. Once it closes, no new Add
		// can race this Wait, and startup recovery cannot overlap a late Start.
		c.ops.Wait()
		if served {
			cleanup, finish := context.WithTimeout(context.Background(), 30*time.Second)
			_, cleanupErr := c.scanner.Recover(cleanup)
			finish()
			err = errors.Join(err, cleanupErr)
		}
		c.mu.Lock()
		c.status.Running = false
		c.status.Err = err
		c.mu.Unlock()
	}()
	if _, err = c.scanner.Recover(c.lifetime); err != nil {
		return err
	}
	c.mu.Lock()
	err = c.lifetime.Err()
	if err == nil {
		c.status.Ready = true
		served = true
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	cursor := ""
	for {
		select {
		case <-c.lifetime.Done():
			return c.lifetime.Err()
		case <-ticker.C:
			page, scanErr := c.scanner.Sweep(c.lifetime, cursor)
			if scanErr != nil {
				return scanErr
			}
			cursor = page.Next
			if page.Done {
				cursor = ""
			}
		}
	}
}

// Launch requires a recovered, running controller and binds the submission to
// both its caller and daemon lifetime. A stopped or unready controller returns
// ErrPending without enrolling or starting the receipt.
func (c *Controller) Launch(ctx context.Context, receipt sandbox.Receipt) error {
	operation, finish, err := c.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return c.launcher.Launch(operation, receipt)
}

func (c *Controller) begin(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	if !c.status.Ready || c.lifetime.Err() != nil {
		c.mu.Unlock()
		return nil, nil, broker.ErrPending
	}
	c.ops.Add(1)
	lifetime := c.lifetime
	c.mu.Unlock()
	operation, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(lifetime, cancel)
	if lifetime.Err() != nil {
		cancel()
	}
	return operation, func() { stop(); cancel(); c.ops.Done() }, nil
}
