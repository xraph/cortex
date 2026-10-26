package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

// AcceptedLessonFixture provides a durable accepted result and lesson proposal.
func AcceptedLessonFixture(t *testing.T, s store.Store) (context.Context, learning.AcceptRequest) {
	t.Helper()
	ctx, request := resultFixture(t, s)
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	return ctx, learning.AcceptRequest{ID: id.New(learning.PrefixLesson), Domain: "reasoning", Result: verification.ResultReadRequest{SchemaVersion: 1, RunID: request.Current.RunID, ResultID: request.ResultID, ReceiptDigest: receipt.Digest}, ExpiresAt: time.Now().UTC().Add(30 * time.Minute), Actor: "operator"}
}

// QualifiedLearning verifies accepted evidence, durable outboxes and local withdrawal.
func QualifiedLearning(t *testing.T, newStore func(*testing.T) store.Store) {
	t.Run("HostGuard", func(t *testing.T) { QualifiedLearningGuard(t, newStore(t)) })
	t.Run("AcceptedLessonAndOutbox", func(t *testing.T) {
		s := newStore(t)
		repository := resultCapability[learning.Repository](t, s)
		ctx, req := AcceptedLessonFixture(t, s)
		publication, err := repository.AcceptLesson(ctx, req)
		require.NoError(t, err)
		require.Equal(t, learning.EvidenceValidated, publication.Payload.EvidenceClass)
		require.Equal(t, "private candidate text", publication.Payload.Statement)
		again, err := repository.AcceptLesson(ctx, req)
		require.NoError(t, err)
		require.Equal(t, publication.PayloadDigest, again.PayloadDigest)
		req.Domain = "different"
		_, err = repository.AcceptLesson(ctx, req)
		require.ErrorIs(t, err, learning.ErrConflict)
		ref := learning.Ref{ID: publication.LessonID, Revision: 1, Digest: publication.PayloadDigest}
		_, err = repository.ResolveLesson(ctx, ref)
		require.NoError(t, err)
		foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "foreign"}}})
		_, err = repository.ResolveLesson(foreign, ref)
		require.Error(t, err)
		claimed, err := repository.ClaimLessonPublication(ctx, ref.ID, 1, "worker")
		require.NoError(t, err)
		_, err = repository.ClaimLessonPublication(ctx, ref.ID, 1, "other")
		require.Error(t, err)
		receipt := learning.Receipt{LessonID: ref.ID, PayloadDigest: ref.Digest, AggregateID: publication.AggregateID, Version: 1, AcknowledgedAt: time.Now().UTC()}
		altered := claimed
		altered.AggregateID = "different"
		forged := receipt
		forged.AggregateID = altered.AggregateID
		require.ErrorIs(t, repository.CompleteLessonPublication(ctx, altered, &forged, ""), learning.ErrConflict)
		require.NoError(t, repository.CompleteLessonPublication(ctx, claimed, &receipt, ""))
		delivered, err := repository.GetLessonPublication(ctx, ref.ID, 1)
		require.NoError(t, err)
		require.Equal(t, &receipt, delivered.Receipt)
		revoked, err := repository.RevokeLesson(ctx, learning.RevokeRequest{ID: ref.ID, ExpectedRevision: 1, Actor: "operator", Reason: "corrected source"})
		require.NoError(t, err)
		require.Equal(t, "revoke", revoked.Operation)
		_, err = repository.ResolveLesson(ctx, ref)
		require.Error(t, err)
	})
	t.Run("CorrectionWithdrawsPriorLessonAtomically", func(t *testing.T) {
		s := newStore(t)
		repository := resultCapability[learning.Repository](t, s)
		ctx, request := AcceptedLessonFixture(t, s)
		original, err := repository.AcceptLesson(ctx, request)
		require.NoError(t, err)
		old := learning.Ref{ID: original.LessonID, Revision: original.Revision, Digest: original.PayloadDigest}
		_, replacement := AcceptedLessonFixture(t, s)
		replacement.Corrects = &old
		corrected, err := repository.AcceptLesson(ctx, replacement)
		require.NoError(t, err)
		require.Equal(t, &old, corrected.Payload.Corrects)
		withdrawn, err := repository.GetLesson(ctx, original.LessonID)
		require.NoError(t, err)
		require.NotZero(t, withdrawn.RevokedAt)
		require.Equal(t, corrected.LessonID, withdrawn.SupersededBy.ID)
		_, err = repository.ResolveLesson(ctx, old)
		require.ErrorIs(t, err, learning.ErrIneligible)
		outbox, err := repository.GetLessonPublication(ctx, original.LessonID, 2)
		require.NoError(t, err)
		require.Equal(t, "revoke", outbox.Operation)
		again, err := repository.AcceptLesson(ctx, replacement)
		require.NoError(t, err)
		require.Equal(t, corrected.PayloadDigest, again.PayloadDigest)
		replacement.ID = id.New(learning.PrefixLesson)
		_, err = repository.AcceptLesson(ctx, replacement)
		require.ErrorIs(t, err, learning.ErrConflict)
		_, err = repository.GetLesson(ctx, replacement.ID)
		require.ErrorIs(t, err, learning.ErrNotFound)
	})
	t.Run("RetentionErasesContentAndReservesIdentity", func(t *testing.T) {
		s := newStore(t)
		repository := resultCapability[learning.Repository](t, s)
		retention := resultCapability[learning.RetentionStore](t, s)
		ctx, request := AcceptedLessonFixture(t, s)
		publication, err := repository.AcceptLesson(ctx, request)
		require.NoError(t, err)
		claim, err := repository.ClaimLessonPublication(ctx, request.ID, 1, "worker")
		require.NoError(t, err)
		deletion := learning.DeletionRequest{ID: request.ID, ExpectedRevision: 1, Actor: "retention-worker", Reason: "retention_expired"}
		tombstone, err := retention.DeleteLesson(ctx, deletion)
		require.NoError(t, err)
		require.Equal(t, publication.PayloadDigest, tombstone.Digest)
		_, err = repository.GetLesson(ctx, request.ID)
		require.ErrorIs(t, err, learning.ErrIneligible)
		_, err = repository.GetLessonPublication(ctx, request.ID, 1)
		require.ErrorIs(t, err, learning.ErrIneligible)
		require.Error(t, repository.CompleteLessonPublication(ctx, claim, nil, "unavailable"))
		_, err = repository.AcceptLesson(ctx, request)
		require.ErrorIs(t, err, learning.ErrIneligible)
		again, err := retention.DeleteLesson(ctx, deletion)
		require.NoError(t, err)
		require.Equal(t, tombstone, again)
		page, err := repository.ListLessonPublications(ctx, "", 100)
		require.NoError(t, err)
		require.Empty(t, page.Items)
	})
	t.Run("RevokedSourceExcluded", func(t *testing.T) {
		s := newStore(t)
		repository := resultCapability[learning.Repository](t, s)
		ctx, req := AcceptedLessonFixture(t, s)
		publication, err := repository.AcceptLesson(ctx, req)
		require.NoError(t, err)
		snapshot, err := resultCapability[verification.ResultSnapshotStore](t, s).ReadResultSnapshot(ctx, req.Result)
		require.NoError(t, err)
		require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), snapshot.Receipt.Request.Current.Candidate, "withdrawn"))
		_, err = repository.ResolveLesson(ctx, learning.Ref{ID: req.ID, Revision: 1, Digest: publication.PayloadDigest})
		require.Error(t, err)
	})
	t.Run("EvaluationCannotLearn", func(t *testing.T) {
		s := newStore(t)
		repository := resultCapability[learning.Repository](t, s)
		ctx, req := AcceptedLessonFixture(t, s)
		current, err := s.GetRun(ctx, req.Result.RunID)
		require.NoError(t, err)
		current.Provenance.Purpose = run.PurposeEvaluation
		require.NoError(t, s.UpdateRun(ctx, current))
		_, err = repository.AcceptLesson(ctx, req)
		require.Error(t, err)
	})
	for _, source := range []string{"recalled", "sources", "source_reads", "unattributed_history"} {
		t.Run("RecallCannotAttest/"+source, func(t *testing.T) {
			s := newStore(t)
			repository := resultCapability[learning.Repository](t, s)
			ctx, req := AcceptedLessonFixture(t, s)
			publication, err := repository.AcceptLesson(ctx, req)
			require.NoError(t, err)
			current, err := s.GetRun(ctx, req.Result.RunID)
			require.NoError(t, err)
			refs := []run.Revision{{Name: "lesson:" + publication.LessonID.String(), Revision: "1", Digest: publication.PayloadDigest}}
			switch source {
			case "recalled":
				current.Provenance.RecalledLessons = refs
			case "sources":
				current.Provenance.Sources = refs
			case "source_reads":
				current.Provenance.SourceReads = refs
			case "unattributed_history":
				current.Provenance.UnattributedHistory = true
			}
			require.NoError(t, s.UpdateRun(ctx, current))
			_, err = repository.ResolveLesson(ctx, learning.Ref{ID: req.ID, Revision: 1, Digest: publication.PayloadDigest})
			require.ErrorIs(t, err, learning.ErrIneligible)
			req.ID = id.New(learning.PrefixLesson)
			_, err = repository.AcceptLesson(ctx, req)
			require.ErrorIs(t, err, learning.ErrIneligible)
		})
	}
}
