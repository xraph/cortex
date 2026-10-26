package engine_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/storetest"
)

type integratedCompletionResolver struct {
	fixture storetest.TaskCompletionFixture
}

func (r integratedCompletionResolver) ResolveTaskSubmission(context.Context, coordination.Assignment) (*coordination.Submission, error) {
	return &r.fixture.Submission, nil
}
func (r integratedCompletionResolver) ResolveTaskCompletion(_ context.Context, a coordination.Assignment) (*coordination.TaskCompletionRequest, error) {
	req := r.fixture.Request
	req.AssignmentRevision = a.Revision
	req.AssignmentDigest = a.Digest
	return &req, nil
}
func (r integratedCompletionResolver) ResolveTaskAcceptance(_ context.Context, c coordination.TaskCompletion, a coordination.TaskAdjudication) (*coordination.TaskAcceptanceRequest, error) {
	req := r.fixture.Acceptance
	req.SchemaVersion = 2
	req.ID = c.Request.AcceptanceID
	req.AssignmentRevision = c.Request.AssignmentRevision
	req.AssignmentDigest = c.Request.AssignmentDigest
	req.Adjudication = &coordination.TaskAdjudicationRef{ID: a.Request.ID, Digest: a.Digest}
	return &req, nil
}

type integratedCompletionAuthority struct{}

func (integratedCompletionAuthority) WithTaskCompletionFence(_ context.Context, a coordination.Assignment, next func(coordination.Assignment) error) error {
	return next(a)
}
func (integratedCompletionAuthority) AuthorizeTaskCompletion(context.Context, coordination.Assignment) error {
	return nil
}
func (integratedCompletionAuthority) WithTaskAcceptanceFence(_ context.Context, r coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
	return next(r)
}
func (integratedCompletionAuthority) AuthorizeTaskAcceptance(context.Context, coordination.TaskAcceptanceRequest) error {
	return nil
}

type integratedCompletionAcceptor struct {
	coordination.TaskAcceptor
	done chan struct{}
	once sync.Once
}

func (a *integratedCompletionAcceptor) Accept(ctx context.Context, r coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
	receipt, err := a.TaskAcceptor.Accept(ctx, r)
	if err == nil {
		a.once.Do(func() { close(a.done) })
	}
	return receipt, err
}

func TestTaskCompletionRunsPreparedReviewerFromCommittedProducer(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		name := "submitted"
		if deferred {
			name = "committed_unsubmitted"
		}
		t.Run(name, func(t *testing.T) {
			backend := newSQLiteStoreForTest(t)
			fixture := storetest.NewTaskCompletionFixture(t, backend, deferred)
			ctx := fixture.Context
			client := new(reviewRuntimeLLM)
			e, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithSuspensionTTL(0))
			require.NoError(t, err)
			reviewerID, err := id.Parse(fixture.Request.Handoff.Receiver.Agent)
			require.NoError(t, err)
			budget := fixture.Request.Handoff.Authority.Budget
			prepared, err := e.PrepareAgent(ctx, reviewerID, &engine.RunOverrides{ToolsRestricted: true, Tools: []string{}, Budget: &budget})
			require.NoError(t, err)
			input := "Review the retained task evidence against the required criteria."
			fixture.Request.PreparedDigest = prepared.Digest()
			fixture.Request.InputDigest = evidence.Digest([]byte(input))
			runtimeConfig := engine.TaskReviewRuntimeConfig{
				Resolver: reviewRuntimeResolver(func(_ context.Context, c coordination.TaskCompletion) (engine.TaskReviewWork, error) {
					output, marshalErr := json.Marshal(coordination.ReviewOutput{SchemaVersion: 1, HandoffDigest: c.Request.Handoff.Digest, SubjectDigest: c.Request.AssignmentDigest, BindingDigest: c.Request.BindingDigest, Findings: fixture.Review.Findings})
					if marshalErr != nil {
						return engine.TaskReviewWork{}, marshalErr
					}
					client.output = string(output)
					return engine.TaskReviewWork{Prepared: prepared, Input: input, Contract: fixture.Contract}, nil
				}),
				Fence: reviewRuntimeFence(func(_ context.Context, _ engine.TaskReviewAction, c coordination.TaskCompletion, next func(coordination.TaskCompletion) error) error {
					return next(c)
				}),
				Authorizer:         reviewRuntimeAuthorizer(func(context.Context, engine.TaskReviewAction, coordination.TaskCompletion) error { return nil }),
				EvidenceAuthorizer: captureAuthorizer(func(context.Context, evidence.Action, evidence.Ref) error { return nil }),
			}
			reviewer, err := e.NewTaskReviewRuntime(runtimeConfig)
			require.NoError(t, err)
			authority := integratedCompletionAuthority{}
			guarded, err := coordination.NewTaskAcceptor(backend, authority, authority)
			require.NoError(t, err)
			accepted := &integratedCompletionAcceptor{TaskAcceptor: guarded, done: make(chan struct{})}
			failures := make(chan error, 10)
			config := coordination.TaskCompletionServiceConfig{
				Store: backend, Discovery: completionSource(func(context.Context, coordination.TaskCompletionListRequest) (coordination.TaskCompletionPage, error) {
					return coordination.TaskCompletionPage{Assignments: []coordination.ScheduledTaskCompletion{{Scope: fixture.Plan.Scope, AssignmentID: fixture.Request.AssignmentID}}}, nil
				}),
				Resolver: integratedCompletionResolver{fixture}, Authority: authority, Reviewer: reviewer, Acceptor: accepted, Interval: 10 * time.Millisecond,
				ReportError: func(err error) {
					select {
					case failures <- err:
					default:
					}
				},
			}
			require.NoError(t, engine.WithTaskCompletion(config)(e))
			require.NoError(t, e.Start(ctx))
			t.Cleanup(func() { require.NoError(t, e.Stop(context.Background())) })
			select {
			case <-accepted.done:
			case failure := <-failures:
				t.Fatal(failure)
			case <-time.After(10 * time.Second):
				t.Fatal("prepared reviewer did not finish")
			}
			require.NoError(t, e.Stop(context.Background()))
			require.Equal(t, 1, client.calls)
			receipt, err := backend.GetTaskAcceptance(ctx, fixture.Request.AcceptanceID)
			require.NoError(t, err)
			require.NoError(t, receipt.Validate())
			require.NotNil(t, receipt.Request.Adjudication)
			settlement, err := backend.GetTaskCompletionSettlement(ctx, fixture.Request.AssignmentID)
			require.NoError(t, err)
			require.NotNil(t, settlement)
			require.NoError(t, settlement.Validate())
			// New engine state observes durable acceptance without a second provider call.
			restarted, err := engine.New(engine.WithStore(backend), engine.WithLLM(client), engine.WithSuspensionTTL(0))
			require.NoError(t, err)
			config.Reviewer, err = restarted.NewTaskReviewRuntime(runtimeConfig)
			require.NoError(t, err)
			observed := make(chan struct{}, 1)
			source := config.Discovery
			config.Discovery = completionSource(func(c context.Context, r coordination.TaskCompletionListRequest) (coordination.TaskCompletionPage, error) {
				page, readErr := source.ListTaskCompletionAssignments(c, r)
				select {
				case observed <- struct{}{}:
				default:
				}
				return page, readErr
			})
			require.NoError(t, engine.WithTaskCompletion(config)(restarted))
			require.NoError(t, restarted.Start(ctx))
			<-observed
			require.NoError(t, restarted.Stop(context.Background()))
			require.Equal(t, 1, client.calls)
		})
	}
}
