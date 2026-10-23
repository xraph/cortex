package execution

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// EffectListRequest bounds a scan in one exact context scope. After is the
// SHA-256 digest of the last operation key, not a timestamp. RunID is optional.
// Scans include terminal records so discovering pending work remains bounded.
type EffectListRequest struct {
	RunID id.AgentRunID
	After string
	Limit int
}

// Validate rejects unbounded scans and malformed filters before storage access.
func (r EffectListRequest) Validate() error {
	if r.Limit < 1 || r.Limit > 256 || (!r.RunID.IsNil() && r.RunID.Prefix() != id.PrefixAgentRun) || (r.After != "" && !evidence.ValidDigest(r.After)) {
		return ErrInvalidEffect
	}
	return nil
}

// EffectDiscoveryStore is an optional host-only journal capability. Results
// include at most Limit records, ordered by operation-key digest, strictly after
// After. Implementations must validate complete request/receipt bindings and
// exact context scope. Pages are observations, not an atomic database snapshot.
type EffectDiscoveryStore interface {
	ListEffects(context.Context, EffectListRequest) ([]EffectRecord, error)
}

// EffectRecoveryStatus reports observation progress, never task acceptance.
type EffectRecoveryStatus string

const (
	EffectRecoveryTerminal    EffectRecoveryStatus = "terminal"
	EffectRecoveryUncertain   EffectRecoveryStatus = "uncertain"
	EffectRecoveryUnavailable EffectRecoveryStatus = "unavailable"
)

// EffectRecoveryItem excludes arguments, authority and provider evidence.
// Outcome is populated only after an authorized terminal receipt is observed.
type EffectRecoveryItem struct {
	OperationDigest string               `json:"operation_digest"`
	RunID           id.AgentRunID        `json:"run_id"`
	Status          EffectRecoveryStatus `json:"status"`
	Outcome         EffectOutcome        `json:"outcome,omitempty"`
}

// EffectRecoveryPage advances past every inspected operation, including denied
// and unknown operations. Empty NextAfter starts a new scan; callers must revisit
// earlier pages because new operations may sort before a retained cursor.
type EffectRecoveryPage struct {
	Items     []EffectRecoveryItem `json:"items"`
	NextAfter string               `json:"next_after,omitempty"`
}

// RecoverPage discovers operations from the executor's journal and reconciles
// each original attempt. The host must authorize exact-scope discovery before
// calling this internal API. Every operation also requires current authorization
// through Reconcile. Recovery never dispatches mutations or resumes agent runs.
func (e *EffectExecutor) RecoverPage(ctx context.Context, request EffectListRequest) (EffectRecoveryPage, error) {
	if err := ctx.Err(); err != nil {
		return EffectRecoveryPage{}, err
	}
	if err := request.Validate(); err != nil {
		return EffectRecoveryPage{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return EffectRecoveryPage{}, err
	}
	discovery, ok := e.store.(EffectDiscoveryStore)
	if !ok {
		return EffectRecoveryPage{}, ErrEffectUnavailable
	}
	select {
	case e.pageSlots <- struct{}{}:
		defer func() { <-e.pageSlots }()
	default:
		return EffectRecoveryPage{}, ErrEffectUnavailable
	}
	bounded, cancel := context.WithTimeout(ctx, MaxEffectDuration)
	defer cancel()
	records, err := discovery.ListEffects(bounded, request)
	if contextErr := bounded.Err(); contextErr != nil {
		return EffectRecoveryPage{}, contextErr
	}
	if err != nil || len(records) > request.Limit {
		return EffectRecoveryPage{}, ErrEffectUnavailable
	}
	// Detach and validate the entire page before any provider probe. Reconcile
	// reads the journal again, so a listed record never replaces durable proof.
	requests := make([]EffectRequest, len(records))
	previous := request.After
	for i, record := range records {
		frozen, _, freezeErr := record.Request.Freeze()
		cursor := evidence.Digest([]byte(frozen.OperationKey))
		if freezeErr != nil || frozen.Scope.Canonical() != scope.Canonical() || cursor <= previous || (!request.RunID.IsNil() && frozen.RunID != request.RunID) || (record.Receipt != nil && record.Receipt.Validate(frozen) != nil) {
			return EffectRecoveryPage{}, ErrEffectUnavailable
		}
		requests[i] = frozen
		previous = cursor
	}
	page := EffectRecoveryPage{Items: make([]EffectRecoveryItem, 0, len(requests)), NextAfter: request.After}
	for _, r := range requests {
		if contextErr := bounded.Err(); contextErr != nil {
			return page, contextErr
		}
		item := EffectRecoveryItem{OperationDigest: evidence.Digest([]byte(r.OperationKey)), RunID: r.RunID, Status: EffectRecoveryUnavailable}
		receipt, reconcileErr := e.Reconcile(bounded, r)
		switch {
		case reconcileErr == nil:
			item.Status, item.Outcome = EffectRecoveryTerminal, receipt.Outcome
		case errors.Is(reconcileErr, ErrEffectUncertain):
			item.Status = EffectRecoveryUncertain
		}
		page.Items = append(page.Items, item)
		page.NextAfter = item.OperationDigest
		if contextErr := bounded.Err(); contextErr != nil {
			return page, contextErr
		}
	}
	if len(requests) < request.Limit {
		page.NextAfter = ""
	}
	return page, nil
}
