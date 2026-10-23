package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// VerificationWorkerConfig supplies trusted services for explicit verification
// of saved candidates. The host reserves the job budget and captures evidence
// before calling VerifyRun. This does not gate ordinary run publication.
type VerificationWorkerConfig struct {
	LeaseRenewal *verification.LeaseRenewalPolicy
	Adapter      verification.EvaluationAdapter
	Authorizer   verification.BindingAuthorizer
	Fence        verification.CommitFence
	Clock        verification.Clock
}

// WithVerificationWorker installs an explicit saved-candidate worker. All ports
// and durable store capabilities are mandatory, regardless of option order.
func WithVerificationWorker(config VerificationWorkerConfig) Option {
	return func(e *Engine) error {
		copied := config
		if config.LeaseRenewal != nil {
			policy := *config.LeaseRenewal
			copied.LeaseRenewal = &policy
		}
		e.verificationConfig = &copied
		return nil
	}
}

type verificationWorkerStore interface {
	verification.JobStore
	verification.ExecutorStore
	verification.CommitStore
}

type verificationWorker struct {
	config    VerificationWorkerConfig
	storage   verificationWorkerStore
	executor  *verification.Executor
	committer verification.Committer
}

func (e *Engine) configureVerificationWorker() error {
	if e.verificationConfig == nil {
		return nil
	}
	storage, ok := e.store.(verificationWorkerStore)
	if !ok {
		return cortex.ErrVerificationUnavailable
	}
	config := *e.verificationConfig
	executor, err := verification.NewExecutor(verification.ExecutorConfig{Store: storage, Adapter: config.Adapter, Authorizer: config.Authorizer, Clock: config.Clock, LeaseRenewal: config.LeaseRenewal})
	if err != nil {
		return fmt.Errorf("cortex: configure verification worker: %w", err)
	}
	committer, err := verification.NewCommitter(storage, config.Fence, config.Authorizer)
	if err != nil {
		return fmt.Errorf("cortex: configure verification worker: %w", err)
	}
	e.verificationWorker = &verificationWorker{config: config, storage: storage, executor: executor, committer: committer}
	e.verificationConfig = nil
	return nil
}

// VerifyRunRequest pins an already captured candidate and a stable result ID.
// Retry uncertain work with the same request. After lease expiry, a new ClaimID
// may take over the same job; keep ResultID unchanged when recovering a commit.
type VerifyRunRequest struct {
	Job      verification.JobRequest
	Claim    verification.JobClaim
	ResultID id.ID
}

// VerificationRunResult contains the saved job and, only after guarded commit,
// its historical receipt. Failed, inconclusive and cancelled jobs have no receipt.
// A receipt is not permission for later public delivery; reauthorize that access.
type VerificationRunResult struct {
	Job     verification.Job
	Receipt *verification.CommitReceipt
}

// VerifyRun enqueues, claims, evaluates and commits a saved candidate. It never
// invokes generation or tools. Groups run in contract order until required checks
// settle; optional groups after readiness are skipped. Configured renewal keeps
// evaluator callbacks owned; claim and commit still enforce stored lease expiry.
// An error may follow durable work: retry the same identities to reconcile it.
// Normal sync, stream and resume publication is unaffected by this explicit API.
func (e *Engine) VerifyRun(ctx context.Context, request VerifyRunRequest) (VerificationRunResult, error) {
	if err := ctx.Err(); err != nil {
		return VerificationRunResult{}, err
	}
	if e.verificationWorker == nil {
		return VerificationRunResult{}, cortex.ErrVerificationUnavailable
	}
	return e.verificationWorker.verify(ctx, request)
}

func (w *verificationWorker) verify(ctx context.Context, request VerifyRunRequest) (VerificationRunResult, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return VerificationRunResult{}, err
	}
	frozen, digest, err := request.Job.Freeze()
	if err != nil {
		return VerificationRunResult{}, err
	}
	claim := request.Claim
	token := verification.JobLease{JobID: claim.JobID, ClaimID: claim.ClaimID, Owner: claim.Owner, Generation: 1, ExpiresAt: frozen.Deadline}
	if frozen.EvaluationBudget == nil || scope.Canonical() != frozen.Binding.Scope.Canonical() || claim.JobID != frozen.ID || claim.Duration <= 0 || claim.Duration > verification.MaxJobLease || token.Validate() != nil || request.ResultID.IsNil() || request.ResultID.Prefix() != verification.PrefixResult {
		return VerificationRunResult{}, verification.ErrInvalid
	}
	if authErr := w.authorize(ctx, frozen); authErr != nil {
		return VerificationRunResult{}, authErr
	}
	if intentErr := w.checkIntent(ctx, frozen, request.ResultID); intentErr != nil {
		return VerificationRunResult{}, intentErr
	}
	job, err := w.storage.EnqueueVerificationJob(ctx, frozen)
	if err != nil {
		return VerificationRunResult{}, err
	}
	job, err = checkedWorkerJob(job, digest)
	if err != nil {
		return VerificationRunResult{}, err
	}
	if job.State == verification.JobQueued || job.State == verification.JobRunning || job.State == verification.JobReady {
		job, err = w.storage.ClaimVerificationJob(ctx, claim)
		if err != nil {
			return VerificationRunResult{}, err
		}
		job, err = checkedWorkerJob(job, digest)
		if err != nil {
			return VerificationRunResult{}, err
		}
		if job.Lease == nil || job.Lease.ClaimID != claim.ClaimID || job.Lease.Owner != claim.Owner {
			return VerificationRunResult{}, verification.ErrJobConflict
		}
		job, err = w.evaluate(ctx, job)
		if err != nil {
			return VerificationRunResult{}, err
		}
	}
	if authErr := w.authorize(ctx, frozen); authErr != nil {
		return VerificationRunResult{}, authErr
	}
	switch job.State {
	case verification.JobFailed, verification.JobInconclusive, verification.JobCancelled:
		return VerificationRunResult{Job: job}, nil
	case verification.JobReady, verification.JobCommitted:
		return w.commit(ctx, job, request.ResultID)
	default:
		return VerificationRunResult{}, verification.ErrJobConflict
	}
}

func (w *verificationWorker) checkIntent(ctx context.Context, request verification.JobRequest, resultID id.ID) error {
	reader, ok := w.storage.(interface {
		GetRunCaptureIntent(context.Context, id.AgentRunID) (verification.CaptureIntent, error)
	})
	if !ok {
		return nil
	}
	intent, err := reader.GetRunCaptureIntent(ctx, request.Binding.RunID)
	if errors.Is(err, verification.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if intent.Validate() != nil || intent.Template.Binding.Scope.Canonical() != request.Binding.Scope.Canonical() || intent.Template.Binding.RunID != request.Binding.RunID {
		return verification.ErrInvalid
	}
	if intent.ResultID != resultID {
		return verification.ErrConflict
	}
	return nil
}

func checkedWorkerJob(job verification.Job, digest string) (verification.Job, error) {
	if err := job.Validate(); err != nil {
		return verification.Job{}, err
	}
	if job.RequestDigest != digest {
		return verification.Job{}, verification.ErrConflict
	}
	return verification.SealJob(job)
}

func (w *verificationWorker) authorize(ctx context.Context, request verification.JobRequest) error {
	detached, _, err := request.Freeze()
	if err != nil {
		return err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if err = w.config.Authorizer.AuthorizeVerification(ctx, detached.Binding); err != nil {
		return fmt.Errorf("cortex: authorize verification worker: %w", err)
	}
	return ctx.Err()
}

func (w *verificationWorker) evaluate(ctx context.Context, job verification.Job) (verification.Job, error) {
	if err := job.CheckLease(*job.Lease, w.config.Clock.Now().UTC()); err != nil {
		return verification.Job{}, err
	}
	if job.State == verification.JobReady {
		return job, nil
	}
	contract, err := w.storage.GetContract(ctx, job.Request.ContractID)
	if err != nil {
		return verification.Job{}, err
	}
	if contract.Validate() != nil || contract.ID != job.Request.ContractID || contract.Digest != job.Request.Binding.ContractDigest || contract.Scope.Canonical() != job.Request.Binding.Scope.Canonical() {
		return verification.Job{}, verification.ErrInvalid
	}
	seen := make(map[[2]string]bool)
	for _, criterion := range contract.Criteria {
		group := [2]string{criterion.Verifier, criterion.VerifierRevision}
		if seen[group] {
			continue
		}
		seen[group] = true
		next, executeErr := w.executor.Execute(ctx, verification.ExecutorRequest{Lease: *job.Lease, Verifier: group[0], VerifierRevision: group[1]})
		if executeErr != nil {
			return verification.Job{}, executeErr
		}
		job, err = checkedWorkerJob(next, job.RequestDigest)
		if err != nil {
			return verification.Job{}, err
		}
		if job.State != verification.JobRunning {
			break
		}
	}
	return job, nil
}

func (w *verificationWorker) commit(ctx context.Context, job verification.Job, resultID id.ID) (VerificationRunResult, error) {
	request := verification.CommitRequest{SchemaVersion: 1, ResultID: resultID, ExpectedRunRevision: job.RunRevision, ContractID: job.Request.ContractID, ReportIDs: job.ReportIDs, Current: job.Request.Binding, JobLease: job.Lease}
	if job.State == verification.JobCommitted {
		if job.ResultID != resultID {
			return VerificationRunResult{}, verification.ErrConflict
		}
		receipt, err := w.storage.GetCommittedResult(ctx, resultID)
		if err != nil {
			return VerificationRunResult{}, err
		}
		if validationErr := receipt.Validate(); validationErr != nil {
			return VerificationRunResult{}, validationErr
		}
		stored := receipt.Request
		bindingDigest, err := stored.Current.Digest()
		if err != nil {
			return VerificationRunResult{}, err
		}
		expectedDigest, err := job.Request.Binding.Digest()
		if err != nil {
			return VerificationRunResult{}, err
		}
		if stored.ResultID != resultID || stored.ContractID != request.ContractID || stored.ExpectedRunRevision != job.RunRevision || !slices.Equal(stored.ReportIDs, job.ReportIDs) || bindingDigest != expectedDigest || stored.JobLease == nil || stored.JobLease.JobID != job.Request.ID || stored.JobLease.Generation != job.Generation || len(job.ClaimIDs) == 0 || stored.JobLease.ClaimID != job.ClaimIDs[len(job.ClaimIDs)-1] {
			return VerificationRunResult{}, verification.ErrConflict
		}
		// Original lease and report order are part of the idempotency digest. The
		// committer reauthorizes under the host fence even on historical recovery.
		request = stored
	}
	receipt, err := w.committer.Commit(ctx, request)
	if err != nil {
		return VerificationRunResult{}, err
	}
	saved, err := w.storage.GetVerificationJob(ctx, job.Request.ID)
	if err != nil {
		return VerificationRunResult{}, err
	}
	saved, err = checkedWorkerJob(saved, job.RequestDigest)
	if err != nil {
		return VerificationRunResult{}, err
	}
	if saved.State != verification.JobCommitted || saved.ResultID != resultID {
		return VerificationRunResult{}, verification.ErrConflict
	}
	if authErr := w.authorize(ctx, saved.Request); authErr != nil {
		return VerificationRunResult{}, authErr
	}
	return VerificationRunResult{Job: saved, Receipt: &receipt}, nil
}
