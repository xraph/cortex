package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

type failingRunDriver struct {
	assurance.Driver
	transaction assurance.RunDriver
	failure     error
}

func (d failingRunDriver) TransactRun(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.RunTransaction) error) error {
	return d.transaction.TransactRun(ctx, runID, func(txctx context.Context, tx assurance.RunTransaction) error {
		if err := callback(txctx, tx); err != nil {
			return err
		}
		return d.failure
	})
}

// VerifiedResultRollback injects failure after both writes in a real transaction.
// It also proves a retry can succeed without an orphan result or advanced run.
func VerifiedResultRollback(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	failure := errors.New("injected after result and run writes")
	repository := assurance.New(failingRunDriver{Driver: driver, transaction: transaction, failure: failure})
	ctx, req := resultFixture(t, s)
	before, err := s.GetRun(ctx, req.Current.RunID)
	require.NoError(t, err)
	receipt, err := repository.CommitVerifiedResult(ctx, req)
	require.ErrorIs(t, err, failure)
	require.Zero(t, receipt)
	after, err := s.GetRun(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	watermark, err := resultCapability[event.Store](t, s).ReadEventWatermark(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Zero(t, watermark.LastSequence)
	_, err = repository.GetCommittedResult(ctx, req.ResultID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	receipt, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, req)
	require.NoError(t, err)
	require.NoError(t, receipt.Validate())
}

type revokeBeforeEvidence struct {
	assurance.RunTransaction
	revoke func() error
}

func (d revokeBeforeEvidence) Select(ctx context.Context, query assurance.Query) ([]assurance.Record, error) {
	if query.Kind == assurance.KindEvidence {
		if err := d.revoke(); err != nil {
			return nil, err
		}
	}
	return d.RunTransaction.Select(ctx, query)
}

type revokingRunDriver struct {
	assurance.Driver
	transaction assurance.RunDriver
	revoke      func() error
}

func (d revokingRunDriver) TransactRun(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.RunTransaction) error) error {
	return d.transaction.TransactRun(ctx, runID, func(txctx context.Context, tx assurance.RunTransaction) error {
		return callback(txctx, revokeBeforeEvidence{RunTransaction: tx, revoke: d.revoke})
	})
}

// VerifiedResultRevocation checks a deletion after the initial transaction reads.
// Use this for row-locking or snapshot backends. SQLite holds a database write
// lock from the start, so a competing delete cannot reach this window there.
func VerifiedResultRevocation(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	ctx, req := resultFixture(t, s)
	revoked := false
	repository := assurance.New(revokingRunDriver{Driver: driver, transaction: transaction, revoke: func() error {
		if revoked {
			return nil
		}
		revoked = true
		return resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), req.Current.Candidate, "revoked during validation")
	}})
	receipt, err := repository.CommitVerifiedResult(ctx, req)
	require.ErrorIs(t, err, evidence.ErrDeleted)
	require.Zero(t, receipt)
	require.True(t, revoked)
	current, err := s.GetRun(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, req.ExpectedRunRevision, current.Revision)
	require.Equal(t, verification.StatusPending, current.VerificationStatus)
	_, err = repository.GetCommittedResult(ctx, req.ResultID)
	require.ErrorIs(t, err, verification.ErrNotFound)
}

// VerifiedResultRestart checks that reopening storage preserves the original
// receipt and event ID and that a lost acknowledgement can be retried safely.
func VerifiedResultRestart(t *testing.T, s store.Store, reopen func() store.Store) {
	t.Helper()
	ctx, req := resultFixture(t, s)
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, req)
	require.NoError(t, err)
	before := readLifecycle(ctx, t, s, req.Current.RunID)
	restored := reopen()
	cs := resultCapability[verification.CommitStore](t, restored)
	got, err := cs.GetCommittedResult(ctx, req.ResultID)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	retry, err := cs.CommitVerifiedResult(ctx, req)
	require.NoError(t, err)
	require.Equal(t, receipt, retry)
	current, err := restored.GetRun(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, receipt.RunRevision, current.Revision)
	page, err := resultCapability[event.Store](t, restored).ReadPublicEvents(ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: req.Current.RunID}, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, before, page)
	requireLifecycle(t, page, event.VerificationPassed, event.ArtifactCreated, event.ResultCommitted)
	require.Equal(t, receipt.Outbox.EventID, page.Events[2].ID)
	require.Equal(t, uint64(3), page.Events[2].Sequence)
	snapshot, err := resultCapability[verification.ResultSnapshotStore](t, restored).ReadResultSnapshot(ctx, verification.ResultReadRequest{SchemaVersion: 1, RunID: req.Current.RunID, ResultID: req.ResultID, ReceiptDigest: receipt.Digest})
	require.NoError(t, err)
	require.Equal(t, "private candidate text", snapshot.Output)
}
