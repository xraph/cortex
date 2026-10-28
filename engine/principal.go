package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	log "github.com/xraph/go-utils/log"

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
// It sorts a failed decode for the caller, so a claim site only has to
// act on what it gets back:
//
//   - err is a decode that failed for now (wrapping
//     cortex.ErrPrincipalUnavailable). It says nothing about the run, so
//     the caller returns it before the claim and the run stays paused for
//     the next attempt.
//   - revoked is a principal that is gone for good (wrapping
//     cortex.ErrPrincipalRevoked), with a nil principal on ctx. The caller
//     claims the run anyway and hands revoked to failRevokedResume, since
//     the claim is what makes the run its to fail.
func (e *Engine) restorePrincipal(ctx context.Context, r *run.Run) (restored context.Context, revoked, err error) {
	if e.principalCodec == nil || len(r.Principal) == 0 {
		return cortex.WithPrincipal(ctx, nil), nil, nil
	}
	// Decoded under the run's own scope, since the identity being looked
	// up is the run's.
	principal, decodeErr := e.principalCodec.DecodePrincipal(cortex.WithScope(ctx, r.Scope), slices.Clone(r.Principal))
	if errors.Is(decodeErr, cortex.ErrPrincipalRevoked) {
		return cortex.WithPrincipal(ctx, nil), fmt.Errorf("cortex: restore run principal: %w", decodeErr), nil
	}
	if decodeErr != nil {
		return nil, nil, fmt.Errorf("cortex: restore run principal: %w: %w", cortex.ErrPrincipalUnavailable, decodeErr)
	}
	return cortex.WithPrincipal(ctx, principal), nil, nil
}

// failRevokedResume fails a run claimed for a resume whose principal was
// revoked. The claim made the run this call's, so it is failed rather
// than left running, under its own scope like every other write a resume
// makes, with the codec's reason on it. The warning is there because a
// run failing for its caller's identity is something an operator should
// see without reading every run.
func (e *Engine) failRevokedResume(ctx context.Context, snapshot *run.Run, susp *suspension.Suspension, revoked error) error {
	ctx = cortex.WithScope(ctx, susp.Scope)
	r := e.claimedRun(ctx, susp.RunID, snapshot, susp.ClaimedRun)
	e.logWarn("fail a resumed run whose principal was revoked", revoked, log.String("run_id", r.ID.String()))
	return e.failResume(ctx, r, r.AgentID, revoked)
}
