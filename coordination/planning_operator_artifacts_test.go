package coordination_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

type planningArtifactPlanStore struct {
	coordination.PlanningOperatorStore
	plan planning.Plan
}

func (s planningArtifactPlanStore) GetPlan(context.Context, id.ID) (planning.Plan, error) {
	return s.plan, nil
}

type planningArtifactReads struct {
	reads []coordination.ArtifactBoardRead
	write coordination.ArtifactBoardRequest
}

func (r *planningArtifactReads) ResolvePlanningArtifactReads(context.Context, planning.Plan) ([]coordination.ArtifactBoardRead, error) {
	return r.reads, nil
}
func (r *planningArtifactReads) ResolvePlanningArtifact(context.Context, coordination.PlanningCommand) (coordination.ArtifactBoardRequest, error) {
	return r.write, nil
}

type planningArtifactHistory struct {
	coordination.ArtifactBoardStore
	entries         []coordination.ArtifactBoardEntry
	reads           int
	deny            bool
	revokeAfterRead bool
	writes          int
	writeErr        error
	late            *coordination.ArtifactBoardEntry
}

func (s *planningArtifactHistory) ReadArtifactBoard(_ context.Context, r coordination.ArtifactBoardRead) ([]coordination.ArtifactBoardEntry, error) {
	s.reads++
	if s.reads == 2 && s.late != nil {
		s.entries = append(s.entries, *s.late)
	}
	if s.revokeAfterRead {
		s.deny = true
	}
	var out []coordination.ArtifactBoardEntry
	for _, e := range s.entries {
		if e.Sequence > r.After && len(out) < r.Limit {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *planningArtifactHistory) WriteArtifactBoard(context.Context, coordination.ArtifactBoardRequest) (coordination.ArtifactBoardEntry, error) {
	s.writes++
	return coordination.ArtifactBoardEntry{}, s.writeErr
}
func (*planningArtifactHistory) WithArtifactBoardFence(_ context.Context, r coordination.ArtifactBoardRequest, next func(coordination.ArtifactBoardRequest) error) error {
	return next(r)
}
func (*planningArtifactHistory) AuthorizeArtifactBoard(context.Context, coordination.ArtifactBoardRequest) error {
	return nil
}
func (*planningArtifactHistory) WithArtifactBoardReadFence(_ context.Context, r coordination.ArtifactBoardRead, next func(coordination.ArtifactBoardRead) error) error {
	return next(r)
}
func (s *planningArtifactHistory) AuthorizeArtifactBoardRead(context.Context, coordination.ArtifactBoardRead) error {
	if s.deny {
		return errors.New("revoked")
	}
	return nil
}

func TestPlanningOperatorArtifactProjection(t *testing.T) {
	for _, mode := range []string{"conflict", "resolved", "denied", "revoked_during_read", "foreign_resource", "foreign_scope", "duplicate", "partial", "too_many", "changed_history"} {
		t.Run(mode, func(t *testing.T) {
			storage, f, source, auth, _ := planningOperatorFixture(t)
			resource := planning.ResourceGrant{Resource: "shared/report", Revision: "1", Access: planning.AccessWrite}
			p := f.Plan
			p.Resources = append(p.Resources, resource)
			var err error
			p, err = planning.Seal(p, planning.Limits{})
			require.NoError(t, err)
			source.plan = p
			source.states = nil
			req := coordination.ArtifactBoardRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixBoardTransition), Scope: p.Scope, PlanID: p.ID, PlanDigest: p.Digest, Resource: resource, AcceptanceID: id.New(coordination.PrefixTaskAcceptance), AcceptanceDigest: p.Digest, Current: f.Acceptance.Current, Actor: "host", Reason: "accepted output"}
			_, digest, err := req.Freeze()
			require.NoError(t, err)
			first, err := coordination.SealArtifactBoardEntry(coordination.ArtifactBoardEntry{SchemaVersion: 1, Request: req, RequestDigest: digest, PlanRevision: p.Revision, Sequence: 1, HeadRevision: 1, Owner: f.ProducerAssignment.Handoff.Receiver, TaskID: p.Tasks[0].ID, Artifact: req.Current.Candidate, At: f.ProducerAssignment.At})
			require.NoError(t, err)
			req.ID = id.New(coordination.PrefixBoardTransition)
			_, digest, err = req.Freeze()
			require.NoError(t, err)
			conflict, err := coordination.SealArtifactBoardEntry(coordination.ArtifactBoardEntry{SchemaVersion: 1, Request: req, RequestDigest: digest, PlanRevision: p.Revision, Sequence: 2, HeadRevision: 1, PreviousDigest: first.Digest, Conflict: true, Owner: first.Owner, TaskID: p.Tasks[0].ID, Artifact: req.Current.Candidate, At: first.At})
			require.NoError(t, err)
			history := &planningArtifactHistory{entries: []coordination.ArtifactBoardEntry{first, conflict}}
			reads := &planningArtifactReads{reads: []coordination.ArtifactBoardRead{{Scope: p.Scope, PlanID: p.ID, Resource: resource, Limit: coordination.MaxBoardTransitions}}}
			switch mode {
			case "resolved":
				req.ID = id.New(coordination.PrefixBoardTransition)
				req.Resolve = conflict.Request.ID
				req.ExpectedRevision = 1
				req.ExpectedDigest = first.Digest
				_, digest, err = req.Freeze()
				require.NoError(t, err)
				resolved, sealErr := coordination.SealArtifactBoardEntry(coordination.ArtifactBoardEntry{SchemaVersion: 1, Request: req, RequestDigest: digest, PlanRevision: p.Revision, Sequence: 3, HeadRevision: 2, PreviousDigest: first.Digest, Owner: first.Owner, TaskID: p.Tasks[0].ID, Artifact: req.Current.Candidate, At: first.At})
				require.NoError(t, sealErr)
				history.entries = append(history.entries, resolved)
			case "changed_history":
				late := conflict
				late.Request.ID = id.New(coordination.PrefixBoardTransition)
				late.Sequence = 3
				_, late.RequestDigest, err = late.Request.Freeze()
				require.NoError(t, err)
				late, err = coordination.SealArtifactBoardEntry(late)
				require.NoError(t, err)
				history.late = &late
			case "denied":
				history.deny = true
			case "revoked_during_read":
				history.revokeAfterRead = true
			case "foreign_resource":
				reads.reads[0].Resource.Revision = "other"
			case "foreign_scope":
				reads.reads[0].Scope.Levels = append(reads.reads[0].Scope.Levels[:0:0], reads.reads[0].Scope.Levels...)
				reads.reads[0].Scope.Levels[0].Value = "other"
			case "duplicate":
				reads.reads = append(reads.reads, reads.reads[0])
			case "partial":
				reads.reads[0].Limit = 1
			case "too_many":
				for len(reads.reads) <= coordination.MaxPlanningArtifactResources {
					reads.reads = append(reads.reads, reads.reads[0])
				}
			}
			board, err := coordination.NewArtifactBoard(history, history)
			require.NoError(t, err)
			operator, err := coordination.NewPlanningOperator(coordination.PlanningOperatorConfig{Store: planningArtifactPlanStore{PlanningOperatorStore: storage, plan: p}, Source: source, Authority: auth, Clock: planningOperatorClock{}, Artifacts: board, ArtifactResolver: reads, ArtifactReads: reads})
			require.NoError(t, err)
			detail, err := operator.Detail(f.Context, p.ID)
			if mode != "conflict" && mode != "resolved" {
				require.Error(t, err)
				require.Empty(t, detail.Artifacts)
				return
			}
			require.NoError(t, err)
			require.Len(t, detail.Artifacts, 1)
			view := detail.Artifacts[0]
			require.Equal(t, resource, view.Resource)
			require.NotNil(t, view.Head)
			if mode == "conflict" {
				require.EqualValues(t, 1, view.HeadRevision)
				require.Equal(t, first.Digest, view.HeadDigest)
				require.Len(t, view.Conflicts, 1)
				require.Equal(t, conflict.Request.ID, view.Conflicts[0].ID)
				resolution := conflict.Request
				resolution.ID = id.New(coordination.PrefixBoardTransition)
				resolution.Resolve = conflict.Request.ID
				resolution.ExpectedRevision = 1
				resolution.ExpectedDigest = first.Digest
				reads.write = resolution
				command := coordination.PlanningCommand{Operation: "resolve_conflict", OperationID: resolution.ID, PlanID: p.ID, ExpectedRevision: p.Revision, ExpectedDigest: p.Digest, ConflictID: conflict.Request.ID, ExpectedResourceRevision: 1, ExpectedResourceDigest: p.Digest, Reason: "select retained candidate"}
				_, commandErr := operator.Command(f.Context, command)
				require.ErrorIs(t, commandErr, planning.ErrConflict)
				require.Zero(t, history.writes)
				command.ExpectedResourceDigest = first.Digest
				history.writeErr = errors.New("authorized write reached")
				_, commandErr = operator.Command(f.Context, command)
				require.ErrorIs(t, commandErr, history.writeErr)
				require.Equal(t, 1, history.writes)
			} else {
				require.EqualValues(t, 2, view.HeadRevision)
				require.Equal(t, history.entries[2].Digest, view.HeadDigest)
				require.Empty(t, view.Conflicts)
			}
		})
	}
}
