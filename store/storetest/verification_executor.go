package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

type evaluatorAdapter struct {
	estimate  func(context.Context, verification.EvaluationInput) (verification.EvaluationAllowance, error)
	evaluate  func(context.Context, verification.EvaluationDispatch) (verification.CallResult, error)
	reconcile func(context.Context, verification.EvaluationDispatch) (verification.CallResult, error)
}

func (a evaluatorAdapter) Estimate(ctx context.Context, input verification.EvaluationInput) (verification.EvaluationAllowance, error) {
	return a.estimate(ctx, input)
}
func (a evaluatorAdapter) Evaluate(ctx context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
	return a.evaluate(ctx, dispatch)
}
func (a evaluatorAdapter) Reconcile(ctx context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
	return a.reconcile(ctx, dispatch)
}

type evaluatorAuthorizer func(context.Context, verification.Binding) error

func (a evaluatorAuthorizer) AuthorizeVerification(ctx context.Context, binding verification.Binding) error {
	return a(ctx, binding)
}

func executorSelection(job verification.Job, report verification.Report) verification.ExecutorRequest {
	return verification.ExecutorRequest{Lease: *job.Lease, Verifier: report.Outcomes[0].Verifier, VerifierRevision: report.Outcomes[0].VerifierRevision}
}

func testVerificationExecutor(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req, reports := verificationCallFixture(t, s, false)
	js := resultCapability[verification.JobStore](t, s)
	job := claimCallJob(ctx, t, js, req)
	storage := resultCapability[verification.ExecutorStore](t, s)
	before, err := s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	invocations, reconciliations := 0, 0
	adapter := evaluatorAdapter{
		estimate: func(_ context.Context, input verification.EvaluationInput) (verification.EvaluationAllowance, error) {
			return verification.EvaluationAllowance{MaxTokens: 40, MaxUSDMicros: 40, PricingRevision: input.Budget.PricingRevision}, nil
		},
		evaluate: func(callctx context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
			invocations++
			saved, readErr := js.GetVerificationJob(callctx, req.ID)
			require.NoError(t, readErr)
			require.Len(t, saved.Calls, invocations)
			require.Equal(t, dispatch.Call, saved.Calls[invocations-1])
			deadline, ok := callctx.Deadline()
			require.True(t, ok)
			require.False(t, deadline.After(job.Lease.ExpiresAt))
			return verification.CallResult{CallID: dispatch.Call.Request.ID, Report: &reports[invocations-1], Usage: verification.EvaluationUsage{Tokens: 20, TokensKnown: true, USDMicros: 20, MoneyKnown: true, Receipt: "fixture-meter"}}, nil
		},
		reconcile: func(context.Context, verification.EvaluationDispatch) (verification.CallResult, error) {
			reconciliations++
			return verification.CallResult{}, errors.New("unexpected reconciliation")
		},
	}
	executor, err := verification.NewExecutor(verification.ExecutorConfig{Store: storage, Adapter: adapter, Authorizer: evaluatorAuthorizer(func(context.Context, verification.Binding) error { return nil }), Clock: &jobClock{at: time.Now().UTC()}})
	require.NoError(t, err)
	selection := executorSelection(job, reports[0])
	job, err = executor.Execute(ctx, selection)
	require.NoError(t, err)
	require.Equal(t, verification.JobRunning, job.State)
	retry, err := executor.Execute(ctx, selection)
	require.NoError(t, err)
	require.Equal(t, job, retry)
	job, err = executor.Execute(ctx, executorSelection(job, reports[1]))
	require.NoError(t, err)
	require.Equal(t, verification.JobReady, job.State)
	require.Equal(t, 2, invocations)
	require.Zero(t, reconciliations)
	require.Equal(t, verification.EvaluationDebit{Calls: 2, Tokens: 40, USDMicros: 40}, verification.DebitCalls(job.Calls))
	after, err := s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func testVerificationExecutorRenewal(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req, reports := verificationCallFixture(t, s, false)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	js := resultCapability[verification.JobStore](t, s)
	_, err := js.EnqueueVerificationJob(ctx, req)
	require.NoError(t, err)
	job, err := js.ClaimVerificationJob(ctx, verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: 2 * time.Second})
	require.NoError(t, err)
	// Each callback crosses its entry snapshot's expiry while the executor renews.
	// Concurrent reads and competing claims exercise the actual backend fences.
	waitOwned := func(callctx context.Context, pendingCalls int) {
		t.Helper()
		before, readErr := js.GetVerificationJob(callctx, req.ID)
		require.NoError(t, readErr)
		require.Len(t, before.Calls, pendingCalls)
		timer := time.NewTimer(time.Until(before.Lease.ExpiresAt) + 20*time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-callctx.Done():
			t.Fatalf("callback lost ownership: %v", context.Cause(callctx))
		}
		_, claimErr := js.ClaimVerificationJob(callctx, verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "competitor", Duration: time.Second})
		require.ErrorIs(t, claimErr, verification.ErrJobConflict)
		after, readErr := js.GetVerificationJob(callctx, req.ID)
		require.NoError(t, readErr)
		require.Equal(t, job.Generation, after.Generation)
		require.True(t, after.Lease.ExpiresAt.After(before.Lease.ExpiresAt))
	}
	adapter := evaluatorAdapter{
		estimate: func(callctx context.Context, input verification.EvaluationInput) (verification.EvaluationAllowance, error) {
			waitOwned(callctx, 0)
			return verification.EvaluationAllowance{MaxTokens: 40, MaxUSDMicros: 40, PricingRevision: input.Budget.PricingRevision}, nil
		},
		evaluate: func(callctx context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
			waitOwned(callctx, 1)
			return verification.CallResult{CallID: dispatch.Call.Request.ID, Report: &reports[0], Usage: verification.EvaluationUsage{Tokens: 20, TokensKnown: true, USDMicros: 20, MoneyKnown: true, Receipt: "renewal-meter"}}, nil
		},
		reconcile: func(context.Context, verification.EvaluationDispatch) (verification.CallResult, error) {
			t.Fatal("fresh evaluation must not reconcile")
			return verification.CallResult{}, nil
		},
	}
	executor, err := verification.NewExecutor(verification.ExecutorConfig{
		Store: resultCapability[verification.ExecutorStore](t, s), Adapter: adapter,
		Authorizer:   evaluatorAuthorizer(func(context.Context, verification.Binding) error { return nil }),
		Clock:        &jobClock{at: time.Now().UTC()},
		LeaseRenewal: &verification.LeaseRenewalPolicy{Duration: 2 * time.Second, Interval: 100 * time.Millisecond},
	})
	require.NoError(t, err)
	saved, err := executor.Execute(ctx, executorSelection(job, reports[0]))
	require.NoError(t, err)
	require.Equal(t, job.Generation, saved.Generation)
	require.Len(t, saved.Calls, 1)
	require.NotNil(t, saved.Calls[0].Completion)
	require.Equal(t, verification.EvaluationDebit{Calls: 1, Tokens: 20, USDMicros: 20}, verification.DebitCalls(saved.Calls))
}

type executorStorage struct {
	verification.ExecutorStore
	contract func(context.Context, id.ID) (verification.Contract, error)
	begin    func(context.Context, verification.JobLease, verification.CallRequest) (verification.Job, error)
	complete func(context.Context, verification.JobLease, verification.CallResult) (verification.Job, error)
}

func (s executorStorage) GetContract(ctx context.Context, contractID id.ID) (verification.Contract, error) {
	if s.contract != nil {
		return s.contract(ctx, contractID)
	}
	return s.ExecutorStore.GetContract(ctx, contractID)
}

func (s executorStorage) BeginVerificationCall(ctx context.Context, lease verification.JobLease, request verification.CallRequest) (verification.Job, error) {
	if s.begin != nil {
		return s.begin(ctx, lease, request)
	}
	return s.ExecutorStore.BeginVerificationCall(ctx, lease, request)
}
func (s executorStorage) CompleteVerificationCall(ctx context.Context, lease verification.JobLease, result verification.CallResult) (verification.Job, error) {
	if s.complete != nil {
		return s.complete(ctx, lease, result)
	}
	return s.ExecutorStore.CompleteVerificationCall(ctx, lease, result)
}

func testVerificationExecutorDenials(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"revoked before", "revoked after estimate", "revoked after evaluate", "cancel estimate", "cancel evaluate", "wrong pricing", "wrong call", "wrong binding", "partial provider error", "missing dispatch", "expired lease", "expire estimate", "expire authorization"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, req, reports := verificationCallFixture(t, s, false)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			js := resultCapability[verification.JobStore](t, s)
			job := claimCallJob(ctx, t, js, req)
			original := job
			storage := executorStorage{ExecutorStore: resultCapability[verification.ExecutorStore](t, s)}
			clock := &jobClock{at: time.Now().UTC()}
			failure := errors.New("adapter denied or unavailable")
			revoked := mode == "revoked before"
			invocations, authorizations := 0, 0
			if mode == "missing dispatch" {
				storage.begin = func(context.Context, verification.JobLease, verification.CallRequest) (verification.Job, error) {
					return original, nil
				}
			}
			adapter := evaluatorAdapter{
				estimate: func(_ context.Context, input verification.EvaluationInput) (verification.EvaluationAllowance, error) {
					if mode == "revoked after estimate" {
						revoked = true
					}
					if mode == "cancel estimate" {
						cancel()
					}
					if mode == "expire estimate" {
						clock.advance(2 * time.Minute)
					}
					pricing := input.Budget.PricingRevision
					if mode == "wrong pricing" {
						pricing = "unapproved-pricing"
					}
					return verification.EvaluationAllowance{MaxTokens: 40, MaxUSDMicros: 40, PricingRevision: pricing}, nil
				},
				evaluate: func(_ context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
					invocations++
					result := verification.CallResult{CallID: dispatch.Call.Request.ID, Report: &reports[0]}
					if mode == "revoked after evaluate" {
						revoked = true
					}
					if mode == "cancel evaluate" {
						cancel()
					}
					if mode == "wrong call" {
						result.CallID = id.New(verification.PrefixCall)
					}
					if mode == "wrong binding" {
						reports[0].Binding.Context.PolicyRevision = "changed"
						var sealErr error
						reports[0], sealErr = verification.SealReport(reports[0])
						require.NoError(t, sealErr)
					}
					if mode == "partial provider error" {
						return result, failure
					}
					return result, nil
				},
				reconcile: func(context.Context, verification.EvaluationDispatch) (verification.CallResult, error) {
					t.Fatal("unexpected recovery")
					return verification.CallResult{}, nil
				},
			}
			executor, err := verification.NewExecutor(verification.ExecutorConfig{Store: storage, Adapter: adapter, Authorizer: evaluatorAuthorizer(func(context.Context, verification.Binding) error {
				authorizations++
				if mode == "expire authorization" && authorizations == 3 {
					clock.advance(2 * time.Minute)
				}
				if revoked {
					return failure
				}
				return nil
			}), Clock: clock})
			require.NoError(t, err)
			if mode == "expired lease" {
				clock.advance(2 * time.Minute)
			}
			result, err := executor.Execute(ctx, executorSelection(job, reports[0]))
			require.Error(t, err)
			require.Zero(t, result)
			if mode == "partial provider error" {
				require.ErrorIs(t, err, verification.ErrCallUncertain)
				require.ErrorIs(t, err, failure)
			}
			saved, readErr := js.GetVerificationJob(context.WithoutCancel(ctx), req.ID)
			require.NoError(t, readErr)
			require.Empty(t, saved.ReportIDs)
			switch {
			case mode == "expire authorization":
				require.Zero(t, invocations)
				require.Len(t, saved.Calls, 1)
			case invocations == 0:
				require.Equal(t, original, saved)
			default:
				require.Len(t, saved.Calls, 1)
				require.Nil(t, saved.Calls[0].Completion)
				require.Equal(t, int64(40), verification.DebitCalls(saved.Calls).Tokens)
			}
		})
	}
}

func testVerificationExecutorLostAcknowledgements(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"begin", "provider", "settle", "unknown recovery"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, req, reports := verificationCallFixture(t, s, false)
			js := resultCapability[verification.JobStore](t, s)
			job := claimCallJob(ctx, t, js, req)
			raw := resultCapability[verification.ExecutorStore](t, s)
			storage := executorStorage{ExecutorStore: raw}
			failure := errors.New("lost acknowledgement")
			if mode == "begin" {
				storage.begin = func(ctx context.Context, lease verification.JobLease, request verification.CallRequest) (verification.Job, error) {
					saved, err := raw.BeginVerificationCall(ctx, lease, request)
					if err != nil {
						return saved, err
					}
					return verification.Job{}, failure
				}
			}
			if mode == "settle" {
				storage.complete = func(ctx context.Context, lease verification.JobLease, result verification.CallResult) (verification.Job, error) {
					saved, err := raw.CompleteVerificationCall(ctx, lease, result)
					if err != nil {
						return saved, err
					}
					return verification.Job{}, failure
				}
			}
			estimates, evaluations, recoveries := 0, 0, 0
			adapter := evaluatorAdapter{
				estimate: func(_ context.Context, input verification.EvaluationInput) (verification.EvaluationAllowance, error) {
					estimates++
					return verification.EvaluationAllowance{MaxTokens: 40, MaxUSDMicros: 40, PricingRevision: input.Budget.PricingRevision}, nil
				},
				evaluate: func(_ context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
					evaluations++
					if mode == "provider" || mode == "unknown recovery" {
						return verification.CallResult{}, failure
					}
					return verification.CallResult{CallID: dispatch.Call.Request.ID, Report: &reports[0]}, nil
				},
				reconcile: func(_ context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
					recoveries++
					if mode == "begin" || mode == "unknown recovery" {
						return verification.CallResult{}, verification.ErrCallUncertain
					}
					return verification.CallResult{CallID: dispatch.Call.Request.ID, Report: &reports[0]}, nil
				},
			}
			executor, err := verification.NewExecutor(verification.ExecutorConfig{Store: storage, Adapter: adapter, Authorizer: evaluatorAuthorizer(func(context.Context, verification.Binding) error { return nil }), Clock: &jobClock{at: time.Now().UTC()}})
			require.NoError(t, err)
			selection := executorSelection(job, reports[0])
			_, err = executor.Execute(ctx, selection)
			require.ErrorIs(t, err, failure)
			result, err := executor.Execute(ctx, selection)
			require.Equal(t, 1, estimates)
			if mode == "begin" || mode == "unknown recovery" {
				require.ErrorIs(t, err, verification.ErrCallUncertain)
				require.Zero(t, result)
				saved, readErr := js.GetVerificationJob(ctx, req.ID)
				require.NoError(t, readErr)
				require.Nil(t, saved.Calls[0].Completion)
			} else {
				require.NoError(t, err)
				require.Len(t, result.ReportIDs, 1)
			}
			if mode == "begin" {
				require.Zero(t, evaluations)
			} else {
				require.Equal(t, 1, evaluations)
			}
			if mode == "settle" {
				require.Zero(t, recoveries)
			} else {
				require.Equal(t, 1, recoveries)
			}
		})
	}
}

func testVerificationExecutorCallbackIsolation(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req, reports := verificationCallFixture(t, s, false)
	job := claimCallJob(ctx, t, resultCapability[verification.JobStore](t, s), req)
	executor, err := verification.NewExecutor(verification.ExecutorConfig{
		Store: resultCapability[verification.ExecutorStore](t, s), Clock: &jobClock{at: time.Now().UTC()},
		Authorizer: evaluatorAuthorizer(func(_ context.Context, binding verification.Binding) error {
			binding.Scope.Levels[0].Value = "mutated"
			return nil
		}),
		Adapter: evaluatorAdapter{
			estimate: func(_ context.Context, input verification.EvaluationInput) (verification.EvaluationAllowance, error) {
				input.Request.Contract.Criteria[0].Verifier = "mutated"
				input.Request.Binding.Scope.Levels[0].Value = "mutated"
				return verification.EvaluationAllowance{MaxTokens: 40, MaxUSDMicros: 40, PricingRevision: input.Budget.PricingRevision}, nil
			},
			evaluate: func(_ context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
				require.NoError(t, dispatch.Input.Request.Contract.Validate())
				require.Equal(t, req.Binding.Scope.Canonical(), dispatch.Input.Request.Binding.Scope.Canonical())
				dispatch.Call.CriterionIDs[0] = "mutated"
				dispatch.Input.Request.Contract.Criteria[0].ID = "mutated"
				return verification.CallResult{CallID: dispatch.Call.Request.ID, Report: &reports[0]}, nil
			},
			reconcile: func(context.Context, verification.EvaluationDispatch) (verification.CallResult, error) {
				t.Fatal("unexpected reconciliation")
				return verification.CallResult{}, nil
			},
		},
	})
	require.NoError(t, err)
	job, err = executor.Execute(ctx, executorSelection(job, reports[0]))
	require.NoError(t, err)
	require.NoError(t, job.Validate())
	require.Equal(t, reports[0].Outcomes[0].CriterionID, job.Calls[0].CriterionIDs[0])
}

func testVerificationExecutorCachedRevocation(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req, reports := verificationCallFixture(t, s, false)
	js := resultCapability[verification.JobStore](t, s)
	job := claimCallJob(ctx, t, js, req)
	call := callFor(reports[0], 40, 40)
	cs := resultCapability[verification.CallStore](t, s)
	_, err := cs.BeginVerificationCall(ctx, *job.Lease, call)
	require.NoError(t, err)
	job, err = cs.CompleteVerificationCall(ctx, *job.Lease, verification.CallResult{CallID: call.ID, Report: &reports[0]})
	require.NoError(t, err)
	raw := resultCapability[verification.ExecutorStore](t, s)
	denied := errors.New("authority revoked during read")
	revoked := false
	storage := executorStorage{ExecutorStore: raw, contract: func(ctx context.Context, contractID id.ID) (verification.Contract, error) {
		c, readErr := raw.GetContract(ctx, contractID)
		revoked = true
		return c, readErr
	}}
	executor, err := verification.NewExecutor(verification.ExecutorConfig{Store: storage, Adapter: evaluatorAdapter{}, Authorizer: evaluatorAuthorizer(func(context.Context, verification.Binding) error {
		if revoked {
			return denied
		}
		return nil
	}), Clock: &jobClock{at: time.Now().UTC()}})
	require.NoError(t, err)
	result, err := executor.Execute(ctx, executorSelection(job, reports[0]))
	require.ErrorIs(t, err, denied)
	require.Zero(t, result)
}

// VerificationExecutorRestart recovers a retained provider outcome after the
// database is closed and reopened, without a second evaluator invocation.
func VerificationExecutorRestart(t *testing.T, s store.Store, reopen func() store.Store) {
	t.Helper()
	ctx, req, reports := verificationCallFixture(t, s, false)
	job := claimCallJob(ctx, t, resultCapability[verification.JobStore](t, s), req)
	estimates, evaluations, recoveries := 0, 0, 0
	var retained verification.CallResult
	adapter := evaluatorAdapter{
		estimate: func(_ context.Context, input verification.EvaluationInput) (verification.EvaluationAllowance, error) {
			estimates++
			return verification.EvaluationAllowance{MaxTokens: 40, MaxUSDMicros: 40, PricingRevision: input.Budget.PricingRevision}, nil
		},
		evaluate: func(_ context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
			evaluations++
			retained = verification.CallResult{CallID: dispatch.Call.Request.ID, Report: &reports[0]}
			return verification.CallResult{}, errors.New("lost provider response")
		},
		reconcile: func(_ context.Context, dispatch verification.EvaluationDispatch) (verification.CallResult, error) {
			recoveries++
			require.Equal(t, retained.CallID, dispatch.Call.Request.ID)
			require.Equal(t, req.Deadline, dispatch.Input.Request.Deadline)
			return retained, nil
		},
	}
	config := verification.ExecutorConfig{Store: resultCapability[verification.ExecutorStore](t, s), Adapter: adapter, Authorizer: evaluatorAuthorizer(func(context.Context, verification.Binding) error { return nil }), Clock: &jobClock{at: time.Now().UTC()}}
	executor, err := verification.NewExecutor(config)
	require.NoError(t, err)
	selection := executorSelection(job, reports[0])
	_, err = executor.Execute(ctx, selection)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	restored := reopen()
	config.Store = resultCapability[verification.ExecutorStore](t, restored)
	executor, err = verification.NewExecutor(config)
	require.NoError(t, err)
	job, err = executor.Execute(ctx, selection)
	require.NoError(t, err)
	require.Len(t, job.ReportIDs, 1)
	require.Equal(t, 1, estimates)
	require.Equal(t, 1, evaluations)
	require.Equal(t, 1, recoveries)
}
