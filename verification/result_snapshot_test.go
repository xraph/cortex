package verification_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func resultSnapshotFixture(t *testing.T) (verification.ResultSnapshot, verification.ResultReadRequest, time.Time) {
	t.Helper()
	contract, report, now := fixture(t)
	request := verification.CommitRequest{SchemaVersion: 1, ResultID: id.New(verification.PrefixResult), ExpectedRunRevision: 1, ContractID: contract.ID, ReportIDs: []id.ID{report.ID}, Current: report.Binding}
	request, digest, err := request.Freeze()
	require.NoError(t, err)
	bindingDigest, err := request.Current.Digest()
	require.NoError(t, err)
	receipt, err := verification.SealCommitReceipt(verification.CommitReceipt{SchemaVersion: 1, Request: request, RequestDigest: digest, RunRevision: 2, ReportDigests: []string{report.Digest}, ObservedAt: now, Outbox: verification.CommitOutbox{EventID: id.New(verification.PrefixCommitEvent), Type: "result_committed", RunID: request.Current.RunID, ResultID: request.ResultID, BindingDigest: bindingDigest}})
	require.NoError(t, err)
	read := verification.ResultReadRequest{SchemaVersion: 1, RunID: request.Current.RunID, ResultID: request.ResultID, ReceiptDigest: receipt.Digest}
	return verification.ResultSnapshot{SchemaVersion: 1, Receipt: receipt, Contract: contract, Reports: []verification.Report{report}, ObservedAt: now, Output: "result"}, read, now
}

func TestResultReadRequestRequiresExactIdentity(t *testing.T) {
	_, valid, _ := resultSnapshotFixture(t)
	require.NoError(t, valid.Validate())
	for name, mutate := range map[string]func(*verification.ResultReadRequest){
		"schema":              func(r *verification.ResultReadRequest) { r.SchemaVersion++ },
		"missing run":         func(r *verification.ResultReadRequest) { r.RunID = id.AgentRunID{} },
		"wrong run prefix":    func(r *verification.ResultReadRequest) { r.RunID = id.NewAgentID() },
		"missing result":      func(r *verification.ResultReadRequest) { r.ResultID = id.ID{} },
		"wrong result prefix": func(r *verification.ResultReadRequest) { r.ResultID = id.New(verification.PrefixReport) },
		"missing digest":      func(r *verification.ResultReadRequest) { r.ReceiptDigest = "" },
		"invalid digest":      func(r *verification.ResultReadRequest) { r.ReceiptDigest = "not a digest" },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			mutate(&request)
			require.ErrorIs(t, request.Validate(), verification.ErrInvalid)
		})
	}
}

func TestResultSnapshotRejectsChangedMaterial(t *testing.T) {
	for name, mutate := range map[string]func(*verification.ResultSnapshot, *verification.ResultReadRequest, *time.Time){
		"schema": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) {
			s.SchemaVersion++
		},
		"receipt": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) {
			s.Receipt.RunRevision++
		},
		"request digest": func(_ *verification.ResultSnapshot, r *verification.ResultReadRequest, _ *time.Time) {
			r.ReceiptDigest = evidence.Digest([]byte("other"))
		},
		"request run": func(_ *verification.ResultSnapshot, r *verification.ResultReadRequest, _ *time.Time) {
			r.RunID = id.NewAgentRunID()
		},
		"request result": func(_ *verification.ResultSnapshot, r *verification.ResultReadRequest, _ *time.Time) {
			r.ResultID = id.New(verification.PrefixResult)
		},
		"contract": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) {
			s.Contract.Objective = "changed"
		},
		"report body": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) {
			s.Reports[0].Outcomes[0].Status = verification.StatusFailed
		},
		"report missing": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) { s.Reports = nil },
		"report duplicated": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) {
			s.Reports = append(s.Reports, s.Reports[0])
		},
		"output same length": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) {
			s.Output = "forged"
		},
		"output missing": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) { s.Output = "" },
		"observation missing": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) {
			s.ObservedAt = time.Time{}
		},
		"observation before commit": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, _ *time.Time) {
			s.ObservedAt = s.ObservedAt.Add(-time.Nanosecond)
		},
		"clock before observation": func(_ *verification.ResultSnapshot, _ *verification.ResultReadRequest, at *time.Time) {
			*at = at.Add(-time.Nanosecond)
		},
		"expired": func(s *verification.ResultSnapshot, _ *verification.ResultReadRequest, at *time.Time) {
			*at = s.Reports[0].ExpiresAt
		},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot, request, at := resultSnapshotFixture(t)
			require.NoError(t, snapshot.Validate(request, at))
			mutate(&snapshot, &request, &at)
			require.Error(t, snapshot.Validate(request, at))
		})
	}
}

func TestResultSnapshotJSONOmitsPrivateOutput(t *testing.T) {
	snapshot, request, now := resultSnapshotFixture(t)
	body, err := json.Marshal(snapshot)
	require.NoError(t, err)
	var decoded verification.ResultSnapshot
	require.NoError(t, json.Unmarshal(body, &decoded))
	require.Empty(t, decoded.Output)
	require.Error(t, decoded.Validate(request, now))
}
