package engine

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// EffectRequestResolver returns a retained host operation for an invocation.
// The host owns the stable operation-to-run mapping and original attempt ID.
// Resolve must return the same complete request across transport retries and
// worker replacement. A model call ID is correlation data, not authorization or
// proof of a new logical operation. Do not mint a replacement key after an
// uncertain outcome. Resolve current identity through the executor's authorizer.
type EffectRequestResolver func(context.Context, cortex.Invocation) (execution.EffectRequest, error)

// NewEffectToolHandler adapts the durable executor to WithTool. Register the
// handler's immutable revision with WithToolRevision and keep the model-facing
// definition consistent with the executor's host registration. Ordinary tools
// retain their existing behavior; this helper never wraps arbitrary handlers
// with a guessed reconciliation or idempotency contract.
//
// The result contains provider outcome and external identity/version. Private
// evidence references and internal authority bindings stay in the journal.
// Any unresolved, unavailable or malformed operation stops the engine with
// ErrEffectUncertain before another tool or model call. A terminal not_applied
// receipt is returned explicitly; neither outcome proves task correctness.
func NewEffectToolHandler(executor *execution.EffectExecutor, resolve EffectRequestResolver) (ToolHandler, error) {
	if executor == nil || resolve == nil {
		return nil, cortex.ErrEffectUnavailable
	}
	return func(ctx context.Context, inv cortex.Invocation) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, execution.MaxEffectDuration)
		defer cancel()
		scope, err := cortex.RequireScope(ctx)
		if err != nil || ctx.Err() != nil || cortex.ValidateScope(inv.Scope) != nil || scope.Canonical() != inv.Scope.Canonical() || inv.RunID.IsNil() || inv.RunID.Prefix() != id.PrefixAgentRun || inv.AgentID.IsNil() || inv.AgentID.Prefix() != id.PrefixAgent || len(inv.Call.Arguments) > 64<<10 || !effectCallID(inv.Call.ID) {
			return "", cortex.ErrEffectUncertain
		}
		// The resolver may retain or mutate its copy. Preserve the dispatch identity
		// and exact arguments for comparison before the executor can reserve a job.
		inv.Scope.Levels = append([]cortex.Level(nil), inv.Scope.Levels...)
		detached := inv
		detached.Scope.Levels = append([]cortex.Level(nil), inv.Scope.Levels...)
		request, err := resolve(ctx, detached)
		if err != nil {
			return "", cortex.ErrEffectUncertain
		}
		frozen, _, err := request.Freeze()
		if err != nil || frozen.Scope.Canonical() != inv.Scope.Canonical() || frozen.RunID != inv.RunID || frozen.Requirement.Tool != inv.Call.Name || frozen.ArgsDigest != evidence.Digest([]byte(inv.Call.Arguments)) {
			return "", cortex.ErrEffectUncertain
		}
		receipt, err := executor.Execute(ctx, frozen, []byte(inv.Call.Arguments))
		if err != nil {
			return "", cortex.ErrEffectUncertain
		}
		output := struct {
			Status          execution.EffectOutcome `json:"effect_status"`
			ExternalID      string                  `json:"external_id,omitempty"`
			ExternalVersion string                  `json:"external_version,omitempty"`
		}{Status: receipt.Outcome, ExternalID: receipt.ExternalID, ExternalVersion: receipt.ExternalVersion}
		data, err := json.Marshal(output)
		if err != nil {
			return "", cortex.ErrEffectUncertain
		}
		return string(data), nil
	}, nil
}
func effectCallID(value string) bool {
	return value != "" && len(value) <= 512 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsRune(value, 0)
}
