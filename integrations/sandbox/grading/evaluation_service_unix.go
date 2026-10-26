//go:build linux || darwin

package grading

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/verification"
)

// Start obtains the journal's exclusive process lock and starts bounded phase
// recovery using the host service context. It must carry current service authority.
func (a *EvaluationAdapter) Start(ctx context.Context) error {
	if a == nil {
		return verification.ErrInvalid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return verification.ErrConflict
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock, err := a.journal.lock()
	if err != nil {
		return err
	}
	service, cancel := context.WithCancel(ctx)
	a.lock = lock
	a.cancel = cancel
	a.done = make(chan struct{})
	go a.run(service, a.done)
	return nil
}

// Stop cancels active work and joins it before releasing the process lock. A
// deadline error leaves the lock held until work drains and Stop is retried.
func (a *EvaluationAdapter) Stop(ctx context.Context) error {
	if a == nil {
		return verification.ErrInvalid
	}
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done != done {
		return nil
	}
	err := a.lock.Close()
	a.lock = nil
	a.cancel = nil
	a.done = nil
	return err
}

// Failures counts unsuccessful phase passes without retaining sensitive errors.
func (a *EvaluationAdapter) Failures() uint64 { a.mu.Lock(); defer a.mu.Unlock(); return a.failures }
func (a *EvaluationAdapter) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(a.config.Interval)
	defer ticker.Stop()
	for {
		for _, entry := range a.entries {
			if ctx.Err() != nil {
				return
			}
			scoped := cortex.WithScope(ctx, entry.manifest.Binding.Scope)
			if err := a.advance(scoped, entry); err != nil && !errors.Is(err, os.ErrNotExist) && ctx.Err() == nil {
				a.mu.Lock()
				a.failures++
				a.mu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (a *EvaluationAdapter) advance(ctx context.Context, entry *evaluationEntry) error {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	// The trusted roster supplies the scope before any admission is hydrated.
	body, marshalErr := json.Marshal(entry.manifest.Binding)
	if marshalErr != nil {
		return verification.ErrInvalid
	}
	var binding verification.Binding
	if json.Unmarshal(body, &binding) != nil {
		return verification.ErrInvalid
	}
	if err := a.config.Tests.Authorizer.AuthorizeVerification(ctx, binding); err != nil {
		return err
	}
	var admission evaluationAdmission
	if err := a.journal.read(ctx, entry.manifest.Digest+".admission", &admission); err != nil {
		return err
	}
	selected, expected, err := a.admission(ctx, admission.Dispatch)
	if err != nil {
		return err
	}
	if selected != entry || expected.Digest != admission.Digest {
		return verification.ErrInvalid
	}
	if identityErr := a.checkCallIdentity(ctx, entry.manifest.Digest, admission); identityErr != nil {
		return identityErr
	}
	var retained evaluationOutcome
	err = a.journal.read(ctx, entry.manifest.Digest+".outcome", &retained)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	operation, cancel := context.WithDeadline(ctx, admission.Dispatch.Input.Request.Deadline)
	defer cancel()
	report, err := entry.verifier.Verify(operation, admission.Dispatch.Input.Request)
	if err != nil {
		return err
	}
	if materialErr := a.validateMaterial(operation, entry, admission.Dispatch.Input, report); materialErr != nil {
		return materialErr
	}
	result := verification.CallResult{CallID: admission.Dispatch.Call.Request.ID, Report: &report, Usage: verification.EvaluationUsage{Receipt: evidence.Digest([]byte(admission.Digest + report.Digest))}}
	if _, _, err = result.Freeze(); err != nil {
		return err
	}
	return a.journal.write(operation, entry.manifest.Digest+".outcome", evaluationOutcome{AdmissionDigest: admission.Digest, Result: result})
}
