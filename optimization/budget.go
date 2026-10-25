package optimization

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// MeterRepository supplies scoped experiment authority and atomic accounting.
type MeterRepository interface {
	GetExperiment(context.Context, id.ID) (Experiment, error)
	Transact(context.Context, func(Transaction) error) error
}

// CallMeter reserves usage and records dispatch before an external operation.
// Only a successfully committed BeginCall permits a new provider invocation.
// Reserve alone does not grant dispatch permission.
type CallMeter interface {
	Meter
	BeginCall(context.Context, UsageReservation, string) (Reservation, error)
}

// LedgerMeter applies current host authority to durable accounting transactions.
// It resolves the experiment's original requester for every operation. Provider
// execution and receipt reconciliation remain the caller's responsibility.
type LedgerMeter struct {
	repository MeterRepository
	authorizer Authorizer
	resolver   IdentityResolver
}

var _ CallMeter = (*LedgerMeter)(nil)

// NewLedgerMeter requires explicit persistence and host authorization services.
func NewLedgerMeter(repository MeterRepository, authorizer Authorizer, resolver IdentityResolver) (*LedgerMeter, error) {
	if repository == nil || authorizer == nil || resolver == nil {
		return nil, ErrInvalidSpec
	}
	return &LedgerMeter{repository: repository, authorizer: authorizer, resolver: resolver}, nil
}

// Reserve holds a conservative debit without dispatching any provider operation.
func (m *LedgerMeter) Reserve(ctx context.Context, request UsageReservation) (Reservation, error) {
	return m.reserve(ctx, request, "", false)
}

// BeginCall atomically reserves and marks a bound request as possibly started.
// An existing dispatch returns ErrUnknownAttempt and must be reconciled, even
// when the worker cannot determine whether the provider received the request.
func (m *LedgerMeter) BeginCall(ctx context.Context, request UsageReservation, digest string) (Reservation, error) {
	return m.reserve(ctx, request, digest, true)
}

func (m *LedgerMeter) reserve(ctx context.Context, request UsageReservation, digest string, begin bool) (Reservation, error) {
	resolved, cleanup, err := m.authorize(ctx, request.ExperimentID)
	if err != nil {
		return Reservation{}, err
	}
	defer cleanup()
	var value Reservation
	err = m.repository.Transact(resolved, func(tx Transaction) error {
		var writeErr error
		if begin {
			value, writeErr = tx.BeginUsageCall(request, digest)
		} else {
			value, writeErr = tx.Reserve(request)
		}
		return writeErr
	})
	if err != nil {
		return Reservation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Reservation{}, err
	}
	if err := resolved.Err(); err != nil {
		return Reservation{}, err
	}
	return value, nil
}

// Settle records a host-attributed provider receipt or uncertainty observation.
// Callers must establish receipt provenance; model output is not a usage receipt.
func (m *LedgerMeter) Settle(ctx context.Context, settlement UsageSettlement) error {
	resolved, cleanup, err := m.authorize(ctx, settlement.ExperimentID)
	if err != nil {
		return err
	}
	defer cleanup()
	return m.repository.Transact(resolved, func(tx Transaction) error { return tx.Settle(settlement) })
}

func (m *LedgerMeter) authorize(ctx context.Context, experimentID id.ID) (context.Context, context.CancelFunc, error) {
	return m.authorizeAction(ctx, experimentID, ActionStart)
}

func (m *LedgerMeter) authorizeAction(ctx context.Context, experimentID id.ID, action Action) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, nil, cortex.ErrNoScope
	}
	key, err := ScopeKey(scope)
	if err != nil {
		return nil, nil, err
	}
	if _, err = ParseID(experimentID.String(), PrefixExperiment); err != nil {
		return nil, nil, err
	}
	experiment, err := m.repository.GetExperiment(copyScopeContext(ctx), experimentID)
	if err != nil {
		return nil, nil, err
	}
	if experiment.ID != experimentID || experiment.Spec.Scope.Canonical() != key {
		return nil, nil, ErrPolicyDenied
	}
	resources := []Resource{{Scope: copyScope(scope), AgentID: experiment.Spec.AgentID, ID: experimentID, Kind: "experiment"}}
	if experiment.Spec.Parent != nil {
		parent, readErr := m.repository.GetExperiment(copyScopeContext(ctx), experiment.Spec.Parent.ExperimentID)
		if readErr != nil {
			return nil, nil, readErr
		}
		if parent.ID != experiment.Spec.Parent.ExperimentID || parent.Spec.Scope.Canonical() != key || parent.Spec.Requester != experiment.Spec.Requester || parent.Spec.Parent != nil || parent.Spec.Improvement == nil {
			return nil, nil, ErrPolicyDenied
		}
		resources = append(resources, Resource{Scope: copyScope(scope), AgentID: parent.Spec.AgentID, ID: parent.ID, Kind: "experiment"})
	}
	authorize := func(current context.Context) error {
		for _, resource := range resources {
			if authErr := m.authorizer.Authorize(copyScopeContext(current), action, resource); authErr != nil {
				return authErr
			}
		}
		return nil
	}
	if authorizeErr := authorize(ctx); authorizeErr != nil {
		return nil, nil, authorizeErr
	}
	resolved, cleanup, err := ResolveIdentity(ctx, m.resolver, experiment.Spec.Requester, scope)
	if err != nil {
		return nil, nil, err
	}
	if err = authorize(resolved); err != nil {
		cleanup()
		return nil, nil, err
	}
	if err = resolved.Err(); err != nil {
		cleanup()
		return nil, nil, err
	}
	return resolved, cleanup, nil
}
