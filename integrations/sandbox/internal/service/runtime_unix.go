//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// DeploymentAttestation checks the installed deployment against host-owned live
// qualification evidence. It must verify the current worker, configuration and
// runtime pins, honor cancellation, and never qualify a deployment from fixtures.
// Missing qualification may be returned as unqualified capabilities; Start then
// refuses execution. This callback is trusted host code, not worker input.
type DeploymentAttestation func(context.Context) (sandbox.Capabilities, error)

// Runtime connects the trusted broker to a controller's durable worker journal.
// Only the broker and its supervisor may call it. The broker owns caller scope
// and grant checks; Inspect and Cancel deliberately permit trusted recovery after
// grants expire. Never expose these methods directly as transport handlers.
type Runtime struct {
	controller *Controller
	attest     DeploymentAttestation
}

var _ broker.Runtime = (*Runtime)(nil)

// NewRuntime requires a controller and an independent deployment attestation.
// Construction neither starts the controller nor qualifies the Linux worker.
func NewRuntime(controller *Controller, attest DeploymentAttestation) (*Runtime, error) {
	if controller == nil || controller.launcher == nil || controller.scanner == nil || attest == nil {
		return nil, fmt.Errorf("cortex: incomplete sandbox runtime adapter")
	}
	return &Runtime{controller: controller, attest: attest}, nil
}

// Capabilities checks the deployment while enrolled in the controller lifetime.
func (r *Runtime) Capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	operation, finish, err := r.begin(ctx)
	if err != nil {
		return sandbox.Capabilities{}, err
	}
	defer finish()
	return r.capabilities(operation)
}

func (r *Runtime) capabilities(ctx context.Context) (sandbox.Capabilities, error) {
	caps, err := r.attest(ctx)
	if operationErr := errors.Join(err, ctx.Err()); operationErr != nil {
		return sandbox.Capabilities{}, operationErr
	}
	return caps, nil
}

// Start rechecks current controls before the launcher verifies and enrolls the
// signed acceptance. Retries retain the original claim and acceptance deadline.
func (r *Runtime) Start(ctx context.Context, receipt sandbox.Receipt) error {
	operation, finish, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	caps, err := r.capabilities(operation)
	if err != nil {
		return err
	}
	if operationErr := checkCurrentCapabilities(caps, receipt); operationErr != nil {
		return operationErr
	}
	return r.controller.launcher.Launch(operation, receipt)
}

func checkCurrentCapabilities(caps sandbox.Capabilities, receipt sandbox.Receipt) error {
	if err := caps.Supports(receipt.Job); err != nil {
		return err
	}
	accepted := receipt.Capabilities
	if caps.RuntimeDigest != accepted.RuntimeDigest || caps.Profile != accepted.Profile || caps.Qualification != accepted.Qualification {
		return broker.ErrConflict
	}
	if caps.QualifiedUntil.Before(acceptanceDeadline(receipt)) {
		return broker.ErrConflict
	}
	return nil
}

// Inspect returns detached terminal output only after independent finalization.
// A claim or pending cleanup reports running without exposing proposal bytes.
// An enrolled but unclaimed acceptance remains pending, not started.
func (r *Runtime) Inspect(ctx context.Context, key id.ID) (sandbox.Result, error) {
	operation, finish, err := r.begin(ctx)
	if err != nil {
		return sandbox.Result{}, err
	}
	defer finish()
	record, err := r.controller.launcher.store.Load(operation, key)
	if err != nil {
		return sandbox.Result{}, err
	}
	if operationErr := operation.Err(); operationErr != nil {
		return sandbox.Result{}, operationErr
	}
	switch record.Phase {
	case workerstore.Accepted:
		return sandbox.Result{}, broker.ErrPending
	case workerstore.Claimed, workerstore.Finalizing, workerstore.Reaping:
		return sandbox.Result{JobDigest: record.Receipt.Job.Digest, State: sandbox.StateRunning}, nil
	case workerstore.Terminal:
		if record.Result != nil {
			return *record.Result, nil
		}
	}
	return sandbox.Result{}, workerstore.ErrCorrupt
}

// Cancel persists intent before stopping containment. An unknown identity is
// persistently masked and stopped before ErrNotFound permits the broker to mark
// a never-started acceptance cancelled. Failed cleanup must not report absence.
func (r *Runtime) Cancel(ctx context.Context, key id.ID) error {
	operation, finish, err := r.begin(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if key.IsNil() || key.Prefix() != sandbox.PrefixJob {
		return broker.ErrDenied
	}
	launcher := r.controller.launcher
	record, err := launcher.store.Cancel(operation, key)
	if err != nil {
		cleanupErr := launcher.reaper.FenceAndStop(operation, key)
		if errors.Is(err, broker.ErrNotFound) {
			if cleanupErr != nil {
				return cleanupErr
			}
			if operationErr := operation.Err(); operationErr != nil {
				return operationErr
			}
			return broker.ErrNotFound
		}
		return errors.Join(err, cleanupErr)
	}
	if record.Phase != workerstore.Terminal {
		err = r.controller.scanner.reap(operation, key, &RecoveryReport{})
	}
	return errors.Join(err, operation.Err())
}

func (r *Runtime) begin(ctx context.Context) (context.Context, func(), error) {
	operation, finish, err := r.controller.begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	bounded, cancel := context.WithTimeout(operation, 30*time.Second)
	return bounded, func() { cancel(); finish() }, nil
}
