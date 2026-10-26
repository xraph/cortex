package coordination_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/store/storetest"
)

type planningOperatorAuthority struct {
	calls  int
	denyAt int
	escape bool
}

func (a *planningOperatorAuthority) AuthorizePlanning(context.Context, coordination.PlanningAccess) error {
	a.calls++
	if a.denyAt > 0 && a.calls >= a.denyAt {
		return errors.New("revoked")
	}
	return nil
}
func (a *planningOperatorAuthority) WithPlanningReadFence(_ context.Context, _ coordination.PlanningAccess, next func() error) error {
	if a.escape {
		return nil
	}
	return next()
}

type planningOperatorClock struct{ now time.Time }

func (c planningOperatorClock) Now() time.Time {
	if c.now.IsZero() {
		return time.Now().UTC()
	}
	return c.now
}

type planningOperatorSource struct {
	plan   planning.Plan
	states []planning.TaskState
	calls  int
}

func (s *planningOperatorSource) LoadSchedulingState(context.Context, id.ID) (planning.Plan, []planning.TaskState, error) {
	s.calls++
	return s.plan, s.states, nil
}

func planningOperatorFixture(t *testing.T) (*sqlite.Store, storetest.TaskCompletionFixture, *planningOperatorSource, *planningOperatorAuthority, *coordination.PlanningOperator) {
	t.Helper()
	driver := sqlitedriver.New()
	require.NoError(t, driver.Open(context.Background(), filepath.Join(t.TempDir(), "planning.db")))
	db, err := grove.Open(driver)
	require.NoError(t, err)
	storage := sqlite.New(db)
	require.NoError(t, storage.Migrate(context.Background()))
	t.Cleanup(func() { require.NoError(t, storage.Close()) })
	f := storetest.NewTaskCompletionFixture(t, storage, false)
	source := &planningOperatorSource{plan: f.Plan, states: []planning.TaskState{{TaskID: f.Plan.Tasks[0].ID, PlanDigest: f.Plan.Digest, Status: planning.StatusSucceeded, Current: f.Acceptance.Current}}}
	auth := &planningOperatorAuthority{}
	operator, err := coordination.NewPlanningOperator(coordination.PlanningOperatorConfig{Store: storage, Source: source, Authority: auth, Clock: planningOperatorClock{}})
	require.NoError(t, err)
	return storage, f, source, auth, operator
}

func TestPlanningOperatorRejectsUnacceptedSuccessAndRechecksAuthority(t *testing.T) {
	_, f, source, auth, operator := planningOperatorFixture(t)
	detail, err := operator.Detail(f.Context, f.Plan.ID)
	require.NoError(t, err)
	require.False(t, detail.Complete)
	require.False(t, detail.Tasks[0].CurrentlyAccepted)
	require.Nil(t, detail.Tasks[0].AcceptedArtifact)
	require.Equal(t, "wait", detail.Tasks[0].NextAction)
	require.False(t, detail.Tasks[0].Ready)
	auth.denyAt = auth.calls + 1
	before := source.calls
	_, err = operator.Detail(f.Context, f.Plan.ID)
	require.ErrorIs(t, err, coordination.ErrPlanningDenied)
	require.Equal(t, before, source.calls)
	auth.denyAt = auth.calls + 2
	_, err = operator.Detail(f.Context, f.Plan.ID)
	require.ErrorIs(t, err, coordination.ErrPlanningDenied)
	auth.denyAt = 0
	auth.escape = true
	_, err = operator.Detail(f.Context, f.Plan.ID)
	require.ErrorIs(t, err, planning.ErrConflict)
}

func TestPlanningOperatorAcceptedArtifactsRequireFreshEvidence(t *testing.T) {
	storage, f, source, _, operator := planningOperatorFixture(t)
	_, err := storage.RecordAssignmentReview(f.Context, f.Review)
	require.NoError(t, err)
	request := f.Acceptance
	adjudicated, err := storage.RecordTaskAdjudication(f.Context, coordination.TaskAdjudicationRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixTaskAdjudication), PlanID: f.Plan.ID, PlanDigest: f.Plan.Digest, TaskID: f.Plan.Tasks[0].ID, AssignmentID: request.AssignmentID, AssignmentRevision: request.AssignmentRevision, AssignmentDigest: request.AssignmentDigest, BindingDigest: f.Review.BindingDigest, ReviewID: f.Review.ID, ReviewDigest: f.Review.Digest, PolicyRevision: f.Plan.Tasks[0].Review.PolicyRevision, Actor: "coordinator", Reason: "supported review"})
	require.NoError(t, err)
	request.SchemaVersion = 2
	request.Adjudication = &coordination.TaskAdjudicationRef{ID: adjudicated.Request.ID, Digest: adjudicated.Digest}
	_, err = storage.AcceptTaskResult(f.Context, request)
	require.NoError(t, err)
	snapshot, snapshotErr := storage.ReadResultSnapshot(f.Context, request.Result)
	require.NoError(t, snapshotErr)
	require.NoError(t, snapshot.Validate(request.Result, time.Now().UTC()))
	state := source.states[0]
	state.Reports = snapshot.Reports
	state.Contract = &snapshot.Contract
	require.NoError(t, planning.ValidateTaskAcceptance(f.Plan, state.TaskID, []planning.TaskState{state}, time.Now().UTC()))
	detail, err := operator.Detail(f.Context, f.Plan.ID)
	require.NoError(t, err)
	require.False(t, detail.Complete)
	require.True(t, detail.Tasks[1].Ready)
	require.True(t, detail.Tasks[0].CurrentlyAccepted)
	require.NotNil(t, detail.Tasks[0].AcceptedArtifact)
	data, err := json.Marshal(detail)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(data), "\"reports\""))
	require.False(t, strings.Contains(string(data), "\"output\":"))
	source.states[0].Current.Context.AuthorityRevision = "revoked"
	detail, err = operator.Detail(f.Context, f.Plan.ID)
	require.NoError(t, err)
	require.False(t, detail.Complete)
	require.NotEmpty(t, detail.Tasks[0].AcceptanceID)
	require.Nil(t, detail.Tasks[0].AcceptedArtifact)
	source.states[0].Current = request.Current
	require.NoError(t, storage.TombstoneEvidence(evidence.WithDeletionActor(f.Context, "host"), request.Current.Candidate, "deleted"))
	detail, err = operator.Detail(f.Context, f.Plan.ID)
	if err == nil {
		require.False(t, detail.Complete)
		require.Nil(t, detail.Tasks[0].AcceptedArtifact)
	}
}

func TestPlanningOperatorBoundsScopeAndStaleCommands(t *testing.T) {
	_, f, _, _, operator := planningOperatorFixture(t)
	_, err := operator.List(f.Context, planning.ListRequest{Limit: 65})
	require.ErrorIs(t, err, planning.ErrInvalid)
	listed, err := operator.List(f.Context, planning.ListRequest{Limit: 10})
	require.NoError(t, err)
	require.Len(t, listed, 1)
	foreign := cortex.WithScope(f.Context, cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "other"}}})
	_, err = operator.Detail(foreign, f.Plan.ID)
	require.Error(t, err)
	command := coordination.PlanningCommand{Operation: "claim", OperationID: id.New(coordination.PrefixClaim), PlanID: f.Plan.ID, ExpectedRevision: f.Plan.Revision + 1, ExpectedDigest: f.Plan.Digest, TaskID: f.Plan.Tasks[0].ID, Reason: "retry after review"}
	_, err = operator.Command(f.Context, command)
	require.ErrorIs(t, err, planning.ErrConflict)
	command.ExpectedRevision = f.Plan.Revision
	_, err = operator.Command(f.Context, command)
	require.ErrorIs(t, err, coordination.ErrPlanningUnavailable)
	require.Equal(t, coordination.PlanningCapabilities{}, operator.Capabilities())
}

type unavailablePlanningClaims struct{ calls int }

func (p *unavailablePlanningClaims) ResolvePlanningClaim(context.Context, coordination.PlanningCommand) (coordination.ReadyTaskClaimRequest, error) {
	p.calls++
	return coordination.ReadyTaskClaimRequest{}, errors.New("unexpected claim resolution")
}
func (p *unavailablePlanningClaims) Admit(context.Context, coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
	p.calls++
	return coordination.ReadyTaskClaim{}, errors.New("unexpected claim admission")
}
func (p *unavailablePlanningClaims) RunClaimedTask(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) error {
	p.calls++
	return errors.New("unexpected worker")
}
func TestPlanningOperatorRetryAndReassignRequireExplicitHostSupport(t *testing.T) {
	storage, f, source, authority, _ := planningOperatorFixture(t)
	claims := &unavailablePlanningClaims{}
	operator, err := coordination.NewPlanningOperator(coordination.PlanningOperatorConfig{Store: storage, Source: source, Authority: authority, Clock: planningOperatorClock{}, Claims: claims, Admitter: claims, Worker: claims})
	require.NoError(t, err)
	require.True(t, operator.Capabilities().Claim)
	require.False(t, operator.Capabilities().Retry)
	require.False(t, operator.Capabilities().Reassign)
	for _, operation := range []string{"retry", "reassign"} {
		_, err = operator.Command(f.Context, coordination.PlanningCommand{Operation: operation, OperationID: id.New(coordination.PrefixClaim), PlanID: f.Plan.ID, ExpectedRevision: f.Plan.Revision, ExpectedDigest: f.Plan.Digest, TaskID: f.Plan.Tasks[0].ID, Reason: "unsupported operation"})
		require.ErrorIs(t, err, coordination.ErrPlanningUnavailable)
	}
	require.Zero(t, claims.calls)
}
