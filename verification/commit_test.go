package verification_test

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func TestCommitRequestFreeze(t *testing.T) {
	contract, report, _ := fixture(t)
	req := verification.CommitRequest{SchemaVersion: 1, ResultID: id.New(verification.PrefixResult), ExpectedRunRevision: 1, ContractID: contract.ID, ReportIDs: []id.ID{report.ID}, Current: report.Binding}
	frozen, digest, err := req.Freeze()
	require.NoError(t, err)
	req.Current.Scope.Levels[0].Value = "mutated"
	req.ReportIDs[0] = id.New(verification.PrefixReport)
	_, again, err := frozen.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest, again)
	for _, mutate := range []func(*verification.CommitRequest){
		func(r *verification.CommitRequest) { r.SchemaVersion = 2 },
		func(r *verification.CommitRequest) { r.ExpectedRunRevision = 0 },
		func(r *verification.CommitRequest) { r.ExpectedRunRevision = math.MaxInt64 },
		func(r *verification.CommitRequest) { r.ResultID = id.New(verification.PrefixReport) },
		func(r *verification.CommitRequest) { r.ReportIDs = nil },
		func(r *verification.CommitRequest) { r.ReportIDs = []id.ID{report.ID, report.ID} },
		func(r *verification.CommitRequest) { r.Current.Context.AuthorityRevision = strings.Repeat("x", 1<<20) },
	} {
		changed, _, freezeErr := frozen.Freeze()
		require.NoError(t, freezeErr)
		mutate(&changed)
		_, _, freezeErr = changed.Freeze()
		require.Error(t, freezeErr)
	}
}

func TestCommitReceiptIntegrity(t *testing.T) {
	contract, report, now := fixture(t)
	req := verification.CommitRequest{SchemaVersion: 1, ResultID: id.New(verification.PrefixResult), ExpectedRunRevision: 1, ContractID: contract.ID, ReportIDs: []id.ID{report.ID}, Current: report.Binding}
	req, requestDigest, err := req.Freeze()
	require.NoError(t, err)
	bindingDigest, err := req.Current.Digest()
	require.NoError(t, err)
	receipt, err := verification.SealCommitReceipt(verification.CommitReceipt{SchemaVersion: 1, Request: req, RequestDigest: requestDigest, RunRevision: 2, ReportDigests: []string{report.Digest}, ObservedAt: now, Outbox: verification.CommitOutbox{EventID: id.New(verification.PrefixCommitEvent), Type: "result_committed", RunID: req.Current.RunID, ResultID: req.ResultID, BindingDigest: bindingDigest}})
	require.NoError(t, err)
	require.NoError(t, receipt.Validate())
	for _, mutate := range []func(*verification.CommitReceipt){
		func(r *verification.CommitReceipt) { r.RunRevision++ },
		func(r *verification.CommitReceipt) { r.Outbox.ResultID = id.New(verification.PrefixResult) },
		func(r *verification.CommitReceipt) { r.Outbox.Type = "candidate" },
		func(r *verification.CommitReceipt) { r.Outbox.EventID = id.New(verification.PrefixCommitEvent) },
		func(r *verification.CommitReceipt) { r.Digest = requestDigest },
	} {
		changed := receipt
		mutate(&changed)
		require.Error(t, changed.Validate())
	}
}
