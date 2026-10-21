// Package service manages the host service boundary for sandbox recovery.
package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
)

type manager interface {
	Mask(context.Context, string) error
	Reload(context.Context) error
	Stop(context.Context, string) error
	Masked(context.Context, string) (bool, error)
	Stopped(context.Context, string) (bool, error)
}

// Reaper permanently fences job service identities and stops their whole tree.
// It never starts or unmasks a service, deletes state, or grants a new claim.
// The host must reserve these identities and prevent other managers from
// removing masks or migrating payloads out of their fixed service hierarchy.
type Reaper struct {
	manager  manager
	check    func(context.Context, id.ID) error
	kill     func(context.Context, id.ID) (containment.Observation, error)
	close    func() error
	once     sync.Once
	closeErr error
}

// Open connects directly to the root systemd manager on Linux. ctx bounds
// connection establishment; Close releases the connection after use.
func Open(ctx context.Context) (*Reaper, error) {
	connection, err := openBus(ctx)
	if err != nil {
		return nil, err
	}
	return &Reaper{manager: &systemManager{bus: connection}, check: containment.VerifyReaper, kill: containment.KillAndWait, close: connection.Close}, nil
}

// Close releases the connection. In-flight operations fail without cleanup proof.
func (r *Reaper) Close() error {
	r.once.Do(func() {
		if r.close != nil {
			r.closeErr = r.close()
		}
	})
	return r.closeErr
}

// FenceAndStop masks the exact instance persistently, reloads the manager,
// replaces queued starts with a stop job, and kills every cgroup descendant.
// Success additionally requires no queued job and an inactive or absent service.
// Use an independent recovery context; the total operation is capped at 30s.
// A failed attempt retains its mask and can be retried. This is a trusted host
// operation, not an authorization endpoint or a sandbox qualification claim.
func (r *Reaper) FenceAndStop(ctx context.Context, key id.ID) error {
	layout, err := containment.ForJob(key)
	if err != nil {
		return err
	}
	if r.manager == nil || r.check == nil || r.kill == nil {
		return fmt.Errorf("cortex: incomplete service reaper")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if operationErr := ctx.Err(); operationErr != nil {
		return operationErr
	}
	if operationErr := r.check(ctx, key); operationErr != nil {
		return operationErr
	}
	if operationErr := r.manager.Mask(ctx, layout.Unit); operationErr != nil {
		return operationErr
	}
	if operationErr := r.manager.Reload(ctx); operationErr != nil {
		return operationErr
	}
	if operationErr := r.masked(ctx, layout.Unit); operationErr != nil {
		return operationErr
	}
	if operationErr := r.manager.Stop(ctx, layout.Unit); operationErr != nil {
		return operationErr
	}
	if operationErr := r.drain(ctx, key); operationErr != nil {
		return operationErr
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if operationErr := ctx.Err(); operationErr != nil {
			return operationErr
		}
		stopped, checkErr := r.manager.Stopped(ctx, layout.Unit)
		if checkErr != nil {
			return checkErr
		}
		if stopped {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	// Stop helpers may have run after the first drain. Quiescence prevents new
	// manager-launched processes; drain their descendants before publishing proof.
	if operationErr := r.drain(ctx, key); operationErr != nil {
		return operationErr
	}
	if operationErr := r.masked(ctx, layout.Unit); operationErr != nil {
		return operationErr
	}
	stopped, err := r.manager.Stopped(ctx, layout.Unit)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("cortex: service changed after containment cleanup")
	}
	return ctx.Err()
}

func (r *Reaper) drain(ctx context.Context, key id.ID) error {
	observation, err := r.kill(ctx, key)
	if err != nil {
		return err
	}
	if observation.Empty == observation.Absent {
		return fmt.Errorf("cortex: missing or ambiguous containment observation")
	}
	return ctx.Err()
}

func (r *Reaper) masked(ctx context.Context, unit string) error {
	masked, err := r.manager.Masked(ctx, unit)
	if err != nil {
		return err
	}
	if !masked {
		return fmt.Errorf("cortex: persistent job service mask missing")
	}
	return ctx.Err()
}

var errUnsupported = errors.New("cortex: systemd reaping requires a root-run Linux host")
