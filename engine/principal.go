package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// encodePrincipal is the bytes a new run, or a checkpoint decision, keeps
// of the principal on ctx. A nil principal stores nothing and never
// reaches the codec, and so does any principal when no codec is
// installed: there is nothing to restore it with later.
//
// It runs before anything is written. A principal that cannot be encoded
// is a run that could not be resumed as its caller, so it is never
// created.
func (e *Engine) encodePrincipal(ctx context.Context) ([]byte, error) {
	principal := cortex.PrincipalFromContext(ctx)
	if principal == nil || e.principalCodec == nil {
		return nil, nil
	}
	data, err := e.principalCodec.EncodePrincipal(ctx, principal)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", cortex.ErrPrincipalUnavailable, err)
	}
	return data, nil
}

// restorePrincipal replaces the principal on ctx with the one the run was
// created under. Every path that claims a paused run to continue it calls
// this first, with the run it read before the claim, because the context
// it was handed belongs to whoever is resuming: an approver, the service
// that ran an external tool, the messaging bus. None of them is the
// run's caller, and tool authorization after the pause must not ask
// about them.
//
// The returned context carries nil when the run has no principal or no
// codec is installed. Never the resumer's.
//
// A revoked principal comes back with the error and a nil principal, and
// the caller claims the run and fails it with that error: the identity is
// gone and nothing will bring it back. Any other decode failure says
// nothing about the run, so the caller returns before the claim and the
// run stays paused for the next attempt. Tell the two apart with
// errors.Is(err, cortex.ErrPrincipalRevoked).
func (e *Engine) restorePrincipal(ctx context.Context, r *run.Run) (context.Context, error) {
	if e.principalCodec == nil || len(r.Principal) == 0 {
		return cortex.WithPrincipal(ctx, nil), nil
	}
	// Decoded under the run's own scope, since the identity being looked
	// up is the run's.
	principal, err := e.principalCodec.DecodePrincipal(cortex.WithScope(ctx, r.Scope), slices.Clone(r.Principal))
	if errors.Is(err, cortex.ErrPrincipalRevoked) {
		return cortex.WithPrincipal(ctx, nil), fmt.Errorf("cortex: restore run principal: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("cortex: restore run principal: %w: %w", cortex.ErrPrincipalUnavailable, err)
	}
	return cortex.WithPrincipal(ctx, principal), nil
}

// failRevokedResume fails a run claimed for a resume whose principal was
// revoked. The claim made the run this call's, so it is failed rather
// than left running, under its own scope like every other write a resume
// makes, with the codec's reason on it.
func (e *Engine) failRevokedResume(ctx context.Context, snapshot *run.Run, susp *suspension.Suspension, revoked error) error {
	ctx = cortex.WithScope(ctx, susp.Scope)
	r := e.claimedRun(ctx, susp.RunID, snapshot, susp.ClaimedRun)
	return e.failResume(ctx, r, r.AgentID, revoked)
}
