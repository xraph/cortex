package execution

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

func recoverySnapshotFixture(t *testing.T) RunRecoverySnapshot {
	t.Helper()
	r := effectRequest()
	call := effectToolCall(r)
	r.RunCall = &call
	sealRequest := RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "closed"}
	_, digest, err := sealRequest.Freeze()
	require.NoError(t, err)
	receipt := effectApplied(r)
	return RunRecoverySnapshot{SchemaVersion: 1, Seal: RunCallSeal{Request: sealRequest, RequestDigest: digest, At: time.Now().UTC()}, Calls: []RecoveredRunCall{{Call: RunCallRecord{Request: call, StartedAt: time.Now().UTC()}}}, Effects: []EffectRecord{{Request: r, Receipt: &receipt}}}
}
func TestRunRecoverySnapshotPinsAllEvidence(t *testing.T) {
	original := recoverySnapshotFixture(t)
	frozen, err := SealRunRecoverySnapshot(original)
	require.NoError(t, err)
	require.NoError(t, frozen.Validate())
	_, digest, err := frozen.Calls[0].Call.Request.Freeze()
	require.NoError(t, err)
	late := frozen
	late.Calls = append([]RecoveredRunCall(nil), frozen.Calls...)
	late.Calls[0].Call.Completion = &RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("late local return"))}
	late.Calls[0].Call.CompletedAt = time.Now().UTC()
	updated, err := SealRunRecoverySnapshot(late)
	require.NoError(t, err)
	require.NotEqual(t, frozen.Digest, updated.Digest)
	original.Effects[0].Request.RunCall.Scope.Levels[0].Value = "mutated"
	require.NoError(t, frozen.Validate())
	require.Equal(t, "effects", frozen.Calls[0].Call.Request.Scope.Levels[0].Value)
	frozen.Effects[0].Receipt.ExternalID = "changed"
	require.Error(t, frozen.Validate())
}
func TestRunRecoverySnapshotRejectsUnboundMembership(t *testing.T) {
	for _, mode := range []string{"run", "scope", "duplicate call", "effect call", "receipt", "seal", "limit"} {
		t.Run(mode, func(t *testing.T) {
			r := recoverySnapshotFixture(t)
			switch mode {
			case "run":
				r.Calls[0].Call.Request.RunID = id.NewAgentRunID()
			case "scope":
				r.Calls[0].Call.Request.Scope.Levels[0].Value = "different"
			case "duplicate call":
				r.Calls = append(r.Calls, r.Calls[0])
			case "effect call":
				call := *r.Effects[0].Request.RunCall
				call.ID = id.New(PrefixRunCall)
				r.Effects[0].Request.RunCall = &call
				r.Effects[0].Receipt = nil
			case "receipt":
				r.Effects[0].Receipt.RequestDigest = evidence.Digest(nil)
			case "seal":
				r.Seal.RequestDigest = evidence.Digest(nil)
			case "limit":
				r.Calls = make([]RecoveredRunCall, MaxRunRecoveryEntries+1)
			}
			_, err := SealRunRecoverySnapshot(r)
			require.Error(t, err)
		})
	}
}
func TestRunRecoverySnapshotRetainsUnresolvedAndContradictoryEvidence(t *testing.T) {
	r := recoverySnapshotFixture(t)
	r.Effects[0].Request.RunCall = nil
	r.Effects[0].Receipt = nil
	call := &r.Calls[0].Call
	call.Request.Kind = RunCallComplete
	_, digest, err := call.Request.Freeze()
	require.NoError(t, err)
	call.Completion = &RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("local"))}
	call.CompletedAt = time.Now().UTC()
	r.Calls[0].Provider = &ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: ProviderCallSucceeded, Provider: "fixture", AdapterRevision: "1", AccountRef: "account", OperationID: "operation", ProofRef: "proof", ProofDigest: evidence.Digest([]byte("proof")), ObservedAt: time.Now().UTC(), ResultDigest: evidence.Digest([]byte("external"))}
	frozen, err := SealRunRecoverySnapshot(r)
	require.NoError(t, err)
	require.NoError(t, frozen.Validate())
	require.Nil(t, frozen.Effects[0].Receipt)
	require.Nil(t, frozen.Effects[0].Request.RunCall)
	require.ErrorIs(t, frozen.Calls[0].Provider.ValidateObservation(frozen.Calls[0].Call), ErrRunCallConflict)
}

func largeRecoverySnapshot(t *testing.T) RunRecoverySnapshot {
	t.Helper()
	s := recoverySnapshotFixture(t)
	s.Effects = nil
	template := s.Calls[0].Call
	template.Request.Kind = RunCallComplete
	s.Calls = make([]RecoveredRunCall, MaxRunRecoveryEntries)
	for i := range s.Calls {
		call := template
		call.Request.ID = id.New(PrefixRunCall)
		call.Request.OperationDigest = evidence.Digest([]byte(fmt.Sprintf("call:%d", i)))
		s.Calls[i] = RecoveredRunCall{Call: call}
	}
	sort.Slice(s.Calls, func(i, j int) bool { return s.Calls[i].Call.Request.Key() < s.Calls[j].Call.Request.Key() })
	return s
}

func addLargeRecoveryProofs(t *testing.T, s *RunRecoverySnapshot) {
	t.Helper()
	for i := range s.Calls {
		_, digest, err := s.Calls[i].Call.Request.Freeze()
		require.NoError(t, err)
		s.Calls[i].Provider = &ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: ProviderCallFailed, Provider: "fixture", AdapterRevision: "1", AccountRef: "account", OperationID: "operation", ProofRef: strings.Repeat("p", 4096), ProofDigest: evidence.Digest([]byte("proof")), ObservedAt: time.Now().UTC()}
	}
}

func TestRunRecoverySnapshotBounds(t *testing.T) {
	s := largeRecoverySnapshot(t)
	frozen, err := SealRunRecoverySnapshot(s)
	require.NoError(t, err)
	require.Len(t, frozen.Calls, MaxRunRecoveryEntries)
	excessive := s
	excessive.Calls = append(append([]RecoveredRunCall(nil), s.Calls...), s.Calls[0])
	_, err = SealRunRecoverySnapshot(excessive)
	require.ErrorIs(t, err, ErrInvalidRunCall)
	addLargeRecoveryProofs(t, &s)
	_, err = SealRunRecoverySnapshot(s)
	require.ErrorIs(t, err, ErrInvalidRunCall, "oversized journals cannot become truncated snapshots")
}

func TestRunRecoverySnapshotEncodingIncludesDigest(t *testing.T) {
	s := largeRecoverySnapshot(t)
	addLargeRecoveryProofs(t, &s)
	data, err := json.Marshal(s)
	require.NoError(t, err)
	reduction := len(data) - (MaxRunRecoveryBytes - 32)
	require.Positive(t, reduction)
	for i := range s.Calls {
		receipt := s.Calls[i].Provider
		remove := min(len(receipt.ProofRef)-1, reduction)
		receipt.ProofRef = receipt.ProofRef[:len(receipt.ProofRef)-remove]
		reduction -= remove
	}
	require.Zero(t, reduction)
	_, err = SealRunRecoverySnapshot(s)
	require.ErrorIs(t, err, ErrInvalidRunCall, "the returned digest must fit inside the encoding bound")
	for i := range s.Calls {
		if len(s.Calls[i].Provider.ProofRef) > 32 {
			s.Calls[i].Provider.ProofRef = s.Calls[i].Provider.ProofRef[32:]
			break
		}
	}
	frozen, err := SealRunRecoverySnapshot(s)
	require.NoError(t, err)
	data, err = json.Marshal(frozen)
	require.NoError(t, err)
	require.Len(t, data, MaxRunRecoveryBytes)
}
