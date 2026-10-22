//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// Launcher consumes durable ownership before publishing a credential and
// issuing one StartUnit call. A retry never reconstructs a lost claim token.
// The owning daemon must recover outstanding claims on startup and monitor
// accepted deadlines independently of request contexts.
type Launcher struct {
	store     *workerstore.Store
	reaper    *Reaper
	profile   LaunchProfile
	authorize func(context.Context, sandbox.Job) error
	now       func() time.Time
	preflight func(context.Context, LaunchProfile, id.ID) error
	prepare   func(context.Context, LaunchProfile, workerstore.Claim) error
	start     func(context.Context, id.ID) error
}

// NewLauncher requires current host authorization. The callback must bind the
// authenticated host caller to the exact job scope/principal and current grants.
// This internal component does not expose an unauthenticated submission API.
func NewLauncher(store *workerstore.Store, reaper *Reaper, profile LaunchProfile, authorize func(context.Context, sandbox.Job) error) (*Launcher, error) {
	if runtime.GOOS != "linux" {
		return nil, errUnsupported
	}
	if store == nil || reaper == nil || authorize == nil {
		return nil, fmt.Errorf("cortex: incomplete worker launcher")
	}
	if err := profile.validate(); err != nil {
		return nil, err
	}
	starter, ok := reaper.manager.(interface {
		Start(context.Context, string) error
	})
	if !ok {
		return nil, fmt.Errorf("cortex: service manager cannot launch workers")
	}
	return &Launcher{store: store, reaper: reaper, profile: profile, authorize: authorize, now: time.Now, preflight: reaper.CheckLaunch, prepare: func(ctx context.Context, p LaunchProfile, claim workerstore.Claim) error {
		return writeCredential(ctx, p, claim, 0)
	}, start: func(ctx context.Context, key id.ID) error {
		layout, err := containment.ForJob(key)
		if err != nil {
			return err
		}
		return starter.Start(ctx, layout.Unit)
	}}, nil
}

// Launch acknowledges an already claimed or completed receipt without another
// start attempt. Reaping work remains pending. Failures after claim acquisition
// trigger independent bounded recovery and preserve both errors when it fails.
func (l *Launcher) Launch(ctx context.Context, receipt sandbox.Receipt) error {
	if l.store == nil || l.reaper == nil || l.now == nil || l.authorize == nil || l.preflight == nil || l.prepare == nil || l.start == nil {
		return fmt.Errorf("cortex: incomplete worker launcher")
	}
	if err := receipt.Job.Validate(); err != nil {
		return err
	}
	detached, err := sandbox.Seal(receipt.Job)
	if err != nil {
		return err
	}
	receipt.Job = detached
	receipt.Signature = bytes.Clone(receipt.Signature)
	if operationErr := l.authorized(ctx, receipt.Job); operationErr != nil {
		return operationErr
	}
	record, err := l.store.Enroll(ctx, receipt)
	if err != nil {
		return err
	}
	if record.Phase != workerstore.Accepted {
		return acknowledge(record)
	}
	if operationErr := l.admit(receipt); operationErr != nil {
		return operationErr
	}
	ctx, cancel := context.WithDeadline(ctx, acceptanceDeadline(receipt))
	defer cancel()
	ctx, bound := context.WithTimeout(ctx, 30*time.Second)
	defer bound()
	if err = l.preflight(ctx, l.profile, receipt.Job.ID); err != nil {
		latest, loadErr := l.store.Load(ctx, receipt.Job.ID)
		if loadErr == nil && latest.Phase != workerstore.Accepted {
			return acknowledge(latest)
		}
		return errors.Join(err, loadErr)
	}
	claim, err := l.store.Claim(ctx, receipt.Job.ID)
	if err != nil {
		if errors.Is(err, workerstore.ErrConflict) {
			latest, loadErr := l.store.Load(ctx, receipt.Job.ID)
			if loadErr != nil {
				return loadErr
			}
			return acknowledge(latest)
		}
		return err
	}
	if err = l.dispatch(ctx, receipt, claim); err != nil {
		return l.recover(receipt.Job.ID, err)
	}
	return nil
}

func acknowledge(record workerstore.Record) error {
	switch record.Phase {
	case workerstore.Claimed, workerstore.Finalizing, workerstore.Terminal:
		return nil
	case workerstore.Reaping:
		return broker.ErrPending
	default:
		return workerstore.ErrConflict
	}
}

func (l *Launcher) dispatch(ctx context.Context, receipt sandbox.Receipt, claim workerstore.Claim) error {
	if err := l.prepare(ctx, l.profile, claim); err != nil {
		return err
	}
	if err := l.preflight(ctx, l.profile, receipt.Job.ID); err != nil {
		return err
	}
	if err := l.authorized(ctx, receipt.Job); err != nil {
		return err
	}
	if err := l.admit(receipt); err != nil {
		return err
	}
	if _, err := l.store.VerifyClaim(ctx, receipt.Job.ID, claim.Token, claim.Record.Revision); err != nil {
		return err
	}
	if operationErr := l.start(ctx, receipt.Job.ID); operationErr != nil {
		return operationErr
	}
	return errors.Join(ctx.Err(), checkAcceptanceTime(receipt, l.now()))
}

func (l *Launcher) authorized(ctx context.Context, job sandbox.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	detached, err := sandbox.Seal(job)
	if err != nil {
		return err
	}
	if err = l.authorize(ctx, detached); err != nil {
		return errors.Join(broker.ErrDenied, err)
	}
	return ctx.Err()
}

func (l *Launcher) admit(receipt sandbox.Receipt) error {
	if err := l.profile.validate(); err != nil {
		return err
	}
	if err := checkAcceptanceTime(receipt, l.now()); err != nil {
		return err
	}
	cpu, memory, processes := receipt.Job.Limits.CPUMillis, receipt.Job.Limits.MemoryBytes, receipt.Job.Limits.Processes
	if cpu <= 0 || memory <= 0 || processes <= 0 {
		return fmt.Errorf("cortex: invalid guest limits")
	}
	if uint64(cpu) > l.profile.CPUMillis || uint64(memory) >= l.profile.MemoryBytes || uint64(processes) >= l.profile.Tasks || receipt.Job.Limits.WallTime > l.profile.WallTime {
		return fmt.Errorf("cortex: service ceiling cannot contain guest and worker")
	}
	return receipt.Capabilities.Supports(receipt.Job)
}

func (l *Launcher) recover(key id.ID, cause error) error {
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, broker.ErrDenied) {
		if _, err := l.store.Cancel(cleanup, key); err != nil {
			return errors.Join(cause, err)
		}
	}
	_, err := l.reaper.Reap(cleanup, l.store, key)
	return errors.Join(cause, err)
}
