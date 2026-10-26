package engine

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// AgentReplyRecoveryConfig installs current host authority for discovered runs.
// Leave Service.Recoverer unset. Authorize runs before private reads, admission,
// start and every provider/tool dispatch, with the exact run scope on context.
type AgentReplyRecoveryConfig struct {
	Service   coordination.AgentReplyRecoveryConfig
	Authorize func(context.Context, id.AgentRunID) error
}

// WithAgentReplyRecovery installs optional bounded background reply recovery.
// Discovery must include interrupted answer seals and admissions. Existing start
// receipts remain reconciliation errors; polling never grants replay authority.
func WithAgentReplyRecovery(config AgentReplyRecoveryConfig) Option {
	return func(e *Engine) error {
		if e.agentReplyRecovery != nil || config.Authorize == nil || !hostport.IsNil(config.Service.Recoverer) {
			return coordination.ErrInvalidScheduler
		}
		applied := config.Service
		applied.Recoverer = &agentReplyRecovery{engine: e, authorize: config.Authorize}
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("agent reply recovery operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewAgentReplyRecoveryService(applied)
		if err != nil {
			return err
		}
		e.agentReplyRecovery = service
		return nil
	}
}

// AgentReplyRecoveryStatus reports configuration and local lifecycle counters.
func (e *Engine) AgentReplyRecoveryStatus() (coordination.TaskServiceStatus, bool) {
	if e.agentReplyRecovery == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.agentReplyRecovery.Status(), true
}

func (e *Engine) configureAgentReplyRecovery() error {
	if e.agentReplyRecovery == nil {
		return nil
	}
	if e.store == nil {
		return cortex.ErrNoStore
	}
	if e.llm == nil {
		return cortex.ErrNoLLM
	}
	if _, ok := e.store.(suspension.ResumeAdmissionStore); !ok {
		return suspension.ErrResumeAdmissionUnavailable
	}
	if _, ok := e.store.(suspension.VersionedStore); !ok {
		return suspension.ErrVersionClaimUnavailable
	}
	if _, ok := e.store.(a2a.AskResolutionStore); !ok {
		return a2a.ErrAskResolutionUnavailable
	}
	return nil
}

type agentReplyRecovery struct {
	engine    *Engine
	authorize func(context.Context, id.AgentRunID) error
}

func (r *agentReplyRecovery) Recover(ctx context.Context, key id.AgentRunID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if key.Prefix() != id.PrefixAgentRun {
		return suspension.ErrResumeAdmissionConflict
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if authErr := r.authorize(ctx, key); authErr != nil {
		return authErr
	}
	current, err := r.engine.store.GetRun(ctx, key)
	if errors.Is(err, cortex.ErrRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current == nil || current.ID != key || current.Scope.Canonical() != scope.Canonical() {
		return suspension.ErrResumeAdmissionConflict
	}
	if current.State != run.StatePaused && current.State != run.StateRunning {
		return nil
	}
	if authErr := r.authorize(ctx, key); authErr != nil {
		return authErr
	}
	paused, err := r.engine.store.GetSuspension(ctx, key)
	if errors.Is(err, cortex.ErrNotSuspended) {
		return nil
	}
	if err != nil {
		return err
	}
	if paused == nil || paused.RunID != key || paused.Scope.Canonical() != scope.Canonical() {
		return suspension.ErrResumeAdmissionConflict
	}
	if paused.Reason != suspension.ReasonAgentReply {
		return nil
	}
	_, err = r.engine.RecoverAgentReply(ctx, key, paused.ID, r.authorize)
	if errors.Is(err, suspension.ErrResumeAdmissionNotFound) {
		return nil
	}
	return err
}
