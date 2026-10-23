package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

type contentScanner struct{ safety.Scanner }

type contentAuthorizer func(context.Context, verification.ResultContentRequest) error

func (f contentAuthorizer) AuthorizeResultContent(ctx context.Context, r verification.ResultContentRequest) error {
	return f(ctx, r)
}

type contentGate func(context.Context, safety.PublicationRequest) (safety.PublicationDecision, error)

func (f contentGate) Check(ctx context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
	return f(ctx, r)
}

func TestVerifiedContentReadAfterRestart(t *testing.T) {
	f := newWorkerFixture(t)
	committed, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	reopened := openSQLiteStoreForTest(t, f.path)
	calls := 0
	reader, err := engine.New(engine.WithResultContent(engine.ResultContentConfig{Authorizer: contentAuthorizer(func(context.Context, verification.ResultContentRequest) error { calls++; return nil }), Fence: f.config.Fence, Clock: workerClock{}}), engine.WithStore(reopened), engine.WithPublicationGate(contentGate(func(_ context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
		require.Equal(t, "result_replay", r.Source)
		require.Equal(t, f.request.Job.Binding.RunID.String(), r.RunID)
		require.Equal(t, safety.DirectionOutput, r.Direction)
		return safety.PublicationDecision{Text: "safe projection", Status: safety.StatusRedacted}, nil
	})))
	require.NoError(t, err)
	require.True(t, reader.ResultContentAvailable())
	before, err := reopened.GetRun(f.ctx, f.request.Job.Binding.RunID)
	require.NoError(t, err)
	result, err := reader.ReadResultContent(f.ctx, verification.ResultContentRequest{SchemaVersion: 1, RunID: f.request.Job.Binding.RunID, ResultID: f.request.ResultID})
	require.NoError(t, err)
	require.Equal(t, "safe projection", result.Content)
	require.True(t, result.ContentChanged)
	require.Equal(t, safety.StatusRedacted, result.PublicationStatus)
	require.Equal(t, committed.Receipt.ObservedAt, result.VerifiedAt)
	require.Equal(t, 3, calls)
	after, err := reopened.GetRun(f.ctx, f.request.Job.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	wire, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(wire), f.request.Job.Binding.Candidate.ID.String())
	require.NotContains(t, string(wire), f.request.Job.Binding.Context.PreparedDigest)
	require.Equal(t, 1, f.adapter.evaluations)
	require.Equal(t, 2, f.llm.calls)
}

func TestVerifiedContentReadRejectsChangesDuringScreening(t *testing.T) {
	for _, mode := range []string{"deleted", "revoked", "run changed", "scanner error", "blocked", "wrong scope"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			_, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
			require.NoError(t, err)
			revoked := false
			reader, err := engine.New(engine.WithStore(f.storage), engine.WithResultContent(engine.ResultContentConfig{Authorizer: contentAuthorizer(func(context.Context, verification.ResultContentRequest) error {
				if revoked {
					return errors.New("private grant")
				}
				return nil
			}), Fence: f.config.Fence, Clock: workerClock{}}), engine.WithPublicationGate(contentGate(func(_ context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
				switch mode {
				case "deleted":
					require.NoError(t, f.storage.TombstoneEvidence(evidence.WithDeletionActor(f.ctx, "operator"), f.request.Job.Binding.Candidate, "revoked"))
				case "revoked":
					revoked = true
				case "run changed":
					current, readErr := f.storage.GetRun(f.ctx, f.request.Job.Binding.RunID)
					require.NoError(t, readErr)
					require.NoError(t, f.storage.UpdateRun(f.ctx, current))
				case "scanner error":
					return safety.PublicationDecision{}, errors.New("private provider payload")
				case "blocked":
					return safety.PublicationDecision{}, safety.ErrBlocked
				}
				return safety.PublicationDecision{Text: r.Content, Status: safety.StatusApproved}, nil
			})))
			require.NoError(t, err)
			ctx := f.ctx
			if mode == "wrong scope" {
				ctx = overlayCtx("other")
			}
			result, err := reader.ReadResultContent(ctx, verification.ResultContentRequest{SchemaVersion: 1, RunID: f.request.Job.Binding.RunID, ResultID: f.request.ResultID})
			require.Error(t, err)
			require.Zero(t, result)
			require.NotContains(t, err.Error(), "private")
		})
	}
}

func TestVerifiedContentRequiresExplicitServices(t *testing.T) {
	f := newWorkerFixture(t)
	config := engine.ResultContentConfig{Authorizer: contentAuthorizer(func(context.Context, verification.ResultContentRequest) error { return nil }), Fence: f.config.Fence, Clock: workerClock{}}
	for _, mode := range []string{"gate", "typed nil gate", "typed nil scanner", "disabled", "authorizer", "fence", "clock", "store"} {
		t.Run(mode, func(t *testing.T) {
			changed := config
			opts := []engine.Option{engine.WithStore(f.storage), engine.WithPublicationGate(contentGate(func(_ context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
				return safety.PublicationDecision{Text: r.Content, Status: safety.StatusApproved}, nil
			}))}
			switch mode {
			case "gate":
				opts = opts[:1]
			case "typed nil gate":
				var gate contentGate
				opts = append(opts, engine.WithPublicationGate(gate))
			case "typed nil scanner":
				opts = append(opts[:1], engine.WithSafety((*contentScanner)(nil)))
			case "disabled":
				opts = append(opts, engine.WithPublicationPolicy(safety.Policy{Mode: safety.ModeDisabled}))
			case "authorizer":
				var auth contentAuthorizer
				changed.Authorizer = auth
			case "fence":
				var fence workerFence
				changed.Fence = fence
			case "clock":
				changed.Clock = nil
			case "store":
				opts = opts[1:]
			}
			opts = append(opts, engine.WithResultContent(changed))
			_, err := engine.New(opts...)
			require.Error(t, err)
		})
	}
	e, err := engine.New(engine.WithStore(f.storage))
	require.NoError(t, err)
	require.False(t, e.ResultContentAvailable())
	result, err := e.ReadResultContent(f.ctx, verification.ResultContentRequest{})
	require.Error(t, err)
	require.Zero(t, result)
}
