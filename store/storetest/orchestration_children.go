package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/store"
)

func testOrchestrationStaleChildren(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx := ctxWithScope("child-history", "project")
	team := &orchestration.Run{ID: id.NewOrchestrationID(), Status: orchestration.StatusRunning, Strategy: orchestration.StrategyParallel, StartedAt: time.Now().UTC()}
	require.NoError(t, s.CreateOrchestrationRun(ctx, team))
	stale, err := s.GetOrchestrationRun(ctx, team.ID)
	require.NoError(t, err)
	first, second := id.NewAgentRunID(), id.NewAgentRunID()
	team.AgentRunIDs = []id.AgentRunID{first}
	require.NoError(t, s.UpdateOrchestrationRun(ctx, team))
	stale.AgentRunIDs = []id.AgentRunID{second}
	require.NoError(t, s.UpdateOrchestrationRun(ctx, stale))
	stored, err := s.GetOrchestrationRun(ctx, team.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []id.AgentRunID{first, second}, stored.AgentRunIDs)
	require.ElementsMatch(t, stored.AgentRunIDs, stale.AgentRunIDs, "acknowledgment includes retained children")
	stored.Status = orchestration.StatusCompleted
	require.NoError(t, s.UpdateOrchestrationRun(ctx, stored))
	require.ErrorIs(t, s.UpdateOrchestrationRun(ctx, stale), cortex.ErrOrchestrationRunClosed)
}

func testOrchestrationChildLinks(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	links, ok := s.(orchestration.ChildRunStore)
	require.True(t, ok)
	ctx := ctxWithScope("child-links", "project")
	team := &orchestration.Run{ID: id.NewOrchestrationID(), Status: orchestration.StatusRunning, Strategy: orchestration.StrategyParallel, StartedAt: time.Now().UTC()}
	require.NoError(t, s.CreateOrchestrationRun(ctx, team))
	child := id.NewAgentRunID()
	require.ErrorIs(t, links.LinkOrchestrationChild(context.Background(), team.ID, child), cortex.ErrNoScope)
	require.ErrorIs(t, links.LinkOrchestrationChild(ctxWithScope("child-links"), team.ID, child), cortex.ErrOrchestrationRunNotFound)
	require.ErrorIs(t, links.LinkOrchestrationChild(ctxWithScope("another"), team.ID, child), cortex.ErrOrchestrationRunNotFound)
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { results <- links.LinkOrchestrationChild(ctx, team.ID, child) })
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, cortex.ErrAlreadyExists)
		}
	}
	require.Equal(t, 1, winners)
	const count = 8
	children := make([]id.AgentRunID, count, count+1)
	linkErrors := make(chan error, count)
	for i := range children {
		children[i] = id.NewAgentRunID()
		wg.Go(func() { linkErrors <- links.LinkOrchestrationChild(ctx, team.ID, children[i]) })
	}
	wg.Wait()
	close(linkErrors)
	for err := range linkErrors {
		require.NoError(t, err)
	}
	// Complete using the original snapshot, before any child was recorded.
	team.Status = orchestration.StatusCompleted
	require.NoError(t, s.UpdateOrchestrationRun(ctx, team))
	stored, err := s.GetOrchestrationRun(ctx, team.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, append(children, child), stored.AgentRunIDs)
	require.ErrorIs(t, links.LinkOrchestrationChild(ctx, team.ID, id.NewAgentRunID()), cortex.ErrOrchestrationRunClosed)
}

func testOrchestrationLinkCompletionRace(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	links, ok := s.(orchestration.ChildRunStore)
	require.True(t, ok)
	ctx := ctxWithScope("link-close")
	for range 8 {
		teamID, child := id.NewOrchestrationID(), id.NewAgentRunID()
		record := &orchestration.Run{ID: teamID, Status: orchestration.StatusRunning, StartedAt: time.Now().UTC()}
		require.NoError(t, s.CreateOrchestrationRun(ctx, record))
		record.Status = orchestration.StatusCompleted
		start := make(chan struct{})
		linked, finished := make(chan error, 1), make(chan error, 1)
		go func() { <-start; linked <- links.LinkOrchestrationChild(ctx, teamID, child) }()
		go func() { <-start; finished <- s.UpdateOrchestrationRun(ctx, record) }()
		close(start)
		linkErr, finishErr := <-linked, <-finished
		require.NoError(t, finishErr)
		stored, err := s.GetOrchestrationRun(ctx, teamID)
		require.NoError(t, err)
		require.Equal(t, orchestration.StatusCompleted, stored.Status)
		if linkErr == nil {
			require.Equal(t, []id.AgentRunID{child}, stored.AgentRunIDs)
		} else {
			require.ErrorIs(t, linkErr, cortex.ErrOrchestrationRunClosed)
			require.Empty(t, stored.AgentRunIDs)
		}
	}
}

// OrchestrationRevisionUpgrade preserves historical children through upgrade.
func OrchestrationRevisionUpgrade(t *testing.T, s store.Store, legacy func()) {
	t.Helper()
	ctx := ctxWithScope("team-upgrade")
	first, second := id.NewAgentRunID(), id.NewAgentRunID()
	record := &orchestration.Run{ID: id.NewOrchestrationID(), Status: orchestration.StatusRunning, AgentRunIDs: []id.AgentRunID{first}, StartedAt: time.Now().UTC()}
	require.NoError(t, s.CreateOrchestrationRun(ctx, record))
	legacy()
	require.NoError(t, s.Migrate(context.Background()))
	require.NoError(t, s.Migrate(context.Background()))
	links, ok := s.(orchestration.ChildRunStore)
	require.True(t, ok)
	require.NoError(t, links.LinkOrchestrationChild(ctx, record.ID, second))
	record.Status = orchestration.StatusCompleted
	require.NoError(t, s.UpdateOrchestrationRun(ctx, record))
	stored, err := s.GetOrchestrationRun(ctx, record.ID)
	require.NoError(t, err)
	require.ElementsMatch(t, []id.AgentRunID{first, second}, stored.AgentRunIDs)
}

// OrchestrationSQLRevisionUpgrade also replays an unrecorded schema change.
func OrchestrationSQLRevisionUpgrade(t *testing.T, s store.Store, exec func(string) error) {
	t.Helper()
	const forget = "DELETE FROM grove_migrations WHERE version = '20261025000003' AND \"group\" = 'cortex'"
	OrchestrationRevisionUpgrade(t, s, func() {
		require.NoError(t, exec("ALTER TABLE cortex_orchestration_runs DROP COLUMN revision"))
		require.NoError(t, exec(forget))
	})
	require.NoError(t, exec(forget))
	require.NoError(t, s.Migrate(context.Background()))
}

type childLinkStore struct {
	store.Store
	links   orchestration.ChildRunStore
	lostAck error
}

func (s *childLinkStore) UpdateOrchestrationRun(ctx context.Context, record *orchestration.Run) error {
	if record.Status == orchestration.StatusRunning {
		return errors.New("full-row admission is not atomic child linkage")
	}
	return s.Store.UpdateOrchestrationRun(ctx, record)
}

func (s *childLinkStore) LinkOrchestrationChild(ctx context.Context, team id.OrchestrationID, child id.AgentRunID) error {
	if err := s.links.LinkOrchestrationChild(ctx, team, child); err != nil {
		return err
	}
	return s.lostAck
}

func testOrchestrationLinkedService(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, lostAck := range []bool{false, true} {
		t.Run(map[bool]string{false: "retains_async_child", true: "lost_ack"}[lostAck], func(t *testing.T) {
			backend := newStore(t)
			links, ok := backend.(orchestration.ChildRunStore)
			require.True(t, ok)
			ctx := ctxWithScope("linked-service")
			storage := &childLinkStore{Store: backend, links: links}
			if lostAck {
				storage.lostAck = errors.New("link acknowledgment lost")
			}
			child, extra := id.NewAgentRunID(), id.NewAgentRunID()
			executions := 0
			runner := reservedMessageRunner{reserve: func(context.Context, id.OrchestrationID, string, string, *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
				return &cortex.ReservedInvocation{RunID: child, Execute: func(execCtx context.Context) (*cortex.AgentResult, error) {
					executions++
					parents, listErr := backend.ListOrchestrationRuns(execCtx, nil)
					require.NoError(t, listErr)
					require.Len(t, parents, 1)
					require.Contains(t, parents[0].AgentRunIDs, child)
					require.NoError(t, links.LinkOrchestrationChild(execCtx, parents[0].ID, extra))
					return &cortex.AgentResult{RunID: child, AgentName: "worker", Output: "done"}, nil
				}}, nil
			}}
			config := orchestration.Config{Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: "worker"}}}
			record, err := orchestration.NewReservedService(runner, storage, nil).RunConfig(ctx, config, "work")
			require.NotNil(t, record)
			stored, readErr := backend.GetOrchestrationRun(ctx, record.ID)
			require.NoError(t, readErr)
			if lostAck {
				require.ErrorIs(t, err, storage.lostAck)
				require.Zero(t, executions)
				require.Equal(t, orchestration.StatusFailed, stored.Status)
				require.Equal(t, []id.AgentRunID{child}, stored.AgentRunIDs)
			} else {
				require.NoError(t, err)
				require.Equal(t, 1, executions)
				require.ElementsMatch(t, []id.AgentRunID{child, extra}, stored.AgentRunIDs)
				require.ElementsMatch(t, stored.AgentRunIDs, record.AgentRunIDs)
			}
		})
	}
}
