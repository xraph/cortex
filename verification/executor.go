package verification

import (
	"context"
	"fmt"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ExecutorStore is the durable subset used by the evaluator executor.
type ExecutorStore interface {
	GetVerificationJob(context.Context, id.ID) (Job, error)
	GetContract(context.Context, id.ID) (Contract, error)
	BeginVerificationCall(context.Context, JobLease, CallRequest) (Job, error)
	CompleteVerificationCall(context.Context, JobLease, CallResult) (Job, error)
}

// EvaluationInput pins the captured candidate and host accounting policy.
// Request.Deadline remains the job deadline across recovery; each operation's
// context is additionally bounded by the current lease and caller deadline.
type EvaluationInput struct {
	JobID   id.ID
	Request Request
	Budget  EvaluationBudget
}

// EvaluationAllowance is a trusted upper bound for one verifier group.
// PricingRevision must match the job, or be empty in explicit token-only mode.
type EvaluationAllowance struct {
	MaxTokens       int64
	MaxUSDMicros    int64
	PricingRevision string
}

// EvaluationDispatch carries the persisted call identity and original allowance.
// Adapters use Call.Request.ID as the stable provider operation key.
type EvaluationDispatch struct {
	Input EvaluationInput
	Call  EvaluationCall
}

// EvaluationAdapter supplies qualified evaluator access and authenticated usage.
// Estimate must use trusted pricing and bounds without billable provider work.
// Evaluate must enforce the allowance and context deadline, retain a recoverable
// result under the call ID, and never run original agent generation or tool effects.
// Reconcile reads that original outcome without billable work or another
// evaluator invocation. Adapters must reauthorize actual resource reads.
// A missing or still-running result returns ErrCallUncertain. Returning an error
// never authorizes a report or refund, even if the result also contains data.
// A definitive evaluator failure uses CallResult.Failure with a nil error.
type EvaluationAdapter interface {
	Estimate(context.Context, EvaluationInput) (EvaluationAllowance, error)
	Evaluate(context.Context, EvaluationDispatch) (CallResult, error)
	Reconcile(context.Context, EvaluationDispatch) (CallResult, error)
}

// ExecutorConfig requires durable storage and explicit trusted host services.
type ExecutorConfig struct {
	Store      ExecutorStore
	Adapter    EvaluationAdapter
	Authorizer BindingAuthorizer
	Clock      Clock
}

// Executor advances one verifier group under an existing lease. It neither
// claims work nor commits/publishes final results. An error may follow durable
// dispatch or settlement; inspect the saved job before deciding what to do next.
type Executor struct{ config ExecutorConfig }

// ExecutorRequest selects one exact contract-defined verifier group.
type ExecutorRequest struct {
	Lease            JobLease
	Verifier         string
	VerifierRevision string
}

// NewExecutor validates the mandatory host services.
func NewExecutor(config ExecutorConfig) (*Executor, error) {
	if nilHostPort(config.Store) || nilHostPort(config.Adapter) || nilHostPort(config.Authorizer) || nilHostPort(config.Clock) {
		return nil, ErrInvalid
	}
	return &Executor{config: config}, nil
}

// Execute dispatches fresh work or reconciles an already recorded call.
func (e *Executor) Execute(ctx context.Context, request ExecutorRequest) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if request.Lease.Validate() != nil || !callText(request.Verifier, 256) || !callText(request.VerifierRevision, 256) {
		return Job{}, ErrInvalid
	}
	job, err := e.load(ctx, request.Lease, "")
	if err != nil {
		return Job{}, err
	}
	deadline := job.Request.Deadline
	if job.Lease.ExpiresAt.Before(deadline) {
		deadline = job.Lease.ExpiresAt
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if checkErr := e.authorizeLease(ctx, job, request.Lease); checkErr != nil {
		return Job{}, checkErr
	}
	contract, err := e.config.Store.GetContract(ctx, job.Request.ContractID)
	if err != nil {
		return Job{}, err
	}
	if contract.Validate() != nil || contract.ID != job.Request.ContractID || contract.Digest != job.Request.Binding.ContractDigest || contract.Scope.Canonical() != job.Request.Binding.Scope.Canonical() {
		return Job{}, ErrInvalid
	}
	input, err := cloneEvaluationInput(EvaluationInput{JobID: job.Request.ID, Request: Request{Contract: contract, Binding: job.Request.Binding, Deadline: job.Request.Deadline, Verifier: request.Verifier, VerifierRevision: request.VerifierRevision}, Budget: *job.Request.EvaluationBudget})
	if err != nil {
		return Job{}, err
	}
	criteria := groupCriteria(contract, request.Verifier, request.VerifierRevision)
	if len(criteria) == 0 {
		return Job{}, ErrInvalid
	}
	index := slices.IndexFunc(job.Calls, func(call EvaluationCall) bool {
		return call.Request.Verifier == request.Verifier && call.Request.VerifierRevision == request.VerifierRevision
	})
	recovering := index >= 0
	if recovering && job.Calls[index].Completion != nil {
		if checkErr := e.authorizeLease(ctx, job, request.Lease); checkErr != nil {
			return Job{}, checkErr
		}
		return job, nil
	}
	if !recovering {
		if job.State != JobRunning {
			return Job{}, ErrJobConflict
		}
		job, index, err = e.begin(ctx, request.Lease, job, input, criteria)
		if err != nil {
			return Job{}, err
		}
	}
	call := job.Calls[index]
	// Refresh ownership after estimation/begin and immediately before adapter access.
	job, err = e.load(ctx, request.Lease, job.RequestDigest)
	if err != nil {
		return Job{}, err
	}
	index = slices.IndexFunc(job.Calls, func(saved EvaluationCall) bool { return saved.Request.ID == call.Request.ID })
	if index < 0 || job.Calls[index].Request != call.Request || !slices.Equal(job.Calls[index].CriterionIDs, criteria) {
		return Job{}, ErrConflict
	}
	if checkErr := e.authorizeLease(ctx, job, request.Lease); checkErr != nil {
		return Job{}, checkErr
	}
	if job.Calls[index].Completion != nil {
		return job, nil
	}
	detached, err := cloneEvaluationInput(input)
	if err != nil {
		return Job{}, err
	}
	call.CriterionIDs = append([]string(nil), call.CriterionIDs...)
	dispatch := EvaluationDispatch{Input: detached, Call: call}
	var result CallResult
	if recovering {
		result, err = e.config.Adapter.Reconcile(ctx, dispatch)
	} else {
		result, err = e.config.Adapter.Evaluate(ctx, dispatch)
	}
	if err != nil {
		return Job{}, fmt.Errorf("%w: %w", ErrCallUncertain, err)
	}
	if checkErr := ctx.Err(); checkErr != nil {
		return Job{}, checkErr
	}
	if checkErr := e.authorizeLease(ctx, job, request.Lease); checkErr != nil {
		return Job{}, checkErr
	}
	return e.complete(ctx, request.Lease, job, input, call.Request.ID, criteria, result)
}

func (e *Executor) load(ctx context.Context, lease JobLease, digest string) (Job, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return Job{}, err
	}
	job, err := e.config.Store.GetVerificationJob(ctx, lease.JobID)
	if err != nil {
		return Job{}, err
	}
	if job.Validate() != nil || job.Request.ID != lease.JobID || job.Request.Binding.Scope.Canonical() != scope.Canonical() || job.Request.EvaluationBudget == nil || (digest != "" && digest != job.RequestDigest) {
		return Job{}, ErrInvalid
	}
	if checkErr := job.CheckLease(lease, e.config.Clock.Now().UTC()); checkErr != nil {
		return Job{}, checkErr
	}
	if checkErr := ctx.Err(); checkErr != nil {
		return Job{}, checkErr
	}
	return SealJob(job)
}

func (e *Executor) authorizeLease(ctx context.Context, job Job, lease JobLease) error {
	if err := e.authorize(ctx, job.Request.Binding); err != nil {
		return err
	}
	return job.CheckLease(lease, e.config.Clock.Now().UTC())
}

func (e *Executor) authorize(ctx context.Context, binding Binding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	detached, err := cloneBinding(binding)
	if err != nil {
		return err
	}
	if err = e.config.Authorizer.AuthorizeVerification(ctx, detached); err != nil {
		return fmt.Errorf("cortex: authorize evaluator: %w", err)
	}
	return ctx.Err()
}

func (e *Executor) begin(ctx context.Context, lease JobLease, job Job, input EvaluationInput, criteria []string) (Job, int, error) {
	detached, err := cloneEvaluationInput(input)
	if err != nil {
		return Job{}, 0, err
	}
	allowance, err := e.config.Adapter.Estimate(ctx, detached)
	if err != nil {
		return Job{}, 0, err
	}
	if checkErr := e.authorizeLease(ctx, job, lease); checkErr != nil {
		return Job{}, 0, checkErr
	}
	if allowance.PricingRevision != input.Budget.PricingRevision {
		return Job{}, 0, ErrConflict
	}
	req := CallRequest{ID: id.New(PrefixCall), Verifier: input.Request.Verifier, VerifierRevision: input.Request.VerifierRevision, MaxTokens: allowance.MaxTokens, MaxUSDMicros: allowance.MaxUSDMicros}
	if checkErr := input.Budget.Admit(DebitCalls(job.Calls), req); checkErr != nil {
		return Job{}, 0, checkErr
	}
	saved, err := e.config.Store.BeginVerificationCall(ctx, lease, req)
	if err != nil {
		return Job{}, 0, err
	}
	if checkErr := ctx.Err(); checkErr != nil {
		return Job{}, 0, checkErr
	}
	if saved.Validate() != nil || saved.RequestDigest != job.RequestDigest || saved.Request.ID != job.Request.ID {
		return Job{}, 0, ErrInvalid
	}
	if checkErr := saved.CheckLease(lease, e.config.Clock.Now().UTC()); checkErr != nil {
		return Job{}, 0, checkErr
	}
	index := slices.IndexFunc(saved.Calls, func(call EvaluationCall) bool { return call.Request.ID == req.ID })
	if index < 0 || saved.Calls[index].Request != req || saved.Calls[index].Generation != lease.Generation || saved.Calls[index].Completion != nil || !slices.Equal(saved.Calls[index].CriterionIDs, criteria) {
		return Job{}, 0, ErrConflict
	}
	return saved, index, nil
}

func (e *Executor) complete(ctx context.Context, lease JobLease, job Job, input EvaluationInput, callID id.ID, criteria []string, result CallResult) (Job, error) {
	if result.CallID != callID {
		return Job{}, ErrConflict
	}
	frozen, completion, err := result.Freeze()
	if err != nil {
		return Job{}, err
	}
	if frozen.Report != nil {
		report := *frozen.Report
		if ValidateReportForContract(input.Request.Contract, report) != nil || CheckFreshness(report, input.Request.Binding, e.config.Clock.Now().UTC()) != StatusPassed || len(report.Outcomes) != len(criteria) {
			return Job{}, ErrConflict
		}
		for _, outcome := range report.Outcomes {
			if !slices.Contains(criteria, outcome.CriterionID) {
				return Job{}, ErrConflict
			}
		}
	}
	saved, err := e.config.Store.CompleteVerificationCall(ctx, lease, frozen)
	if err != nil {
		return Job{}, err
	}
	if saved.Validate() != nil || saved.RequestDigest != job.RequestDigest || saved.Request.ID != job.Request.ID {
		return Job{}, ErrInvalid
	}
	index := slices.IndexFunc(saved.Calls, func(call EvaluationCall) bool { return call.Request.ID == callID })
	if index < 0 || saved.Calls[index].Completion == nil || *saved.Calls[index].Completion != completion {
		return Job{}, ErrConflict
	}
	if checkErr := e.authorize(ctx, input.Request.Binding); checkErr != nil {
		return Job{}, checkErr
	}
	return SealJob(saved)
}

func groupCriteria(contract Contract, verifier, revision string) []string {
	var criteria []string
	for _, criterion := range contract.Criteria {
		if criterion.Verifier == verifier && criterion.VerifierRevision == revision {
			criteria = append(criteria, criterion.ID)
		}
	}
	return criteria
}

func cloneEvaluationInput(input EvaluationInput) (EvaluationInput, error) {
	contract, err := SealContract(input.Request.Contract)
	if err != nil {
		return EvaluationInput{}, err
	}
	binding, err := cloneBinding(input.Request.Binding)
	if err != nil {
		return EvaluationInput{}, err
	}
	input.Request.Contract, input.Request.Binding = contract, binding
	return input, nil
}
