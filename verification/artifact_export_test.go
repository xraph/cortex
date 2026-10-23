package verification_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

type exportAuth func(context.Context, verification.ArtifactExportRequest) error

func (f exportAuth) AuthorizeArtifactExport(ctx context.Context, r verification.ArtifactExportRequest) error {
	return f(ctx, r)
}

type exportFence func(context.Context, verification.ArtifactExportRequest, verification.Binding, func(verification.Binding) error) error

func (f exportFence) WithArtifactExportFence(ctx context.Context, r verification.ArtifactExportRequest, b verification.Binding, next func(verification.Binding) error) error {
	return f(ctx, r, b, next)
}

type exportScreen func(context.Context, verification.ArtifactExportScreenRequest) (verification.ArtifactExportScreenResult, error)

func (f exportScreen) ScreenArtifactExport(ctx context.Context, r verification.ArtifactExportScreenRequest) (verification.ArtifactExportScreenResult, error) {
	return f(ctx, r)
}

type exportSink func(context.Context, verification.ArtifactExportRequest, []byte) (verification.ArtifactExportReceipt, error)

func (f exportSink) ExportArtifact(ctx context.Context, r verification.ArtifactExportRequest, b []byte) (verification.ArtifactExportReceipt, error) {
	return f(ctx, r, b)
}

type exportFixtureState struct {
	denied   bool
	held     bool
	writes   int
	reads    int
	snapshot verification.ResultSnapshot
}

func artifactExportFixture(t *testing.T) (verification.ArtifactExporterConfig, verification.ArtifactExportRequest, *exportFixtureState) {
	t.Helper()
	snapshot, read, now := resultSnapshotFixture(t)
	state := &exportFixtureState{snapshot: snapshot}
	r := verification.ArtifactExportRequest{SchemaVersion: 1, ID: id.New(verification.PrefixExport), Result: verification.ResultContentRequest{SchemaVersion: 1, RunID: read.RunID, ResultID: read.ResultID}, Artifact: snapshot.Receipt.Request.Current.Candidate, Actor: "user:exporter", ApprovalRef: "approval:1", Destination: "review-files", DestinationRevision: "1"}
	repo := &evaluationMemory{data: map[id.ID]evidence.Artifact{r.Artifact.ID: {Ref: r.Artifact, Data: []byte(snapshot.Output)}}}
	cfg := verification.ArtifactExporterConfig{MaxConcurrent: 1, Clock: &resultClock{now: now}, Repository: repo, Store: resultReaderStore{receipt: func(context.Context, id.ID) (verification.CommitReceipt, error) { return state.snapshot.Receipt, nil }, snapshot: func(context.Context, verification.ResultReadRequest) (verification.ResultSnapshot, error) {
		state.reads++
		require.True(t, state.held)
		return state.snapshot, nil
	}}, Authorizer: exportAuth(func(context.Context, verification.ArtifactExportRequest) error {
		if state.denied {
			return errors.New("revoked")
		}
		return nil
	}), Fence: exportFence(func(_ context.Context, _ verification.ArtifactExportRequest, b verification.Binding, next func(verification.Binding) error) error {
		state.held = true
		defer func() { state.held = false }()
		return next(b)
	}), Screener: exportScreen(func(_ context.Context, r verification.ArtifactExportScreenRequest) (verification.ArtifactExportScreenResult, error) {
		require.True(t, state.held)
		return verification.ArtifactExportScreenResult{Approved: true, SHA256: evidence.Digest(r.Data)}, nil
	}), Sink: exportSink(func(_ context.Context, r verification.ArtifactExportRequest, data []byte) (verification.ArtifactExportReceipt, error) {
		require.True(t, state.held)
		state.writes++
		require.Equal(t, "result", string(data))
		_, digest, err := r.Freeze()
		return verification.ArtifactExportReceipt{SchemaVersion: 1, ID: r.ID, RequestDigest: digest, SHA256: evidence.Digest(data), Bytes: int64(len(data))}, err
	})}
	return cfg, r, state
}
func TestArtifactExportRequiresCurrentApproval(t *testing.T) {
	cfg, r, state := artifactExportFixture(t)
	state.denied = true
	exporter, err := verification.NewArtifactExporter(cfg)
	require.NoError(t, err)
	_, err = exporter.Export(cortex.WithScope(t.Context(), r.Artifact.Scope), r)
	require.Error(t, err)
	require.Zero(t, state.writes)
	require.Zero(t, state.reads)
}
func TestArtifactExportWritesOnlyScreenedExactBytesUnderFence(t *testing.T) {
	cfg, r, state := artifactExportFixture(t)
	exporter, err := verification.NewArtifactExporter(cfg)
	require.NoError(t, err)
	receipt, err := exporter.Export(cortex.WithScope(t.Context(), r.Artifact.Scope), r)
	require.NoError(t, err)
	require.NoError(t, receipt.Validate(r))
	require.Equal(t, 1, state.writes)
	require.GreaterOrEqual(t, state.reads, 2)
}
func TestArtifactExportRejectsRevocationAndUnsafeContentBeforeWrite(t *testing.T) {
	for _, mode := range []string{"revoke", "deleted", "scanner outage", "blocked", "rewritten digest", "mutated bytes", "swapped evidence", "foreign artifact", "destination", "actor"} {
		t.Run(mode, func(t *testing.T) {
			cfg, r, state := artifactExportFixture(t)
			original := cfg.Screener
			cfg.Screener = exportScreen(func(ctx context.Context, input verification.ArtifactExportScreenRequest) (verification.ArtifactExportScreenResult, error) {
				result, err := original.ScreenArtifactExport(ctx, input)
				switch mode {
				case "revoke":
					state.denied = true
				case "deleted":
					state.snapshot.Output = "deleted"
				case "scanner outage":
					return result, errors.New("scanner unavailable")
				case "blocked":
					result.Approved = false
				case "rewritten digest":
					result.SHA256 = evidence.Digest([]byte("redaction"))
				case "mutated bytes":
					copy(input.Data, []byte("forged"))
				}
				return result, err
			})
			switch mode {
			case "swapped evidence":
				repo := cfg.Repository.(*evaluationMemory)
				changed := repo.data[r.Artifact.ID]
				changed.Data = []byte("forged")
				repo.data[r.Artifact.ID] = changed
			case "foreign artifact":
				r.Artifact.Version = "other"
			case "destination":
				r.Destination = ""
			case "actor":
				r.Actor = ""
			}
			exporter, err := verification.NewArtifactExporter(cfg)
			require.NoError(t, err)
			_, err = exporter.Export(cortex.WithScope(t.Context(), r.Artifact.Scope), r)
			if mode == "mutated bytes" {
				require.NoError(t, err)
				require.Equal(t, 1, state.writes)
			} else {
				require.Error(t, err)
				require.Zero(t, state.writes)
			}
		})
	}
}
func TestArtifactExportDeliveryErrorIsUncertain(t *testing.T) {
	cfg, r, state := artifactExportFixture(t)
	cfg.Sink = exportSink(func(context.Context, verification.ArtifactExportRequest, []byte) (verification.ArtifactExportReceipt, error) {
		state.writes++
		return verification.ArtifactExportReceipt{}, errors.New("lost acknowledgement")
	})
	exporter, err := verification.NewArtifactExporter(cfg)
	require.NoError(t, err)
	_, err = exporter.Export(cortex.WithScope(t.Context(), r.Artifact.Scope), r)
	require.ErrorIs(t, err, verification.ErrExportUncertain)
	require.Equal(t, 1, state.writes)
}

func TestArtifactExportPreservesAuthorizationCancellation(t *testing.T) {
	cfg, r, state := artifactExportFixture(t)
	cfg.Authorizer = exportAuth(func(context.Context, verification.ArtifactExportRequest) error { return context.Canceled })
	exporter, err := verification.NewArtifactExporter(cfg)
	require.NoError(t, err)
	_, err = exporter.Export(cortex.WithScope(t.Context(), r.Artifact.Scope), r)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, state.writes)
}
func TestArtifactExportKeepsPostDeliveryFailuresUncertain(t *testing.T) {
	for _, mode := range []string{"receipt", "fence error", "double fence", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			cfg, r, state := artifactExportFixture(t)
			ctx, cancel := context.WithCancel(cortex.WithScope(t.Context(), r.Artifact.Scope))
			defer cancel()
			original := cfg.Sink
			cfg.Sink = exportSink(func(ctx context.Context, r verification.ArtifactExportRequest, data []byte) (verification.ArtifactExportReceipt, error) {
				receipt, err := original.ExportArtifact(ctx, r, data)
				if mode == "receipt" {
					receipt.RequestDigest = evidence.Digest([]byte("other"))
				}
				if mode == "cancelled" {
					cancel()
				}
				return receipt, err
			})
			if mode == "fence error" || mode == "double fence" {
				cfg.Fence = exportFence(func(_ context.Context, _ verification.ArtifactExportRequest, b verification.Binding, next func(verification.Binding) error) error {
					state.held = true
					defer func() { state.held = false }()
					err := next(b)
					if mode == "double fence" {
						return next(b)
					}
					return errors.Join(err, errors.New("fence acknowledgement lost"))
				})
			}
			exporter, err := verification.NewArtifactExporter(cfg)
			require.NoError(t, err)
			_, err = exporter.Export(ctx, r)
			require.ErrorIs(t, err, verification.ErrExportUncertain)
			require.Equal(t, 1, state.writes)
		})
	}
}
func TestArtifactExporterRequiresEveryPortAndBoundedAdmission(t *testing.T) {
	for _, mode := range []string{"store", "repository", "authorizer", "fence", "screener", "sink", "clock", "slots"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, _ := artifactExportFixture(t)
			switch mode {
			case "store":
				cfg.Store = nil
			case "repository":
				cfg.Repository = nil
			case "authorizer":
				cfg.Authorizer = nil
			case "fence":
				cfg.Fence = nil
			case "screener":
				cfg.Screener = nil
			case "sink":
				cfg.Sink = nil
			case "clock":
				cfg.Clock = nil
			case "slots":
				cfg.MaxConcurrent = 0
			}
			_, err := verification.NewArtifactExporter(cfg)
			require.ErrorIs(t, err, verification.ErrInvalid)
		})
	}
}

func TestArtifactExportBoundsAdmissionAndHonorsCancelledWaiter(t *testing.T) {
	cfg, r, state := artifactExportFixture(t)
	original := cfg.Screener
	var exporter *verification.ArtifactExporter
	cfg.Screener = exportScreen(func(ctx context.Context, input verification.ArtifactExportScreenRequest) (verification.ArtifactExportScreenResult, error) {
		_, nestedErr := exporter.Export(ctx, r)
		require.ErrorIs(t, nestedErr, cortex.ErrVerificationUnavailable)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, nestedErr = exporter.Export(cancelled, r)
		require.ErrorIs(t, nestedErr, context.Canceled)
		return original.ScreenArtifactExport(ctx, input)
	})
	var err error
	exporter, err = verification.NewArtifactExporter(cfg)
	require.NoError(t, err)
	ctx := cortex.WithScope(t.Context(), r.Artifact.Scope)
	for i := 0; i < 2; i++ {
		_, err = exporter.Export(ctx, r)
		require.NoError(t, err)
	}
	require.Equal(t, 2, state.writes)
}
