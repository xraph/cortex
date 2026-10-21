package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func testEvidenceDeletionFence(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	es, ok := s.(evidence.Storage)
	require.True(t, ok)
	audit, ok := s.(evidence.TombstoneReader)
	require.True(t, ok)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "capture-recovery"}}}
	ctx := evidence.WithDeletionActor(cortex.WithScope(t.Context(), scope), "service:capture-recovery")
	artifact := func() evidence.Artifact {
		now := time.Now().UTC()
		a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "host", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("delayed upload")})
		require.NoError(t, err)
		return a
	}
	t.Run("AbsentIdentityBlocksDelayedUpload", func(t *testing.T) {
		a := artifact()
		require.NoError(t, es.TombstoneEvidence(ctx, a.Ref, "abandoned capture"))
		first, err := audit.GetEvidenceTombstone(ctx, a.Ref)
		require.NoError(t, err)
		require.Equal(t, a.Ref, first.Ref)
		require.Equal(t, "service:capture-recovery", first.Actor)
		require.Equal(t, "abandoned capture", first.Reason)
		require.NoError(t, es.TombstoneEvidence(ctx, a.Ref, "abandoned capture"))
		again, err := audit.GetEvidenceTombstone(ctx, a.Ref)
		require.NoError(t, err)
		require.Equal(t, first, again)
		require.ErrorIs(t, es.SaveEvidence(ctx, a), evidence.ErrDeleted)
		_, err = es.LoadEvidence(ctx, a.Ref)
		require.ErrorIs(t, err, evidence.ErrDeleted)
		require.ErrorIs(t, es.TombstoneEvidence(ctx, a.Ref, "changed reason"), evidence.ErrConflict)
		require.ErrorIs(t, es.TombstoneEvidence(evidence.WithDeletionActor(ctx, "different"), a.Ref, "abandoned capture"), evidence.ErrConflict)
		changed := a.Ref
		changed.Version = "different"
		require.ErrorIs(t, es.TombstoneEvidence(ctx, changed, "abandoned capture"), evidence.ErrDigestMismatch)
	})
	t.Run("InvalidAuthorityCannotReserveIdentity", func(t *testing.T) {
		for _, mode := range []string{"scope", "actor", "cancelled", "reason"} {
			a := artifact()
			operation := ctx
			reason := "cleanup"
			switch mode {
			case "scope":
				operation = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			case "actor":
				operation = evidence.WithDeletionActor(ctx, "")
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				operation = cancelled
			case "reason":
				reason = ""
			}
			require.Error(t, es.TombstoneEvidence(operation, a.Ref, reason))
			require.NoError(t, es.SaveEvidence(ctx, a), mode)
		}
	})
	t.Run("OversizedReferenceCannotReserveIdentity", func(t *testing.T) {
		a := artifact()
		oversized := a.Ref
		oversized.SourceURI = strings.Repeat("x", 3<<20)
		require.ErrorIs(t, es.TombstoneEvidence(ctx, oversized, "cleanup"), evidence.ErrTooLarge)
		require.NoError(t, es.SaveEvidence(ctx, a))
	})
	t.Run("UploadRaceCannotResurrectContent", func(t *testing.T) {
		for range 12 {
			a := artifact()
			start := make(chan struct{})
			saved, deleted := make(chan error, 1), make(chan error, 1)
			go func() { <-start; saved <- es.SaveEvidence(ctx, a) }()
			go func() { <-start; deleted <- es.TombstoneEvidence(ctx, a.Ref, "abandoned capture") }()
			close(start)
			saveErr, deleteErr := <-saved, <-deleted
			require.NoError(t, deleteErr)
			require.True(t, saveErr == nil || errors.Is(saveErr, evidence.ErrDeleted), "unexpected upload result: %v", saveErr)
			_, err := audit.GetEvidenceTombstone(ctx, a.Ref)
			require.NoError(t, err)
			require.ErrorIs(t, es.SaveEvidence(ctx, a), evidence.ErrDeleted)
		}
	})
}
