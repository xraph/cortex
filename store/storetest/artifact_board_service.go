package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

type artifactPublicationResolver func(context.Context, coordination.TaskAcceptance) (*coordination.ArtifactBoardRequest, error)

func (f artifactPublicationResolver) ResolveArtifactPublication(ctx context.Context, a coordination.TaskAcceptance) (*coordination.ArtifactBoardRequest, error) {
	return f(ctx, a)
}

// ArtifactPublication verifies actual managed publication, restart and drainage.
func ArtifactPublication(t *testing.T, s store.Store) {
	for _, mode := range []string{"restart", "revoked after read", "timeout", "drain"} {
		t.Run(mode, func(t *testing.T) {
			ctx, requests := artifactBoardFixture(t, s)
			request := requests[0]
			storage := resultCapability[coordination.ArtifactPublicationStore](t, s)
			var revoked atomic.Bool
			var calls atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			if mode == "revoked after read" {
				storage = publicationReadRevocation{ArtifactPublicationStore: storage, revoked: &revoked}
			}
			config := coordination.ArtifactPublicationConfig{Store: storage, Authority: &boardFixtureAuthority{}, Discovery: adoptionPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
				return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: request.Scope, PlanID: request.PlanID}}}, nil
			}), AuthorizeScan: func(context.Context, id.ID) error {
				if revoked.Load() {
					return errAssignmentInjected
				}
				return nil
			}, Resolver: artifactPublicationResolver(func(ctx context.Context, accepted coordination.TaskAcceptance) (*coordination.ArtifactBoardRequest, error) {
				calls.Add(1)
				if accepted.Request.ID != request.AcceptanceID {
					return nil, nil
				}
				if mode == "timeout" || mode == "drain" {
					once.Do(func() { close(entered) })
					<-ctx.Done()
					if mode == "drain" {
						<-release
					}
					return nil, ctx.Err()
				}
				frozen, _, err := request.Freeze()
				return &frozen, err
			}), Interval: 10 * time.Millisecond, PageSize: 2, MaxInFlight: 1}
			if mode == "timeout" {
				config.PageTimeout = 50 * time.Millisecond
			}
			managed, err := engine.New(engine.WithStore(s), engine.WithArtifactPublication(config), engine.WithSuspensionTTL(0))
			require.NoError(t, err)
			require.NoError(t, managed.Start(ctx))
			t.Cleanup(func() { require.NoError(t, managed.Stop(context.Background())) })
			read := coordination.ArtifactBoardRead{Scope: request.Scope, PlanID: request.PlanID, Resource: request.Resource, Limit: 128}
			switch mode {
			case "restart":
				require.Eventually(t, func() bool {
					history, readErr := storage.ReadArtifactBoard(ctx, read)
					return readErr == nil && len(history) == 1
				}, 10*time.Second, 10*time.Millisecond)
				require.NoError(t, managed.Stop(context.Background()))
				require.NoError(t, managed.Start(ctx))
				require.Eventually(t, func() bool { return calls.Load() >= 4 }, 10*time.Second, 10*time.Millisecond)
				require.NoError(t, managed.Stop(context.Background()))
				history, readErr := storage.ReadArtifactBoard(ctx, read)
				require.NoError(t, readErr)
				require.Len(t, history, 1)
				require.False(t, history[0].Conflict)
			case "drain":
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("publication resolver did not start")
				}
				stopping, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				require.ErrorIs(t, managed.Stop(stopping), context.DeadlineExceeded)
				cancel()
				close(release)
				require.NoError(t, managed.Stop(context.Background()))
			default:
				require.Eventually(t, func() bool { status, _ := managed.ArtifactPublicationStatus(); return status.Failures > 0 }, 10*time.Second, 10*time.Millisecond)
				require.NoError(t, managed.Stop(context.Background()))
				if mode == "revoked after read" {
					require.Zero(t, calls.Load())
				}
			}
			if mode != "restart" {
				history, readErr := storage.ReadArtifactBoard(ctx, read)
				require.NoError(t, readErr)
				require.Empty(t, history)
			}
		})
	}
}

type publicationReadRevocation struct {
	coordination.ArtifactPublicationStore
	revoked *atomic.Bool
}

func (s publicationReadRevocation) ListTaskAcceptances(ctx context.Context, r coordination.TaskAcceptanceListRequest) ([]coordination.TaskAcceptance, error) {
	accepted, err := s.ArtifactPublicationStore.ListTaskAcceptances(ctx, r)
	s.revoked.Store(true)
	return accepted, err
}
