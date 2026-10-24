package execution

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// EffectRecoveryPageRequest selects a bounded page from a permanently sealed
// run. Restart each pass with After empty and revisit uncertain operations.
type EffectRecoveryPageRequest struct {
	Seal  RunCallSeal
	After string
	Limit int
}

// RecoverSealedPage checks current scan authority and the retained seal before
// and after reconciliation. The store must implement BoundEffectStore,
// EffectDiscoveryStore and RunCallSealStore, and every effect writer must honor
// that seal. Per-operation authorization remains mandatory through Reconcile.
// Errors return no cursor, although earlier terminal receipts may have committed.
// Legacy unbound operations are included; terminal status does not establish a
// task-call binding, successful task acceptance or complete usage accounting.
func (e *EffectExecutor) RecoverSealedPage(ctx context.Context, request EffectRecoveryPageRequest) (EffectRecoveryPage, error) {
	if err := ctx.Err(); err != nil {
		return EffectRecoveryPage{}, err
	}
	if request.Seal.Validate() != nil {
		return EffectRecoveryPage{}, ErrInvalidRunCall
	}
	if e.authorizeScan == nil {
		return EffectRecoveryPage{}, ErrEffectUnavailable
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return EffectRecoveryPage{}, err
	}
	if scope.Canonical() != request.Seal.Request.Scope.Canonical() {
		return EffectRecoveryPage{}, cortex.ErrInvalidScope
	}
	seal := request.Seal
	seal.Request = cloneSealRequest(seal.Request)
	query := EffectListRequest{RunID: seal.Request.RunID, After: request.After, Limit: request.Limit}
	if validateErr := query.Validate(); validateErr != nil {
		return EffectRecoveryPage{}, validateErr
	}
	storage, ok := e.store.(RunCallSealStore)
	if !ok || hostport.IsNil(storage) {
		return EffectRecoveryPage{}, ErrEffectUnavailable
	}
	if _, supported := e.store.(BoundEffectStore); !supported {
		return EffectRecoveryPage{}, ErrEffectUnavailable
	}
	if _, supported := e.store.(EffectDiscoveryStore); !supported {
		return EffectRecoveryPage{}, ErrEffectUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, MaxEffectDuration)
	defer cancel()
	// Separate page and operation slots let a bounded scan enter RecoverPage
	// without holding the slot that its per-operation reconciliation needs.
	select {
	case e.scanSlots <- struct{}{}:
		defer func() { <-e.scanSlots }()
	case <-bounded.Done():
		return EffectRecoveryPage{}, bounded.Err()
	}
	if authErr := authorizeSealedScan(bounded, storage, seal, e.authorizeScan); authErr != nil {
		return EffectRecoveryPage{}, authErr
	}
	page, err := e.RecoverPage(bounded, query)
	if err != nil {
		return EffectRecoveryPage{}, err
	}
	for _, item := range page.Items {
		if item.Status == EffectRecoveryUnavailable {
			return EffectRecoveryPage{}, ErrEffectUnavailable
		}
	}
	if authErr := authorizeSealedScan(bounded, storage, seal, e.authorizeScan); authErr != nil {
		return EffectRecoveryPage{}, authErr
	}
	return page, nil
}
