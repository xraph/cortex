package storetest

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

func artifactBoardFixture(t *testing.T, s store.Store) (context.Context, []coordination.ArtifactBoardRequest) {
	t.Helper()
	resource := planning.ResourceGrant{Resource: "shared/report", Revision: "1", Access: planning.AccessWrite}
	config := taskContractConfig(t)
	previous := config.Plan
	config.Plan = func(p *planning.Plan) {
		previous(p)
		p.Owners = append(p.Owners, "second-worker")
		p.Resources = append(p.Resources, resource)
		for i := range p.Tasks {
			p.Tasks[i].Resources = append(p.Tasks[i].Resources, resource)
			p.Tasks[i].DependsOn = nil
		}
		p.Tasks[1].Owner = "second-worker"
	}
	ctx, working, change, job := configuredSubmissionFixture(t, s, config)
	req := finishTaskAcceptanceFixture(ctx, t, s, working, change, job)
	accepted, err := resultCapability[coordination.TaskAcceptanceStore](t, s).AcceptTaskResult(ctx, req)
	require.NoError(t, err)
	p, err := resultCapability[planning.Store](t, s).GetPlan(ctx, working.Handoff.PlanID)
	require.NoError(t, err)
	requests := make([]coordination.ArtifactBoardRequest, 0, 2)
	requests = append(requests, boardRequest(accepted, p, resource))
	h := working.Handoff
	h.ID = id.New(coordination.PrefixHandoff)
	h.AssignmentID = id.New(coordination.PrefixAssignment)
	h.TaskID = p.Tasks[1].ID
	h.Criteria = p.Tasks[1].Criteria
	h.Receiver = coordination.Identity{Agent: "second-worker", Principal: "worker-2"}
	h.Authority, err = planning.TaskAuthority(p, h.TaskID)
	require.NoError(t, err)
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	contract, err := planning.TaskContract(p, h.TaskID, id.New(verification.PrefixContract))
	require.NoError(t, err)
	ctx, working, change, job = submissionFixtureForHandoff(ctx, t, s, p, h, contract)
	req = finishTaskAcceptanceFixture(ctx, t, s, working, change, job)
	accepted, err = resultCapability[coordination.TaskAcceptanceStore](t, s).AcceptTaskResult(ctx, req)
	require.NoError(t, err)
	return ctx, append(requests, boardRequest(accepted, p, resource))
}
func boardRequest(accepted coordination.TaskAcceptance, p planning.Plan, resource planning.ResourceGrant) coordination.ArtifactBoardRequest {
	return coordination.ArtifactBoardRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixBoardTransition), Scope: p.Scope, PlanID: p.ID, PlanDigest: p.Digest, Resource: resource, AcceptanceID: accepted.Request.ID, AcceptanceDigest: accepted.Digest, Current: accepted.Request.Current, Actor: "host", Reason: "publish accepted output"}
}

// ArtifactBoard exercises accepted resource conflicts without rewriting results.
func ArtifactBoard(t *testing.T, s store.Store) {
	ctx, reqs := artifactBoardFixture(t, s)
	board := resultCapability[coordination.ArtifactBoardStore](t, s)

	for _, mode := range []string{"source", "binding"} {
		invalid, _, freezeErr := reqs[0].Freeze()
		require.NoError(t, freezeErr)
		invalid.ID = id.New(coordination.PrefixBoardTransition)
		if mode == "source" {
			invalid.Current.Context.Sources[0].Revision = "changed"
		} else {
			invalid.Current.Context.AuthorityRevision = "revoked"
		}
		_, writeErr := board.WriteArtifactBoard(ctx, invalid)
		require.Error(t, writeErr)
	}
	var entries [2]coordination.ArtifactBoardEntry
	var errs [2]error
	var wait sync.WaitGroup
	for i := range reqs {
		wait.Add(1)
		go func() { defer wait.Done(); entries[i], errs[i] = board.WriteArtifactBoard(ctx, reqs[i]) }()
	}
	wait.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.NotEqual(t, entries[0].Conflict, entries[1].Conflict, "one candidate wins the initial compare-and-swap")
	head, conflict := entries[0], entries[1]
	if head.Conflict {
		head, conflict = conflict, head
	}
	require.EqualValues(t, 1, head.HeadRevision)
	require.EqualValues(t, 1, conflict.HeadRevision)
	require.Equal(t, head.Digest, conflict.PreviousDigest)
	replay, err := board.WriteArtifactBoard(ctx, head.Request)
	require.NoError(t, err)
	require.Equal(t, head, replay)
	resolve := conflict.Request
	resolve.ID = id.New(coordination.PrefixBoardTransition)
	resolve.Resolve = conflict.Request.ID
	resolve.ExpectedRevision = 1
	resolve.ExpectedDigest = head.Digest

	resolutionAuthority := &boardFixtureAuthority{denyResolution: true}
	guardedResolution, guardErr := coordination.NewArtifactBoard(board, resolutionAuthority)
	require.NoError(t, guardErr)
	_, guardErr = guardedResolution.Write(ctx, resolve)
	require.ErrorIs(t, guardErr, errAssignmentInjected)
	resolutionAuthority.denyResolution = false
	resolved, err := guardedResolution.Write(ctx, resolve)
	require.NoError(t, err)
	require.False(t, resolved.Conflict)
	require.EqualValues(t, 2, resolved.HeadRevision)
	require.Equal(t, conflict.Owner, resolved.Owner)
	again := resolve
	again.ID = id.New(coordination.PrefixBoardTransition)
	again.ExpectedRevision = 2
	again.ExpectedDigest = resolved.Digest
	_, err = board.WriteArtifactBoard(ctx, again)
	require.ErrorIs(t, err, planning.ErrConflict, "same conflict cannot be resolved twice")
	history, err := board.ReadArtifactBoard(ctx, coordination.ArtifactBoardRead{Scope: resolve.Scope, PlanID: resolve.PlanID, Resource: resolve.Resource, Limit: 128})
	require.NoError(t, err)
	require.Len(t, history, 3)
	for _, entry := range history {
		require.NoError(t, entry.Validate())
	}
	retained, err := resultCapability[coordination.TaskAcceptanceStore](t, s).GetTaskAcceptance(ctx, head.Request.AcceptanceID)
	require.NoError(t, err)
	require.Equal(t, head.Request.AcceptanceDigest, retained.Digest)
	for _, mode := range []string{"resource", "source", "binding", "scope"} {
		t.Run(mode, func(t *testing.T) {
			bad, _, freezeErr := head.Request.Freeze()
			require.NoError(t, freezeErr)
			bad.ID = id.New(coordination.PrefixBoardTransition)
			switch mode {
			case "resource":
				bad.Resource.Revision = "2"
			case "source":
				bad.Current.Context.Sources[0].Revision = "2"
			case "binding":
				bad.Current.Context.AuthorityRevision = "revoked"
			case "scope":
				bad.Scope.Levels[0].Value = "other"
			}
			_, writeErr := board.WriteArtifactBoard(ctx, bad)
			require.Error(t, writeErr)
		})
	}
	// Exact historical metadata survives later head changes, but current authority
	// remains mandatory on public retries and reads.
	auth := &boardFixtureAuthority{}
	guarded, err := coordination.NewArtifactBoard(board, auth)
	require.NoError(t, err)
	auth.deny = true
	_, err = guarded.Write(ctx, head.Request)
	require.ErrorIs(t, err, errAssignmentInjected)
	auth.deny = false
	auth.denyReadAfter = 2
	_, err = guarded.Read(ctx, coordination.ArtifactBoardRead{Scope: resolve.Scope, PlanID: resolve.PlanID, Resource: resolve.Resource, Limit: 128})
	require.ErrorIs(t, err, errAssignmentInjected)
}

type boardFixtureAuthority struct {
	deny                 bool
	denyResolution       bool
	denyReadAfter, reads int
}

func (a *boardFixtureAuthority) WithArtifactBoardFence(_ context.Context, r coordination.ArtifactBoardRequest, next func(coordination.ArtifactBoardRequest) error) error {
	return next(r)
}
func (a *boardFixtureAuthority) AuthorizeArtifactBoard(_ context.Context, r coordination.ArtifactBoardRequest) error {
	if a.deny || (a.denyResolution && !r.Resolve.IsNil()) {
		return errAssignmentInjected
	}
	return nil
}
func (a *boardFixtureAuthority) WithArtifactBoardReadFence(_ context.Context, r coordination.ArtifactBoardRead, next func(coordination.ArtifactBoardRead) error) error {
	return next(r)
}
func (a *boardFixtureAuthority) AuthorizeArtifactBoardRead(_ context.Context, _ coordination.ArtifactBoardRead) error {
	a.reads++
	if a.deny || (a.denyReadAfter > 0 && a.reads >= a.denyReadAfter) {
		return errAssignmentInjected
	}
	return nil
}
