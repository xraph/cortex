package engine

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// VerificationRecoveryRequest bounds one exact-scope scan. Owner identifies this
// worker; live leases are respected even when their owner string matches.
type VerificationRecoveryRequest struct {
	Owner         string
	LeaseDuration time.Duration
	After         id.ID
	Limit         int
}

// VerificationRecoveryStatus describes recovery work, not public delivery rights.
type VerificationRecoveryStatus string

const (
	RecoveryCommitted   VerificationRecoveryStatus = "committed"
	RecoveryTerminal    VerificationRecoveryStatus = "terminal"
	RecoveryBusy        VerificationRecoveryStatus = "busy"
	RecoveryUnmanaged   VerificationRecoveryStatus = "unmanaged"
	RecoveryExpired     VerificationRecoveryStatus = "expired"
	RecoveryUncertain   VerificationRecoveryStatus = "uncertain"
	RecoveryUnavailable VerificationRecoveryStatus = "unavailable"
)

// VerificationRecoveryItem contains metadata only. State is the latest validated
// snapshot observed by this pass; an unavailable operation may have committed.
type VerificationRecoveryItem struct {
	JobID    id.ID                      `json:"job_id"`
	RunID    id.AgentRunID              `json:"run_id"`
	ResultID id.ID                      `json:"result_id,omitempty"`
	State    verification.JobState      `json:"state"`
	Status   VerificationRecoveryStatus `json:"status"`
}

// VerificationRecoveryPage advances past every inspected job, including busy or
// denied jobs. A zero NextAfter starts the next full scan, avoiding starvation
// behind old terminal records without requiring an unbounded in-memory queue.
type VerificationRecoveryPage struct {
	Items     []VerificationRecoveryItem `json:"items"`
	NextAfter id.ID                      `json:"next_after,omitempty"`
}

// VerificationRecoveryLoop runs one sequential page per interval. Observe is an
// optional synchronous host callback for metadata; it must return promptly.
// Cancellation is joined through the caller rather than a detached goroutine.
type VerificationRecoveryLoop struct {
	Request  VerificationRecoveryRequest
	Interval time.Duration
	Observe  func(VerificationRecoveryPage)
}

func (r VerificationRecoveryRequest) validate() error {
	token := verification.JobLease{JobID: id.New(verification.PrefixJob), ClaimID: id.New(verification.PrefixJobClaim), Owner: r.Owner, Generation: 1, ExpiresAt: time.Unix(1, 0)}
	if token.Validate() != nil || r.LeaseDuration <= 0 || r.LeaseDuration > verification.MaxJobLease || r.Limit < 1 || r.Limit > 256 || (!r.After.IsNil() && r.After.Prefix() != verification.PrefixJob) {
		return verification.ErrInvalid
	}
	return nil
}

// RecoverVerificationPage discovers captured work and reuses VerifyRun to resume
// checks. It never generates, dispatches tools or invents a result identity. Jobs
// without a saved capture intent remain managed through explicit VerifyRun calls.
// The host authorizes exact-scope discovery before calling this trusted API;
// each job also passes the configured current binding authorizer before work.
func (e *Engine) RecoverVerificationPage(ctx context.Context, request VerificationRecoveryRequest) (VerificationRecoveryPage, error) {
	if err := ctx.Err(); err != nil {
		return VerificationRecoveryPage{}, err
	}
	if err := request.validate(); err != nil {
		return VerificationRecoveryPage{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return VerificationRecoveryPage{}, err
	}
	w := e.verificationWorker
	if w == nil {
		return VerificationRecoveryPage{}, cortex.ErrVerificationUnavailable
	}
	intents, ok := w.storage.(verification.CaptureIntentStore)
	if !ok {
		return VerificationRecoveryPage{}, cortex.ErrVerificationUnavailable
	}
	jobs, err := w.storage.ListVerificationJobs(ctx, verification.JobListRequest{After: request.After, Limit: request.Limit})
	if contextErr := ctx.Err(); contextErr != nil {
		return VerificationRecoveryPage{}, contextErr
	}
	if err != nil {
		return VerificationRecoveryPage{}, cortex.ErrVerificationUnavailable
	}
	if len(jobs) > request.Limit {
		return VerificationRecoveryPage{}, cortex.ErrVerificationUnavailable
	}
	jobs = slices.Clone(jobs)
	previous := request.After.String()
	// Validate and detach the whole page before any job can dispatch paid work.
	for i, job := range jobs {
		if job.Request.Binding.Scope.Canonical() != scope.Canonical() || job.Request.ID.String() <= previous {
			return VerificationRecoveryPage{}, cortex.ErrVerificationUnavailable
		}
		jobs[i], err = checkedWorkerJob(job, job.RequestDigest)
		if err != nil {
			return VerificationRecoveryPage{}, cortex.ErrVerificationUnavailable
		}
		previous = job.Request.ID.String()
	}
	page := VerificationRecoveryPage{Items: make([]VerificationRecoveryItem, 0, len(jobs)), NextAfter: request.After}
	for _, job := range jobs {
		if contextErr := ctx.Err(); contextErr != nil {
			return page, contextErr
		}
		item := w.recoverJob(ctx, intents, job, request)
		page.Items = append(page.Items, item)
		page.NextAfter = job.Request.ID
		if contextErr := ctx.Err(); contextErr != nil {
			return page, contextErr
		}
	}
	if len(jobs) < request.Limit {
		page.NextAfter = id.ID{}
	}
	return page, nil
}

func (w *verificationWorker) recoverJob(ctx context.Context, intents verification.CaptureIntentStore, job verification.Job, request VerificationRecoveryRequest) VerificationRecoveryItem {
	item := VerificationRecoveryItem{JobID: job.Request.ID, RunID: job.Request.Binding.RunID, State: job.State, Status: RecoveryUnavailable}
	if w.authorize(ctx, job.Request) != nil {
		return item
	}
	switch job.State {
	case verification.JobCommitted, verification.JobFailed, verification.JobInconclusive, verification.JobCancelled:
		item.Status = RecoveryTerminal
		return item
	}
	intent, err := intents.GetRunCaptureIntent(ctx, job.Request.Binding.RunID)
	if errors.Is(err, verification.ErrNotFound) {
		item.Status = RecoveryUnmanaged
		return item
	}
	if err != nil || intent.Validate() != nil || intent.Template.ID != job.Request.ID || intent.Template.Binding.RunID != job.Request.Binding.RunID || intent.Template.Binding.Scope.Canonical() != job.Request.Binding.Scope.Canonical() {
		return item
	}
	now := w.config.Clock.Now().UTC()
	if now.IsZero() || now.Before(job.UpdatedAt) {
		return item
	}
	if job.Request.Deadline.After(now) && job.Lease != nil && job.Lease.ExpiresAt.After(now) {
		item.Status = RecoveryBusy
		return item
	}
	if !job.Request.Deadline.After(now) || job.Attempts >= job.Request.MaxAttempts {
		expired, expireErr := w.storage.ExpireVerificationJob(ctx, job.Request.ID)
		if expireErr == nil {
			expired, expireErr = checkedWorkerJob(expired, job.RequestDigest)
			if expireErr == nil && expired.State == verification.JobInconclusive {
				item.State, item.Status = expired.State, RecoveryExpired
			}
		}
		return item
	}
	result, err := w.verify(ctx, VerifyRunRequest{Job: job.Request, ResultID: intent.ResultID, Claim: verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: request.Owner, Duration: request.LeaseDuration}})
	if err != nil {
		switch {
		case errors.Is(err, verification.ErrCallUncertain):
			item.Status = RecoveryUncertain
		case errors.Is(err, verification.ErrJobConflict):
			item.Status = RecoveryBusy
		}
		return item
	}
	item.State = result.Job.State
	item.Status = RecoveryTerminal
	if result.Receipt != nil {
		item.Status, item.ResultID = RecoveryCommitted, result.Receipt.Request.ResultID
	}
	return item
}

// RunVerificationRecovery runs immediate and periodic bounded scans until
// cancellation or a discovery failure. It retains the cursor between pages and
// wraps after reaching the end. Restart starts from the supplied cursor; durable
// job leases and call IDs prevent repeating paid evaluation after uncertainty.
// This loop does not renew leases, publish history or replay completion hooks.
func (e *Engine) RunVerificationRecovery(ctx context.Context, config VerificationRecoveryLoop) error {
	if config.Interval <= 0 || config.Interval > time.Hour {
		return verification.ErrInvalid
	}
	request := config.Request
	for {
		page, err := e.RecoverVerificationPage(ctx, request)
		if err != nil {
			return err
		}
		request.After = page.NextAfter
		if config.Observe != nil {
			config.Observe(page)
		}
		timer := time.NewTimer(config.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
