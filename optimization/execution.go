package optimization

import (
	"fmt"
	"strings"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
	"github.com/xraph/cortex/run"
)

// Validate checks receipt identity; capability qualification belongs to the host.
func (receipt EnvironmentReceipt) Validate() error {
	if _, err := ScopeKey(receipt.Scope); err != nil {
		return err
	}
	if _, err := ParseID(receipt.AttemptID.String(), PrefixAttempt); err != nil {
		return err
	}
	for _, value := range []string{receipt.ID, receipt.Provider, receipt.EnvironmentID, receipt.Fence.Owner} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value {
			return ErrInvalidSpec
		}
	}
	if receipt.Fence.Generation < 1 {
		return ErrInvalidSpec
	}
	return validateContentRef(receipt.Snapshot)
}

// Validate requires concrete containment evidence for a confirmed stop.
func (receipt StopReceipt) Validate() error {
	if _, err := ParseID(receipt.AttemptID.String(), PrefixAttempt); err != nil {
		return err
	}
	if strings.TrimSpace(receipt.EnvironmentReceiptID) == "" || strings.TrimSpace(receipt.Fence.Owner) == "" || receipt.Fence.Generation < 1 {
		return ErrInvalidSpec
	}
	switch receipt.State {
	case StopTerminated, StopQuarantined:
		return validateContentRef(receipt.Evidence)
	case StopUnknown:
		return nil
	default:
		return ErrInvalidSpec
	}
}

// SealExecution binds the captured output, original run identity and receipts.
// It checks internal consistency, not whether referenced artifacts still exist.
func SealExecution(record ExecutionRecord) (ExecutionRecord, error) {
	var empty ExecutionRecord
	if _, err := ParseID(record.RunID.String(), id.PrefixAgentRun); err != nil {
		return empty, err
	}
	if !validDigest(record.PreparedDigest) || !validDigest(record.InputDigest) || record.StartedAt.IsZero() || record.CompletedAt.Before(record.StartedAt) || record.Duration < 0 || record.Duration != record.CompletedAt.Sub(record.StartedAt) {
		return empty, ErrInvalidSpec
	}
	if record.Outcome.StopReason == "" || record.Outcome.StepCount < 0 || record.Outcome.TokensUsed < 0 || !record.Outcome.CompletedAt.Equal(record.CompletedAt) {
		return empty, ErrInvalidSpec
	}
	switch record.State {
	case run.StateCompleted:
		if record.Outcome.StopReason != run.StopFinalAnswer {
			return empty, ErrIncompleteEvidence
		}
	case run.StateIncomplete, run.StateFailed, run.StateCancelled:
	default:
		return empty, ErrIncompleteEvidence
	}
	if err := record.Environment.Validate(); err != nil {
		return empty, err
	}
	if err := record.Usage.Validate(); err != nil {
		return empty, err
	}
	for prefix, ids := range map[id.Prefix][]id.ID{id.PrefixStep: record.StepIDs, id.PrefixToolCall: record.ToolCallIDs} {
		seen := make(map[string]bool, len(ids))
		for _, entityID := range ids {
			if _, err := ParseID(entityID.String(), prefix); err != nil {
				return empty, err
			}
			if seen[entityID.String()] {
				return empty, ErrInvalidSpec
			}
			seen[entityID.String()] = true
		}
	}
	scopeKey, scopeErr := ScopeKey(record.Environment.Scope)
	if scopeErr != nil {
		return empty, scopeErr
	}
	for _, artifact := range record.Artifacts {
		artifactScope, err := ScopeKey(artifact.Scope)
		if err != nil {
			return empty, err
		}
		if artifactScope != scopeKey || !validDigest(artifact.Digest) || artifact.Size < 0 || strings.TrimSpace(artifact.Kind) == "" || strings.TrimSpace(artifact.AccessPolicy) == "" || strings.TrimSpace(artifact.MediaType) == "" {
			return empty, ErrInvalidSpec
		}
	}
	record.Digest = ""
	encoded, err := canonical.Marshal(record)
	if err != nil {
		return empty, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	var detached ExecutionRecord
	if err := canonical.Decode(encoded, &detached); err != nil {
		return empty, err
	}
	detached.Digest = digestBytes(encoded)
	return detached, nil
}
