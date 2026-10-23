package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func testResultSnapshot(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	reader := resultCapability[verification.ResultSnapshotStore](t, s)
	ctx, request := resultFixture(t, s)
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	read := verification.ResultReadRequest{SchemaVersion: 1, RunID: request.Current.RunID, ResultID: request.ResultID, ReceiptDigest: receipt.Digest}
	before, err := s.GetRun(ctx, read.RunID)
	require.NoError(t, err)
	events := readLifecycle(ctx, t, s, read.RunID)
	snapshot, err := reader.ReadResultSnapshot(ctx, read)
	require.NoError(t, err)
	require.NoError(t, snapshot.Validate(read, time.Now().UTC()))
	require.Equal(t, "private candidate text", snapshot.Output)
	require.Equal(t, receipt, snapshot.Receipt)
	wire, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(wire), "private candidate text")
	require.Error(t, snapshot.Validate(read, snapshot.Reports[0].ExpiresAt))
	changed := snapshot
	changed.Output = "tampered"
	require.Error(t, changed.Validate(read, time.Now().UTC()))
	snapshot.Reports[0].Outcomes[0].Reason = "mutated caller copy"
	snapshot.Receipt.ReportDigests[0] = evidence.Digest([]byte("changed"))
	next, err := reader.ReadResultSnapshot(ctx, read)
	require.NoError(t, err)
	require.NoError(t, next.Validate(read, time.Now().UTC()))
	after, err := s.GetRun(ctx, read.RunID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, events, readLifecycle(ctx, t, s, read.RunID))
	// Metadata retention never deletes the underlying result, but a tombstone does.
	require.NoError(t, resultCapability[event.Store](t, s).PrunePublicEvents(ctx, events.Next))
	_, err = reader.ReadResultSnapshot(ctx, read)
	require.NoError(t, err)
	require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), request.Current.Candidate, "revoked"))
	denied, err := reader.ReadResultSnapshot(ctx, read)
	require.ErrorIs(t, err, evidence.ErrDeleted)
	require.Zero(t, denied)
}

func testResultSnapshotDenials(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"pending", "scope", "no scope", "cancelled", "digest", "run", "revision", "output", "state"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			reader := resultCapability[verification.ResultSnapshotStore](t, s)
			ctx, request := resultFixture(t, s)
			read := verification.ResultReadRequest{SchemaVersion: 1, RunID: request.Current.RunID, ResultID: request.ResultID, ReceiptDigest: evidence.Digest([]byte("missing"))}
			if mode != "pending" {
				receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
				require.NoError(t, err)
				read.ReceiptDigest = receipt.Digest
			}
			switch mode {
			case "scope":
				ctx = ctxWithScope("other")
			case "no scope":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "digest":
				read.ReceiptDigest = evidence.Digest([]byte("wrong"))
			case "run":
				read.RunID = id.NewAgentRunID()
			case "revision", "output", "state":
				current, err := s.GetRun(ctx, read.RunID)
				require.NoError(t, err)
				if mode == "output" {
					current.Output = "tampered"
				}
				if mode == "state" {
					current.VerificationStatus = verification.StatusFailed
				}
				require.NoError(t, s.UpdateRun(ctx, current))
			}
			snapshot, err := reader.ReadResultSnapshot(ctx, read)
			require.Error(t, err)
			require.Zero(t, snapshot)
			if mode == "no scope" {
				require.ErrorIs(t, err, cortex.ErrNoScope)
			}
		})
	}
}

func testResultSnapshotPrivateEvidence(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, kind := range []string{"inputs", "transcript"} {
		t.Run(kind, func(t *testing.T) {
			s := newStore(t)
			ctx, intent, capture := transcriptCaptureFixture(t, s)
			require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
			job, err := resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, capture)
			require.NoError(t, err)
			job = settlePrivateCandidate(ctx, t, s, job, verification.StatusPassed)
			request := jobCommitRequest(job)
			request.ResultID = intent.ResultID
			receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
			require.NoError(t, err)
			read := verification.ResultReadRequest{SchemaVersion: 1, RunID: request.Current.RunID, ResultID: request.ResultID, ReceiptDigest: receipt.Digest}
			reader := resultCapability[verification.ResultSnapshotStore](t, s)
			snapshot, err := reader.ReadResultSnapshot(ctx, read)
			require.NoError(t, err)
			require.Equal(t, capture.Output, snapshot.Output)
			ref := request.Current.Context.ExecutionInputs
			if kind == "transcript" {
				ref = request.Current.Context.ExecutionTranscript
			}
			require.NotNil(t, ref)
			require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), *ref, "revoked supporting evidence"))
			snapshot, err = reader.ReadResultSnapshot(ctx, read)
			require.ErrorIs(t, err, evidence.ErrDeleted)
			require.Zero(t, snapshot)
		})
	}
}

type snapshotDelayedDriver struct {
	assurance.Driver
	transaction assurance.RunDriver
	after       func()
}

func (d snapshotDelayedDriver) TransactRun(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.RunTransaction) error) error {
	err := d.transaction.TransactRun(ctx, runID, callback)
	if err == nil {
		d.after()
	}
	return err
}

// ResultSnapshotBoundaries checks expiry and failures at transaction boundaries.
func ResultSnapshotBoundaries(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	ctx, request := resultFixture(t, s)
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	read := verification.ResultReadRequest{SchemaVersion: 1, RunID: request.Current.RunID, ResultID: request.ResultID, ReceiptDigest: receipt.Digest}
	clock := &jobClock{at: time.Now().UTC()}
	repository := assurance.New(driver).WithClock(clock)
	snapshot, err := repository.ReadResultSnapshot(ctx, read)
	require.NoError(t, err)
	require.NoError(t, snapshot.Validate(read, clock.Now()))
	clock.advance(2 * time.Hour)
	snapshot, err = repository.ReadResultSnapshot(ctx, read)
	require.ErrorIs(t, err, verification.ErrConflict)
	require.Zero(t, snapshot)
	// Receipt retries remain historical and cannot be used as a freshness check.
	historical, err := repository.CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	require.Equal(t, receipt, historical)
	clock.advance(-2 * time.Hour)
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	failure := errors.New("transaction acknowledgement failed")
	failed := assurance.New(failingRunDriver{Driver: driver, transaction: transaction, failure: failure})
	snapshot, err = failed.ReadResultSnapshot(ctx, read)
	require.ErrorIs(t, err, failure)
	require.Zero(t, snapshot)
	cancelledCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cancelled := assurance.New(snapshotDelayedDriver{Driver: driver, transaction: transaction, after: cancel}).WithClock(clock)
	snapshot, err = cancelled.ReadResultSnapshot(cancelledCtx, read)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, snapshot)
	delayed := assurance.New(snapshotDelayedDriver{Driver: driver, transaction: transaction, after: func() { clock.advance(2 * time.Hour) }}).WithClock(clock)
	snapshot, err = delayed.ReadResultSnapshot(ctx, read)
	require.ErrorIs(t, err, verification.ErrConflict)
	require.Zero(t, snapshot)
	unsupported := assurance.New(struct{ assurance.Driver }{driver})
	snapshot, err = unsupported.ReadResultSnapshot(ctx, read)
	require.ErrorIs(t, err, verification.ErrCommitUnavailable)
	require.Zero(t, snapshot)
}
