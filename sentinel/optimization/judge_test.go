package optimization

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/sentinel/scorer"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type judgeRepository struct {
	opt.Repository
	trial opt.Trial
}

func (r *judgeRepository) GetTrial(context.Context, id.ID) (opt.Trial, error) { return r.trial, nil }

type judgeAuthority struct{ deny bool }

func (a *judgeAuthority) Authorize(context.Context, opt.Action, opt.Resource) error {
	if a.deny {
		return opt.ErrPolicyDenied
	}
	return nil
}
func (*judgeAuthority) Resolve(ctx context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return ctx, nil
}

type judgeProvider struct {
	calls int
	fail  bool
}

func (p *judgeProvider) Complete(_ context.Context, r *llm.Request) (*llm.Response, error) {
	p.calls++
	if r.MaxTokens != 100 || !strings.Contains(r.Messages[0].Content, "private rubric") {
		return nil, opt.ErrInvalidSpec
	}
	if p.fail {
		return nil, errors.New("lost response")
	}
	return &llm.Response{Model: "fixture", Content: `{"score":0.8,"reason":"matches"}`, FinishReason: "stop", Usage: llm.Usage{TotalTokens: 12}}, nil
}
func (*judgeProvider) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, opt.ErrUnavailable
}

type judgeCounter struct{}

func (judgeCounter) Bound(context.Context, *llm.Request) (opt.InputTokenBound, error) {
	return opt.InputTokenBound{Tokens: 50, Model: "fixture", ModelRevision: "1", Bounded: true}, nil
}

func judgeFixture(t *testing.T, budget int64) (context.Context, *MeteredJudge, *judgeRepository, *judgeAuthority, *judgeProvider, opt.EvaluationRequest) {
	t.Helper()
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	bundle := storetest.Fixture(t, "judge")
	bundle.Experiment.Spec.Budget = opt.Budget{TokenOnly: true, MaxTokens: budget}
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	var lease opt.WorkLease
	err = repo.Transact(ctx, func(tx opt.Transaction) error {
		if e := tx.CreateExperiment(bundle); e != nil {
			return e
		}
		if e := tx.CommitTransition(opt.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: opt.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}); e != nil {
			return e
		}
		var e error
		lease, e = tx.ClaimWork(opt.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: "judge-worker", Duration: time.Minute})
		return e
	})
	if err != nil {
		t.Fatal(err)
	}
	exp, err := repo.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	task := exp.Spec.Tasks[0]
	execution := opt.ExecutionRecord{Digest: strings.Repeat("b", 64), Environment: opt.EnvironmentReceipt{AttemptID: id.New(opt.PrefixAttempt)}}
	trial := opt.Trial{ID: id.New(opt.PrefixTrial), ExperimentID: exp.ID, CaseID: task.ID, CaseDigest: task.Digest, Execution: &execution}
	repository := &judgeRepository{Repository: repo, trial: trial}
	auth := &judgeAuthority{}
	provider := &judgeProvider{}
	judge, err := NewMeteredJudge(JudgeConfig{Name: "quality", Repository: repository, Authorizer: auth, IdentityResolver: auth, Model: opt.ModelBinding{Model: "fixture", Revision: "1", Provider: provider, Counter: judgeCounter{}}, MaxInputBytes: 4096, MaxResponseBytes: 4096, MaxOutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	request := opt.EvaluationRequest{ExperimentID: exp.ID, TrialID: trial.ID, CaseID: task.ID, CaseDigest: task.Digest, Execution: execution, Plan: exp.EvaluationPlan, Lease: lease}
	return ctx, judge, repository, auth, provider, request
}
func scoreJudge(ctx context.Context, j *MeteredJudge, r opt.EvaluationRequest) (*scorer.Output, error) {
	return (&judgeScorer{judge: j, rubric: "private rubric"}).Score(context.WithValue(ctx, judgeContextKey{}, judgeContext{request: r}), &scorer.Input{Actual: r.Execution.Outcome.Output})
}

func TestJudgeChargesOnceAndRejectsUnfencedCalls(t *testing.T) {
	ctx, j, repo, auth, provider, r := judgeFixture(t, 1000)
	stale := r
	stale.Lease.Generation++
	if _, err := scoreJudge(ctx, j, stale); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("stale lease: %v", err)
	}
	auth.deny = true
	if _, err := scoreJudge(ctx, j, r); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("revoked caller: %v", err)
	}
	auth.deny = false
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := scoreJudge(cancelled, j, r); err == nil {
		t.Fatal("cancelled request admitted")
	}
	if provider.calls != 0 {
		t.Fatal("denied request dispatched")
	}
	result, err := scoreJudge(ctx, j, r)
	if err != nil || result == nil || result.Score != .8 {
		t.Fatalf("score: %+v %v", result, err)
	}
	saved, err := repo.GetReservation(ctx, r.ExperimentID, j.callKey(r))
	if err != nil {
		t.Fatal(err)
	}
	if saved.Call == nil || saved.Request.Operation != opt.OperationJudge || saved.DebitedTokens != 150 || saved.State != opt.ReservationUnknown {
		t.Fatalf("unknown accounting released: %+v", saved)
	}
	if _, err = scoreJudge(ctx, j, r); !errors.Is(err, opt.ErrUnknownAttempt) {
		t.Fatalf("replay: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("provider called %d times", provider.calls)
	}
}

func TestJudgeBudgetFailureNeverCallsProvider(t *testing.T) {
	ctx, j, _, _, provider, r := judgeFixture(t, 149)
	if _, err := scoreJudge(ctx, j, r); !errors.Is(err, opt.ErrBudgetExceeded) {
		t.Fatalf("budget: %v", err)
	}
	if provider.calls != 0 {
		t.Fatal("budget failure dispatched")
	}
}

type judgeReceiptProvider struct{}

func (judgeReceiptProvider) Recover(_ context.Context, r opt.Reservation) (opt.VerifiedUsageReceipt, error) {
	return opt.VerifiedUsageReceipt{RequestDigest: r.Call.RequestDigest, Settlement: opt.UsageSettlement{ExperimentID: r.Request.ExperimentID, AttemptID: r.Request.AttemptID, CallKey: r.Request.CallKey, Receipt: "late-provider-receipt", Usage: opt.Usage{Tokens: 12, TokenQuality: opt.UsageKnown, Receipt: "late-provider-receipt"}, Complete: true}}, nil
}

func TestJudgeLostAnswerAndLateReceiptRemainSeparate(t *testing.T) {
	ctx, j, repo, auth, provider, r := judgeFixture(t, 1000)
	provider.fail = true
	if result, err := scoreJudge(ctx, j, r); err == nil || result != nil {
		t.Fatal("lost answer became a score")
	}
	exp, err := repo.GetExperiment(ctx, r.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	err = repo.Transact(ctx, func(tx opt.Transaction) error {
		return tx.CommitTransition(opt.Transition{ExperimentID: exp.ID, ExpectedRevision: exp.Revision, NextState: opt.StatePaused, IdempotencyKey: "pause", Actor: exp.Spec.Requester})
	})
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := opt.NewUsageRecovery(repo, auth, auth, judgeReceiptProvider{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = recovery.Recover(ctx, r.ExperimentID, j.callKey(r), exp.Spec.Requester); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := repo.GetReservation(ctx, r.ExperimentID, j.callKey(r))
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != opt.ReservationSettled || saved.DebitedTokens != 12 {
		t.Fatalf("late receipt not settled: %+v", saved)
	}
	if result, scoreErr := scoreJudge(ctx, j, r); scoreErr == nil || result != nil {
		t.Fatal("receipt fabricated answer")
	}
	if provider.calls != 1 {
		t.Fatal("recovery replayed judge")
	}
}

func TestJudgeOutputRejectsMissingMalformedAndTruncatedScores(t *testing.T) {
	for _, body := range []string{`{}`, `{"score":null}`, `{"score":-1}`, `{"score":1.1}`, `{"score":1} {}`, `{"score":1,"extra":true}`, `not JSON`, `{"score":0,"score":1}`} {
		if _, err := parseJudgment(&llm.Response{Content: body, FinishReason: "stop"}); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if _, err := parseJudgment(&llm.Response{Content: `{"score":1}`, FinishReason: "length"}); err == nil {
		t.Fatal("accepted truncated answer")
	}
}
