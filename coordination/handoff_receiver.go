package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/planning"
)

// HandoffContext is freshly resolved by the receiving host, never from the peer.
type HandoffContext struct {
	Plan      planning.Plan
	Authority HandoffAuthority
}

// HandoffReceiverConfig separates host resolution from durable acceptance.
// Accept must atomically check current assignment ownership, grants and plan
// revision, and deduplicate by assignment/fence/digest. Success acknowledges only
// retained work. It must not dispatch an unreserved agent or mark a task passed.
// Both callbacks must honor cancellation and protect direct artifact access.
type HandoffReceiverConfig struct {
	Resolve       func(context.Context, Handoff) (HandoffContext, error)
	Accept        func(context.Context, Handoff) error
	Now           func() time.Time
	MaxConcurrent int
}

// HandoffReceiver admits bounded host work without using an ordinary agent run.
type HandoffReceiver struct {
	config HandoffReceiverConfig
	slots  chan struct{}
}

// NewHandoffReceiver requires explicit current-authority and persistence ports.
func NewHandoffReceiver(config HandoffReceiverConfig) (*HandoffReceiver, error) {
	if config.Resolve == nil || config.Accept == nil || config.MaxConcurrent < 1 || config.MaxConcurrent > 64 {
		return nil, ErrHandoffUnavailable
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &HandoffReceiver{config: config, slots: make(chan struct{}, config.MaxConcurrent)}, nil
}

// Receive validates the complete proposal before passing it to host acceptance.
func (r *HandoffReceiver) Receive(ctx context.Context, h Handoff) error {
	if r == nil {
		return ErrHandoffUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.Validate() != nil {
		return ErrInvalidHandoff
	}
	frozen, err := SealHandoff(h)
	if err != nil {
		return err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	now := r.config.Now()
	if scope.Canonical() != frozen.Scope.Canonical() || now.IsZero() || !now.Before(frozen.Deadline) {
		return ErrInvalidHandoff
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return ErrHandoffUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, min(30*time.Second, frozen.Deadline.Sub(now)))
	defer cancel()
	resolveInput, err := SealHandoff(frozen)
	if err != nil {
		return err
	}
	current, err := r.config.Resolve(bounded, resolveInput)
	if contextErr := bounded.Err(); contextErr != nil {
		return contextErr
	}
	if err != nil {
		return ErrHandoffUnavailable
	}
	if ValidateHandoff(frozen, current.Plan, current.Authority, r.config.Now()) != nil {
		return ErrInvalidHandoff
	}
	acceptErr := r.config.Accept(bounded, frozen)
	if contextErr := bounded.Err(); contextErr != nil {
		return contextErr
	}
	if acceptErr != nil {
		return ErrHandoffUnavailable
	}
	return nil
}
