package storetest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store"
)

var errCompletionFixture = errors.New("fixture acknowledgment lost")

type completionFixtureStore struct {
	coordination.TaskCompletionServiceStore
	lose    atomic.Bool
	settles atomic.Int32
}

func (s *completionFixtureStore) SettleTaskCompletion(ctx context.Context, assignmentID id.ID, digest string) (*coordination.TaskCompletionSettlement, error) {
	result, err := s.TaskCompletionServiceStore.SettleTaskCompletion(ctx, assignmentID, digest)
	s.settles.Add(1)
	return result, err
}

func (s *completionFixtureStore) ReserveTaskCompletion(ctx context.Context, r coordination.TaskCompletionRequest) (coordination.TaskCompletion, error) {
	result, err := s.TaskCompletionServiceStore.ReserveTaskCompletion(ctx, r)
	if err == nil && s.lose.Swap(false) {
		return coordination.TaskCompletion{}, errCompletionFixture
	}
	return result, err
}

type completionFixtureResolver struct {
	request    coordination.TaskCompletionRequest
	accept     coordination.TaskAcceptanceRequest
	submission *coordination.Submission
	abandon    bool
}

func (r completionFixtureResolver) ResolveTaskCompletionAbandonment(_ context.Context, c coordination.TaskCompletion) (*coordination.TaskCompletionAbandonmentRequest, error) {
	if !r.abandon {
		return nil, nil
	}
	proof := completionAbandonment(c)
	return &proof, nil
}

func (r completionFixtureResolver) ResolveTaskSubmission(context.Context, coordination.Assignment) (*coordination.Submission, error) {
	return r.submission, nil
}
func (r completionFixtureResolver) ResolveTaskCompletion(_ context.Context, subject coordination.Assignment) (*coordination.TaskCompletionRequest, error) {
	req := r.request
	req.AssignmentRevision, req.AssignmentDigest = subject.Revision, subject.Digest
	return &req, nil
}
func (r completionFixtureResolver) ResolveTaskAcceptance(_ context.Context, c coordination.TaskCompletion, a coordination.TaskAdjudication) (*coordination.TaskAcceptanceRequest, error) {
	req := r.accept
	req.SchemaVersion = 2
	req.ID = c.Request.AcceptanceID
	req.AssignmentRevision, req.AssignmentDigest = c.Request.AssignmentRevision, c.Request.AssignmentDigest
	req.Adjudication = &coordination.TaskAdjudicationRef{ID: a.Request.ID, Digest: a.Digest}
	return &req, nil
}

type completionFixtureAuthority struct{ deny bool }

func (a completionFixtureAuthority) WithTaskCompletionFence(_ context.Context, subject coordination.Assignment, next func(coordination.Assignment) error) error {
	return next(subject)
}
func (a completionFixtureAuthority) AuthorizeTaskCompletion(context.Context, coordination.Assignment) error {
	if a.deny {
		return errCompletionFixture
	}
	return nil
}

type completionFixtureSource struct {
	request coordination.TaskCompletionRequest
}

func (s completionFixtureSource) ListTaskCompletionAssignments(context.Context, coordination.TaskCompletionListRequest) (coordination.TaskCompletionPage, error) {
	return coordination.TaskCompletionPage{Assignments: []coordination.ScheduledTaskCompletion{{Scope: s.request.Handoff.Scope, AssignmentID: s.request.AssignmentID}}}, nil
}

type completionFixtureAcceptor struct {
	store coordination.TaskAcceptanceStore
	done  chan struct{}
	once  sync.Once
}

func (a *completionFixtureAcceptor) Accept(ctx context.Context, r coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
	v, err := a.store.AcceptTaskResult(ctx, r)
	if err == nil {
		a.once.Do(func() { close(a.done) })
	}
	return v, err
}

type completionFixtureReviewer struct {
	store         store.Store
	template      coordination.Review
	executions    atomic.Int32
	observations  chan struct{}
	pending, lose bool
	terminal      run.State
	malformed     bool
}

func (r *completionFixtureReviewer) Execute(ctx context.Context, c coordination.TaskCompletion) error {
	r.executions.Add(1)
	if !c.Fresh {
		return errCompletionFixture
	}
	if r.pending {
		return nil
	}
	original, err := r.store.GetRun(ctx, r.template.RunID)
	if err != nil {
		return err
	}
	current := newRun(original.AgentID)
	current.ID = c.Request.RunID
	current.SessionID = c.Request.SessionID
	current.Input = "review input"
	current.Budget = c.Request.Handoff.Authority.Budget
	started := time.Now().UTC()
	current.StartedAt = &started
	if createErr := r.store.CreateSession(ctx, &session.Session{ID: c.Request.SessionID, AgentID: original.AgentID}); createErr != nil {
		return createErr
	}
	if createErr := r.store.CreateRun(ctx, current); createErr != nil {
		return createErr
	}
	now := time.Now().UTC()
	review := r.completionReview(c, current.Revision+1, now)
	output, err := review.Output()
	if err != nil {
		return err
	}
	current.State, current.StopReason, current.Output = run.StateCompleted, run.StopFinalAnswer, string(output)
	current.CompletedAt = &now
	if r.terminal != "" {
		current.State = r.terminal
	}
	if r.malformed {
		current.Output = "malformed review"
	}
	current.Usage.ReportedTokens = int(c.Request.Reservation.Allocation.Tokens) + 7
	current.Provenance = original.Provenance
	current.Provenance.Purpose = run.PurposeEvaluation
	current.Provenance.AssignmentDigest = c.Request.Handoff.Digest
	current.Provenance.PreparedDigest = c.Request.PreparedDigest
	if err := r.store.UpdateRun(ctx, current); err != nil {
		return err
	}
	if r.lose {
		return errCompletionFixture
	}
	return nil
}
func (r *completionFixtureReviewer) completionReview(c coordination.TaskCompletion, revision int64, at time.Time) coordination.Review {
	result := r.template
	result.ID = c.Request.ReviewID
	result.SubjectRevision, result.SubjectDigest = c.Request.AssignmentRevision, c.Request.AssignmentDigest
	result.Handoff = c.Request.Handoff
	result.RunID = c.Request.RunID
	result.RunRevision = revision
	result.PreparedDigest = c.Request.PreparedDigest
	result.ReviewedAt = at
	sealed, err := coordination.SealReview(result)
	if err != nil {
		return coordination.Review{}
	}
	return sealed
}
func (r *completionFixtureReviewer) Observe(ctx context.Context, c coordination.TaskCompletion) (*coordination.Review, error) {
	select {
	case r.observations <- struct{}{}:
	default:
	}
	if r.pending {
		return nil, nil
	}
	current, err := r.store.GetRun(ctx, c.Request.RunID)
	if errors.Is(err, cortex.ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if current.CompletedAt == nil || current.State != run.StateCompleted {
		return nil, nil
	}
	review := r.completionReview(c, current.Revision, *current.CompletedAt)
	return &review, nil
}

// TaskCompletionLifecycle checks restart recovery and uncertain dispatch boundaries.
func TaskCompletionLifecycle(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"complete", "committed_unsubmitted", "abandoned", "lost_execution_ack", "lost_reservation_ack", "pending", "revoked", "failed", "cancelled", "incomplete", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			fixture := NewTaskCompletionFixture(t, s, mode == "committed_unsubmitted")
			ctx, req, accept, review := fixture.Context, fixture.Request, fixture.Acceptance, fixture.Review
			storage := &completionFixtureStore{TaskCompletionServiceStore: resultCapability[coordination.TaskCompletionServiceStore](t, s)}
			storage.lose.Store(mode == "lost_reservation_ack" || mode == "abandoned")
			reviewer := &completionFixtureReviewer{store: s, template: review, observations: make(chan struct{}, 20), pending: mode == "pending", lose: mode == "lost_execution_ack"}
			switch mode {
			case "failed":
				reviewer.terminal = run.StateFailed
			case "cancelled":
				reviewer.terminal = run.StateCancelled
			case "incomplete":
				reviewer.terminal = run.StateIncomplete
			case "malformed":
				reviewer.malformed = true
			}
			acceptor := &completionFixtureAcceptor{store: resultCapability[coordination.TaskAcceptanceStore](t, s), done: make(chan struct{})}
			failures := make(chan error, 20)
			config := coordination.TaskCompletionServiceConfig{Store: storage, Discovery: completionFixtureSource{req}, Resolver: completionFixtureResolver{request: req, accept: accept, submission: &fixture.Submission, abandon: mode == "abandoned"}, Authority: completionFixtureAuthority{deny: mode == "revoked"}, Reviewer: reviewer, Acceptor: acceptor, Interval: time.Millisecond, ReportError: func(err error) {
				select {
				case failures <- err:
				default:
				}
			}}
			if mode == "abandoned" {
				closer, closeErr := coordination.NewTaskCompletionCloser(resultCapability[coordination.TaskCompletionAbandonmentStore](t, s), completionAbandonmentAuthority{})
				require.NoError(t, closeErr)
				config.Abandoner = closer
			}
			service, err := coordination.NewTaskCompletionService(config)
			require.NoError(t, err)
			require.NoError(t, service.Start(ctx))
			defer func() {
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, service.Stop(stopCtx))
			}()
			switch mode {
			case "complete", "committed_unsubmitted", "lost_execution_ack":
				select {
				case <-acceptor.done:
				case <-time.After(5 * time.Second):
					t.Fatal("completion did not finish")
				}
				require.Equal(t, int32(1), reviewer.executions.Load())
			case "failed", "cancelled", "incomplete", "malformed":
				select {
				case <-reviewer.observations:
				case <-time.After(5 * time.Second):
					t.Fatal("terminal reviewer was not reconciled")
				}
				settled, settleErr := storage.GetTaskCompletionSettlement(ctx, req.AssignmentID)
				require.NoError(t, settleErr)
				require.NotNil(t, settled)
				require.Zero(t, settled.Settlement.Request.Usage.Known)
				require.Equal(t, req.Reservation.Allocation.Tokens+7, settled.Settlement.Request.Minimum.Tokens)
				budget, budgetErr := resultCapability[execution.BudgetStore](t, s).GetBudgetSnapshot(ctx, req.Handoff.PlanID)
				require.NoError(t, budgetErr)
				require.GreaterOrEqual(t, budget.Charged.Tokens, req.Reservation.Allocation.Tokens+7)
				select {
				case <-acceptor.done:
					t.Fatal("invalid reviewer result accepted")
				default:
				}
			case "abandoned":
				require.Eventually(t, func() bool {
					closed, closeErr := storage.GetTaskCompletionSettlement(ctx, req.AssignmentID)
					return closeErr == nil && closed != nil && closed.Abandonment != nil
				}, 5*time.Second, time.Millisecond)
				require.Zero(t, reviewer.executions.Load())
				require.NoError(t, service.Stop(context.Background()))
				priorSettles := storage.settles.Load()
				restarted, restartErr := coordination.NewTaskCompletionService(config)
				require.NoError(t, restartErr)
				service = restarted
				require.NoError(t, service.Start(ctx))
				require.Eventually(t, func() bool { return storage.settles.Load() > priorSettles }, time.Second, time.Millisecond)
				require.Zero(t, reviewer.executions.Load())
			case "revoked":
				select {
				case <-failures:
				case <-time.After(5 * time.Second):
					t.Fatal("authority was not checked")
				}
				require.Zero(t, reviewer.executions.Load())
			case "pending", "lost_reservation_ack":
				for range 2 {
					select {
					case <-reviewer.observations:
					case <-time.After(5 * time.Second):
						t.Fatal("pending review was not revisited")
					}
				}
				require.NoError(t, service.Stop(context.Background()))
				restarted, buildErr := coordination.NewTaskCompletionService(config)
				require.NoError(t, buildErr)
				service = restarted
				require.NoError(t, service.Start(ctx))
				select {
				case <-reviewer.observations:
				case <-time.After(5 * time.Second):
					t.Fatal("restart did not reconcile")
				}
				expected := int32(1)
				if mode == "lost_reservation_ack" {
					expected = 0
				}
				require.Equal(t, expected, reviewer.executions.Load())
			}
		})
	}
}
