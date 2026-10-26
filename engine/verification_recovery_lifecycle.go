package engine

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/verification"
)

// VerificationRecoveryConfig installs current job access and bounded recovery.
// Leave Service.Recoverer unset. Discovery grants and job grants are independent.
type VerificationRecoveryConfig struct {
	Service       coordination.VerificationRecoveryConfig
	Authorize     func(context.Context, id.ID) error
	Owner         string
	LeaseDuration time.Duration
}

// WithVerificationRecovery installs automatic recovery of captured jobs through
// the existing worker. It never generates candidates, repeats tools or publishes
// history. Jobs without durable capture intent remain explicit host work.
func WithVerificationRecovery(config VerificationRecoveryConfig) Option {
	return func(e *Engine) error {
		request := VerificationRecoveryRequest{Owner: config.Owner, LeaseDuration: config.LeaseDuration, Limit: 1}
		if e.verificationRecovery != nil || config.Authorize == nil || !hostport.IsNil(config.Service.Recoverer) || request.validate() != nil {
			return coordination.ErrInvalidScheduler
		}
		applied := config.Service
		applied.Recoverer = &capturedVerificationRecovery{engine: e, authorize: config.Authorize, request: request}
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("verification recovery operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewVerificationRecoveryService(applied)
		if err != nil {
			return err
		}
		e.verificationRecovery = service
		return nil
	}
}

// VerificationRecoveryStatus reports configuration and local lifecycle counters.
func (e *Engine) VerificationRecoveryStatus() (coordination.TaskServiceStatus, bool) {
	if e.verificationRecovery == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.verificationRecovery.Status(), true
}

func (e *Engine) configureVerificationRecovery() error {
	if e.verificationRecovery == nil {
		return nil
	}
	if e.verificationWorker == nil {
		return cortex.ErrVerificationUnavailable
	}
	if _, ok := e.verificationWorker.storage.(verification.CaptureIntentStore); !ok {
		return cortex.ErrVerificationUnavailable
	}
	return nil
}

type capturedVerificationRecovery struct {
	engine    *Engine
	authorize func(context.Context, id.ID) error
	request   VerificationRecoveryRequest
}

func (r *capturedVerificationRecovery) Recover(ctx context.Context, key id.ID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if key.IsNil() || key.Prefix() != verification.PrefixJob {
		return verification.ErrInvalid
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if authErr := r.authorize(ctx, key); authErr != nil {
		return authErr
	}
	w := r.engine.verificationWorker
	if w == nil {
		return cortex.ErrVerificationUnavailable
	}
	intents, ok := w.storage.(verification.CaptureIntentStore)
	if !ok {
		return cortex.ErrVerificationUnavailable
	}
	job, err := w.storage.GetVerificationJob(ctx, key)
	if errors.Is(err, verification.ErrJobNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	job, err = checkedWorkerJob(job, job.RequestDigest)
	if err != nil || job.Request.ID != key || job.Request.Binding.Scope.Canonical() != scope.Canonical() {
		return cortex.ErrVerificationUnavailable
	}
	if authErr := r.authorize(ctx, key); authErr != nil {
		return authErr
	}
	item := w.recoverJob(ctx, intents, job, r.request)
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	switch item.Status {
	case RecoveryCommitted, RecoveryTerminal, RecoveryBusy, RecoveryExpired:
		return nil
	case RecoveryUncertain:
		return verification.ErrCallUncertain
	default:
		return cortex.ErrVerificationUnavailable
	}
}
