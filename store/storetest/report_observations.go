package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

// ReportObservations checks dynamic supervisor evidence at save, commit and read.
func ReportObservations(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, phase := range []string{"before commit", "after commit"} {
		t.Run(phase, func(t *testing.T) {
			s := newStore(t)
			ctx, request := resultFixture(t, s)
			reports := resultCapability[verification.Store](t, s)
			artifacts := resultCapability[evidence.Storage](t, s)
			report, err := reports.GetReport(ctx, request.ReportIDs[0])
			require.NoError(t, err)
			artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: request.Current.Scope, Version: "1", MediaType: "application/json", Producer: "protected-supervisor", Sensitivity: evidence.SensitivityRestricted, CreatedAt: time.Now().UTC().Add(-time.Second), RetainUntil: report.ExpiresAt.Add(time.Minute)}, Data: []byte(`{"observation":"private"}`)})
			require.NoError(t, err)
			report.ID = id.New(verification.PrefixReport)
			report.Outcomes[0].ObservationRefs = []evidence.Ref{artifact.Ref}
			report, err = verification.SealReport(report)
			require.NoError(t, err)
			require.ErrorIs(t, reports.SaveReport(ctx, report), evidence.ErrNotFound)
			require.NoError(t, artifacts.SaveEvidence(ctx, artifact))
			require.NoError(t, reports.SaveReport(ctx, report))
			request.ReportIDs = []id.ID{report.ID}
			commits := resultCapability[verification.CommitStore](t, s)
			if phase == "before commit" {
				require.NoError(t, artifacts.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), artifact.Ref, "revoked observation"))
				_, err = commits.CommitVerifiedResult(ctx, request)
				require.ErrorIs(t, err, evidence.ErrDeleted)
				_, err = commits.GetCommittedResult(ctx, request.ResultID)
				require.ErrorIs(t, err, verification.ErrNotFound)
			} else {
				receipt, commitErr := commits.CommitVerifiedResult(ctx, request)
				require.NoError(t, commitErr)
				reader := resultCapability[verification.ResultSnapshotStore](t, s)
				read := verification.ResultReadRequest{SchemaVersion: 1, RunID: request.Current.RunID, ResultID: request.ResultID, ReceiptDigest: receipt.Digest}
				snapshot, readErr := reader.ReadResultSnapshot(ctx, read)
				require.NoError(t, readErr)
				require.Len(t, snapshot.Reports[0].Outcomes[0].ObservationRefs, 1)
				require.NoError(t, artifacts.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), artifact.Ref, "revoked observation"))
				snapshot, readErr = reader.ReadResultSnapshot(ctx, read)
				require.ErrorIs(t, readErr, evidence.ErrDeleted)
				require.Empty(t, snapshot.Output)
			}
			historical, err := reports.GetReport(ctx, report.ID)
			require.NoError(t, err)
			require.Equal(t, report.Digest, historical.Digest)
			report.ID = id.New(verification.PrefixReport)
			report, err = verification.SealReport(report)
			require.NoError(t, err)
			require.ErrorIs(t, reports.SaveReport(ctx, report), evidence.ErrDeleted)
		})
	}
}
