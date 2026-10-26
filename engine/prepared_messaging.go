package engine

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// PreparedTeamMessagingConfig supplies current host authority for private teams.
// Callbacks receive exact scope and must honor cancellation. WithA2A supplies
// the tool definitions; this option permits those tools inside prepared teams.
// AuthorizeRun also checks assigned child IDs before their run rows are created.
type PreparedTeamMessagingConfig struct {
	AuthorizeTeam func(context.Context, id.OrchestrationID) error
	AuthorizeRun  func(context.Context, id.OrchestrationID, id.AgentRunID) error
}

// WithPreparedTeamMessaging enables captured-roster messaging under coordinator
// ownership. No background dispatcher receives a coordinator's execution rights.
func WithPreparedTeamMessaging(config PreparedTeamMessagingConfig) Option {
	return func(e *Engine) error {
		if e.preparedMessaging != nil || config.AuthorizeTeam == nil || config.AuthorizeRun == nil {
			return cortex.ErrInvalidPrepared
		}
		saved := config
		e.preparedMessaging = &saved
		return nil
	}
}

func snapshotUsesMessaging(snap *preparedSnapshot) bool {
	if snap == nil {
		return false
	}
	for _, tool := range snap.Tools {
		if tool.Name == toolAgentAsk || tool.Name == toolAgentSend || tool.Name == toolAgentInbox {
			return true
		}
	}
	return false
}

func (e *Engine) authorizePreparedMessaging(ctx context.Context, team id.OrchestrationID, key id.AgentRunID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.preparedMessaging == nil || e.a2a == nil {
		return cortex.ErrInvalidPrepared
	}
	if err := e.preparedMessaging.AuthorizeTeam(ctx, team); err != nil {
		return err
	}
	if !key.IsNil() {
		return e.preparedMessaging.AuthorizeRun(ctx, team, key)
	}
	return nil
}

type preparedMessagingKey struct{}
type preparedMessagingPumpKey struct{}

func (r *preparedTeamRunner) messagingBus() (*a2a.Bus, error) {
	r.busOnce.Do(func() {
		roster := &a2a.Roster{Scope: r.policy.Scope}
		enabled := false
		for name, prepared := range r.view.Participants {
			snap, err := prepared.snapshot()
			if err != nil {
				r.busErr = err
				return
			}
			enabled = enabled || snapshotUsesMessaging(snap)
			roster.Participants = append(roster.Participants, a2a.RosterParticipant{Name: name, AgentID: snap.Agent.ID})
		}
		if !enabled {
			return
		}
		if r.eng.preparedMessaging == nil {
			r.busErr = cortex.ErrInvalidPrepared
			return
		}
		r.busGate = make(chan struct{}, 1)
		r.bus, r.busErr = a2a.NewBus(a2a.BusConfig{Store: r.eng.store, ReservedRunner: r, RecipientObserver: r, Resumer: preparedTeamResumer{runner: r}, Roster: roster, OrchestrationID: r.policy.ExecutionID, Synchronous: true, Hooks: a2aHooks{eng: r.eng}, Metrics: r.eng.metrics,
			RecipientRecovery: &a2a.RecipientRecoveryConfig{
				AuthorizeDiscovery: func(ctx context.Context, team id.OrchestrationID) error {
					return r.eng.authorizePreparedMessaging(ctx, team, id.Nil)
				},
				AuthorizeDelivery: func(ctx context.Context, team id.OrchestrationID, _ id.DeliveryID) error {
					return r.authorizeMessagingCoordinator(ctx, team)
				},
			},
		})
	})
	return r.bus, r.busErr
}

func (r *preparedTeamRunner) authorizeMessagingCoordinator(ctx context.Context, team id.OrchestrationID) error {
	if team != r.policy.ExecutionID {
		return cortex.ErrInvalidPrepared
	}
	if err := orchestration.RequireCoordinator(ctx, team); err != nil {
		return err
	}
	return r.eng.authorizePreparedMessaging(ctx, team, id.Nil)
}

func (r *preparedTeamRunner) withMessaging(ctx context.Context) (context.Context, error) {
	bus, err := r.messagingBus()
	if err != nil || bus == nil {
		return ctx, err
	}
	return context.WithValue(ctx, preparedMessagingKey{}, r), nil
}

func (e *Engine) invocationMessageBus(ctx context.Context, inv cortex.Invocation) (*a2a.Bus, error) {
	provenance, _ := ExecutionProvenanceFromContext(ctx)
	if !hasOrchestrationBinding(provenance) {
		return e.a2a, nil
	}
	binding, ok := ctx.Value(teamCallContextKey{}).(teamCallBinding)
	runner, bound := ctx.Value(preparedMessagingKey{}).(*preparedTeamRunner)
	if !ok || !bound || binding.snapshot == nil || inv.RunID != binding.runID || inv.AgentID != binding.snapshot.Agent.ID || runner.policy.ExecutionID.String() != provenance.OrchestrationID || runner.policy.PreparedDigest != provenance.OrchestrationDigest {
		return nil, cortex.ErrInvalidPrepared
	}
	if err := e.checkTeamCall(ctx, inv.RunID, provenance); err != nil {
		return nil, err
	}
	return runner.messagingBus()
}

// bindPreparedMessageRun restores private routing for independent child resumes.
// Persisted provenance identifies data; current callbacks provide authority.
func (e *Engine) bindPreparedMessageRun(ctx context.Context, record *run.Run) (context.Context, error) {
	if record == nil {
		return ctx, cortex.ErrInvalidPrepared
	}
	if !hasOrchestrationBinding(record.Provenance) {
		return ctx, nil
	}
	if !validOrchestrationBinding(record.Provenance) || record.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return ctx, cortex.ErrInvalidPrepared
	}
	team, err := id.ParseOrchestrationID(record.Provenance.OrchestrationID)
	if err != nil {
		return ctx, err
	}
	runner, ok := ctx.Value(preparedMessagingKey{}).(*preparedTeamRunner)
	if !ok || runner.policy.ExecutionID != team || runner.policy.PreparedDigest != record.Provenance.OrchestrationDigest {
		runner, err = e.restorePreparedTeam(ctx, team)
		if err != nil {
			return ctx, err
		}
	}
	return runner.withMessaging(ctx)
}

type preparedTeamResumer struct{ runner *preparedTeamRunner }

func (preparedTeamResumer) ResumeAgentReply(context.Context, id.AgentRunID, string, string) error {
	return a2a.ErrAskResolutionUnavailable
}

func (a preparedTeamResumer) ResumeAgentResolution(ctx context.Context, resolution a2a.AskResolution) error {
	r := a.runner
	if err := r.authorizeMessagingCoordinator(ctx, r.policy.ExecutionID); err != nil {
		return err
	}
	if err := r.eng.authorizePreparedMessaging(ctx, r.policy.ExecutionID, resolution.Ask.AskerRunID); err != nil {
		return err
	}
	reader, err := r.resolutionReader()
	if err != nil {
		return err
	}
	resumer := a2aResumer{eng: r.eng, team: r.policy.ExecutionID, reader: reader}
	current, in, err := resumer.assembleResolvedQuestions(ctx, resolution, id.Nil)
	if err != nil || in == nil {
		return err
	}
	_, err = r.eng.RecoverAgentReply(ctx, current.ID, in.agentReplyVersion.SuspensionID, func(ac context.Context, key id.AgentRunID) error {
		if authErr := r.authorizeMessagingCoordinator(ac, r.policy.ExecutionID); authErr != nil {
			return authErr
		}
		return r.eng.authorizePreparedMessaging(ac, r.policy.ExecutionID, key)
	})
	return err
}

func (r *preparedTeamRunner) resolutionReader() (a2a.AskResolutionStore, error) {
	private, err := a2a.NewRoutingStore(r.eng.store, r.policy.ExecutionID)
	if err != nil {
		return nil, err
	}
	reader, ok := private.(a2a.AskResolutionStore)
	if !ok {
		return nil, a2a.ErrAskResolutionUnavailable
	}
	return reader, nil
}

// ReconcileOrchestration carries private traffic before strategy replay and
// before terminal publication. Waiting recipients keep the parent resumable.
func (r *preparedTeamRunner) ReconcileOrchestration(ctx context.Context) error {
	bus, err := r.messagingBus()
	if err != nil || bus == nil {
		return err
	}
	if active, pumping := ctx.Value(preparedMessagingPumpKey{}).(*preparedTeamRunner); pumping && active == r {
		return nil
	}
	if authorityErr := r.authorizeMessagingCoordinator(ctx, r.policy.ExecutionID); authorityErr != nil {
		return authorityErr
	}
	select {
	case r.busGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-r.busGate }()
	ctx = context.WithValue(ctx, preparedMessagingPumpKey{}, r)
	ctx = context.WithValue(ctx, preparedMessagingKey{}, r)
	if _, err = bus.ReclaimStaleDeliveries(ctx); err != nil {
		return err
	}
	if _, err = bus.SweepExpiredAsks(ctx); err != nil {
		return err
	}
	for range maxPendingQuestions {
		progress := 0
		var after id.DeliveryID
		for seen := 0; seen < 4096; {
			page, recoverErr := bus.RecoverRecipients(ctx, after)
			if recoverErr != nil {
				return recoverErr
			}
			progress += page.Completed
			seen += page.Examined
			after = page.Next
			if after.IsNil() {
				break
			}
		}
		delivered, drainErr := bus.Drain(ctx)
		if drainErr != nil {
			return drainErr
		}
		resumed, resumeErr := r.recoverMessageAskers(ctx)
		if resumeErr != nil {
			return resumeErr
		}
		progress += delivered + resumed
		if progress == 0 {
			queued, pendingErr := bus.PendingDispatch(ctx)
			if pendingErr != nil {
				return pendingErr
			}
			if queued {
				progress++
			}
		}
		if progress == 0 {
			break
		}
	}
	reader, err := a2a.NewRoutingStore(r.eng.store, r.policy.ExecutionID)
	if err != nil {
		return err
	}
	discovery, ok := reader.(a2a.RecipientDiscoveryStore)
	if !ok {
		return a2a.ErrRecipientDiscoveryUnavailable
	}
	pending, err := discovery.ListDeferredRecipients(ctx, id.Nil, 1)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return cortex.ErrOrchestrationWaiting
	}
	return nil
}

func (r *preparedTeamRunner) recoverMessageAskers(ctx context.Context) (int, error) {
	parent, err := r.eng.store.GetOrchestrationRun(ctx, r.policy.ExecutionID)
	if err != nil {
		return 0, err
	}
	if parent == nil || parent.ID != r.policy.ExecutionID || parent.Scope.Canonical() != r.policy.Scope.Canonical() || len(parent.AgentRunIDs) > 4096 {
		return 0, cortex.ErrInvalidContinuation
	}
	resumed := 0
	for _, key := range parent.AgentRunIDs {
		if authErr := r.eng.authorizePreparedMessaging(ctx, r.policy.ExecutionID, key); authErr != nil {
			return resumed, authErr
		}
		child, readErr := r.eng.store.GetRun(ctx, key)
		if readErr != nil {
			return resumed, readErr
		}
		if child.Scope.Canonical() != r.policy.Scope.Canonical() || child.Provenance.OrchestrationID != r.policy.ExecutionID.String() {
			return resumed, cortex.ErrInvalidPrepared
		}
		if child.State != run.StatePaused && child.State != run.StateRunning {
			continue
		}
		paused, readErr := r.eng.store.GetSuspension(ctx, key)
		if errors.Is(readErr, cortex.ErrNotSuspended) {
			continue
		}
		if readErr != nil {
			return resumed, readErr
		}
		if paused.Reason != suspension.ReasonAgentReply {
			continue
		}
		result, recoverErr := r.eng.RecoverAgentReply(ctx, key, paused.ID, func(ac context.Context, runID id.AgentRunID) error {
			if authErr := r.authorizeMessagingCoordinator(ac, r.policy.ExecutionID); authErr != nil {
				return authErr
			}
			return r.eng.authorizePreparedMessaging(ac, r.policy.ExecutionID, runID)
		})
		if errors.Is(recoverErr, suspension.ErrResumeAdmissionNotFound) || errors.Is(recoverErr, suspension.ErrResumeAlreadyStarted) {
			continue
		}
		if recoverErr != nil {
			return resumed, recoverErr
		}
		if result != nil && result.Revision > child.Revision {
			resumed++
		}
	}
	return resumed, nil
}

func (e *Engine) configurePreparedTeamMessaging() error {
	if e.preparedMessaging == nil {
		return nil
	}
	if e.a2a == nil || e.store == nil {
		return cortex.ErrInvalidPrepared
	}
	_, held := e.store.(a2a.HeldAskStore)
	_, binding := e.store.(a2a.DeliveryRunBinder)
	_, replies := e.store.(a2a.ReservedReplyStore)
	_, resolutions := e.store.(a2a.AskResolutionStore)
	_, discovery := e.store.(a2a.RecipientDiscoveryStore)
	_, versions := e.store.(suspension.VersionedStore)
	_, admissions := e.store.(suspension.ResumeAdmissionStore)
	routing, scoped := e.store.(a2a.ScopedRoutingBackend)
	if !held || !binding || !replies || !resolutions || !discovery || !versions || !admissions || !scoped || !routing.SupportsScopedOrchestrationDiscovery() || !routing.SupportsOrchestrationRouting() {
		return cortex.ErrInvalidPrepared
	}
	return nil
}
