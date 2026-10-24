package execution

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// ProviderRecoveryPageRequest selects one bounded page in an already sealed
// journal. The caller starts each pass with After empty and follows NextAfter.
// Revisit unresolved calls on a new pass; a cursor never proves their completion.
type ProviderRecoveryPageRequest struct {
	Seal  RunCallSeal
	After string
	Limit int
}

// ProviderRecoveryPage counts observations, not newly committed transitions.
// Terminal includes authenticated success, failure and non-acceptance. ToolCalls
// require separate effect recovery. An empty NextAfter ends only this scan pass;
// it does not establish task success, complete accounting or permission to retry.
type ProviderRecoveryPage struct {
	NextAfter                                string
	Scanned, Terminal, Unresolved, ToolCalls int
}

// ReconcilePage validates the full page before querying any provider. It requires
// AuthorizeScan and a RunCallSealStore on the configured receipt store. Every
// model call uses Reconcile's current authorization and authenticated proof path.
// Errors return no cursor, although earlier proof writes may have committed.
// Retrying the same page reauthenticates those receipts without repeating calls.
// Timeout bounds the whole page including its queue wait. MaxConcurrent bounds
// pages separately from provider operations so queued pages cannot deadlock calls.
func (p *ProviderReconciler) ReconcilePage(ctx context.Context, request ProviderRecoveryPageRequest) (ProviderRecoveryPage, error) {
	if err := ctx.Err(); err != nil {
		return ProviderRecoveryPage{}, err
	}
	if request.Seal.Validate() != nil || p.config.AuthorizeScan == nil {
		return ProviderRecoveryPage{}, ErrInvalidRunCall
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return ProviderRecoveryPage{}, err
	}
	if scope.Canonical() != request.Seal.Request.Scope.Canonical() {
		return ProviderRecoveryPage{}, cortex.ErrInvalidScope
	}
	seal := request.Seal
	seal.Request = cloneSealRequest(seal.Request)
	query := RunCallListRequest{RunID: seal.Request.RunID, After: request.After, Limit: request.Limit}
	if validateErr := query.Validate(); validateErr != nil {
		return ProviderRecoveryPage{}, validateErr
	}
	storage, ok := p.config.Store.(RunCallSealStore)
	if !ok || hostport.IsNil(storage) {
		return ProviderRecoveryPage{}, ErrRunCallUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, p.config.Timeout)
	defer cancel()
	select {
	case p.scanSlots <- struct{}{}:
		defer func() { <-p.scanSlots }()
	case <-bounded.Done():
		return ProviderRecoveryPage{}, bounded.Err()
	}
	if authErr := p.authorizeSealedScan(bounded, storage, seal); authErr != nil {
		return ProviderRecoveryPage{}, authErr
	}
	records, err := storage.ListRunCalls(bounded, query)
	if err != nil {
		return ProviderRecoveryPage{}, err
	}
	if len(records) > query.Limit {
		return ProviderRecoveryPage{}, ErrInvalidRunCall
	}
	// Detach each binding before invoking host code. No malformed later entry can
	// trigger an earlier provider lookup or receipt write.
	requests := make([]RunCallRequest, 0, len(records))
	previous := query.After
	for _, record := range records {
		if record.Validate() != nil || record.Request.Scope.Canonical() != scope.Canonical() || record.Request.RunID != query.RunID || record.Request.Key() <= previous {
			return ProviderRecoveryPage{}, ErrInvalidRunCall
		}
		frozen, _, freezeErr := record.Request.Freeze()
		if freezeErr != nil {
			return ProviderRecoveryPage{}, freezeErr
		}
		requests = append(requests, frozen)
		previous = frozen.Key()
	}
	page := ProviderRecoveryPage{Scanned: len(requests)}
	for _, call := range requests {
		if contextErr := bounded.Err(); contextErr != nil {
			return ProviderRecoveryPage{}, contextErr
		}
		if call.Kind == RunCallTool {
			page.ToolCalls++
			continue
		}
		_, reconcileErr := p.Reconcile(bounded, call)
		if contextErr := bounded.Err(); contextErr != nil {
			return ProviderRecoveryPage{}, contextErr
		}
		switch {
		case errors.Is(reconcileErr, ErrProviderCallUncertain):
			page.Unresolved++
		case reconcileErr != nil:
			return ProviderRecoveryPage{}, reconcileErr
		default:
			page.Terminal++
		}
	}
	if authErr := p.authorizeSealedScan(bounded, storage, seal); authErr != nil {
		return ProviderRecoveryPage{}, authErr
	}
	if len(requests) == query.Limit {
		page.NextAfter = previous
	}
	return page, nil
}

func (p *ProviderReconciler) authorizeSealedScan(ctx context.Context, storage RunCallSealStore, seal RunCallSeal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	offered := seal
	offered.Request = cloneSealRequest(seal.Request)
	if err := p.config.AuthorizeScan(ctx, offered); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := storage.GetRunCallSeal(ctx, seal.Request.RunID)
	if err != nil {
		return err
	}
	if current == nil || current.Validate() != nil || current.RequestDigest != seal.RequestDigest || !current.At.Equal(seal.At) {
		return ErrRunCallConflict
	}
	return ctx.Err()
}
