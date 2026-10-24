package execution_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
)

func providerReceipt(t *testing.T, r execution.RunCallRequest) execution.ProviderCallReceipt {
	t.Helper()
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	return execution.ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.ProviderCallSucceeded, Provider: "fixture", AdapterRevision: "v1", AccountRef: "account-one", OperationID: "original-operation", ProofRef: "proof-one", ProofDigest: evidence.Digest([]byte("signed fixture")), ObservedAt: time.Now().UTC(), ResultDigest: evidence.Digest([]byte("output")), Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 7}}}
}

func TestProviderReceiptBindsOriginalCallAndExplicitUsage(t *testing.T) {
	r := runCallRequest()
	c := providerReceipt(t, r)
	_, digest, err := c.Freeze(r)
	require.NoError(t, err)
	require.True(t, evidence.ValidDigest(digest))
	for name, change := range map[string]func(*execution.ProviderCallReceipt){
		"request":           func(c *execution.ProviderCallReceipt) { c.RequestDigest = r.RequestDigest },
		"outcome":           func(c *execution.ProviderCallReceipt) { c.Outcome = "not-found" },
		"provider":          func(c *execution.ProviderCallReceipt) { c.Provider = "" },
		"revision":          func(c *execution.ProviderCallReceipt) { c.AdapterRevision = "" },
		"account":           func(c *execution.ProviderCallReceipt) { c.AccountRef = "" },
		"operation":         func(c *execution.ProviderCallReceipt) { c.OperationID = "" },
		"proof":             func(c *execution.ProviderCallReceipt) { c.ProofDigest = "" },
		"reference":         func(c *execution.ProviderCallReceipt) { c.ProofRef = "" },
		"time":              func(c *execution.ProviderCallReceipt) { c.ObservedAt = time.Time{} },
		"result":            func(c *execution.ProviderCallReceipt) { c.ResultDigest = "" },
		"unknown usage":     func(c *execution.ProviderCallReceipt) { c.Usage.Known = 0 },
		"negative usage":    func(c *execution.ProviderCallReceipt) { c.Usage.Actual.Tokens = -1 },
		"foreign dimension": func(c *execution.ProviderCallReceipt) { c.Usage.Known |= execution.KnownSteps },
		"failed result":     func(c *execution.ProviderCallReceipt) { c.Outcome = execution.ProviderCallFailed },
	} {
		t.Run(name, func(t *testing.T) {
			changed := c
			change(&changed)
			_, _, freezeErr := changed.Freeze(r)
			require.ErrorIs(t, freezeErr, execution.ErrInvalidRunCall)
		})
	}
	c.Usage = execution.ReservationUsage{}
	_, _, err = c.Freeze(r)
	require.NoError(t, err, "unreported usage stays unknown")
	c.Outcome, c.ResultDigest = execution.ProviderCallNotAccepted, ""
	_, _, err = c.Freeze(r)
	require.NoError(t, err, "non-acceptance needs authentication, not fabricated zero billing")
	c.Usage = execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 1}}
	_, _, err = c.Freeze(r)
	require.ErrorIs(t, err, execution.ErrInvalidRunCall)
	r.Kind = execution.RunCallTool
	c = providerReceipt(t, r)
	_, _, err = c.Freeze(r)
	require.ErrorIs(t, err, execution.ErrInvalidRunCall)
}

func TestProviderReceiptRejectsContradictoryLocalObservation(t *testing.T) {
	r := runCallRequest()
	c := providerReceipt(t, r)
	record := execution.RunCallRecord{Request: r, StartedAt: c.ObservedAt.Add(-time.Second)}
	require.NoError(t, c.ValidateObservation(record))
	record.Completion = &execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: c.RequestDigest, ResultDigest: c.ResultDigest}
	record.CompletedAt = c.ObservedAt
	require.NoError(t, c.ValidateObservation(record))
	record.Completion.ResultDigest = evidence.Digest([]byte("different"))
	require.ErrorIs(t, c.ValidateObservation(record), execution.ErrRunCallConflict)
	c.Outcome, c.ResultDigest, c.Usage = execution.ProviderCallNotAccepted, "", execution.ReservationUsage{}
	require.ErrorIs(t, c.ValidateObservation(record), execution.ErrRunCallConflict)
	record.Completion, record.CompletedAt = nil, time.Time{}
	require.NoError(t, c.ValidateObservation(record))
	c.ObservedAt = record.StartedAt.Add(-time.Second)
	require.ErrorIs(t, c.ValidateObservation(record), execution.ErrInvalidRunCall)
}
