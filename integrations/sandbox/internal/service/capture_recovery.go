package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// CaptureRecoveryConfig supplies independent cleanup authority. Authorize must
// permit deletion and tombstone audit for Actor under the signed job's subject.
// It must not depend on the original execution grant remaining valid. Repository
// deletion must fence delayed uploads, including identities with no stored bytes.
// Every callback must honor its context. Audit is trusted repository metadata.
type CaptureRecoveryConfig struct {
	Store      *workerstore.Store
	Repository evidence.Repository
	Audit      evidence.TombstoneReader
	Authorize  func(context.Context, sandbox.Job, evidence.Ref) error
	Actor      string
	PageSize   int
	Now        func() time.Time
}

// CaptureRecoveryReport counts observations, not unique lifetime events.
// Examined, Deferred and Unresolved count jobs; other fields count artifacts.
type CaptureRecoveryReport struct{ Examined, Deferred, Retained, Discarded, Unresolved int }

// CaptureRecoveryPage advances past individual failures. Start a new pass with
// an empty cursor after Done to revisit unresolved or retained artifacts.
type CaptureRecoveryPage struct {
	CaptureRecoveryReport
	Next string
	Done bool
}

// CaptureRecovery reconciles terminal captures without re-executing work or
// manufacturing successful results. Live jobs and unexpired reported evidence
// are retained. Abandoned or expired captures require durable deletion proof.
type CaptureRecovery struct {
	store      *workerstore.Store
	repository evidence.Repository
	audit      evidence.TombstoneReader
	authorize  func(context.Context, sandbox.Job, evidence.Ref) error
	actor      string
	pageSize   int
	now        func() time.Time
}

// NewCaptureRecovery requires explicit cleanup authority and bounded pages.
func NewCaptureRecovery(cfg CaptureRecoveryConfig) (*CaptureRecovery, error) {
	if cfg.Store == nil || cfg.Repository == nil || cfg.Audit == nil || cfg.Authorize == nil || cfg.PageSize < 1 || cfg.PageSize > 256 || !validCleanupText(cfg.Actor) {
		return nil, fmt.Errorf("cortex: invalid capture recovery configuration")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &CaptureRecovery{store: cfg.Store, repository: cfg.Repository, audit: cfg.Audit, authorize: cfg.Authorize, actor: cfg.Actor, pageSize: cfg.PageSize, now: cfg.Now}, nil
}

// Recover visits the full index, including terminal jobs, with bounded memory.
// It retains the first failure and continues to later pages while time remains.
func (s *CaptureRecovery) Recover(ctx context.Context) (CaptureRecoveryReport, error) {
	var result CaptureRecoveryReport
	var first error
	cursor := ""
	for {
		page, err := s.Sweep(ctx, cursor)
		result.Examined += page.Examined
		result.Deferred += page.Deferred
		result.Retained += page.Retained
		result.Discarded += page.Discarded
		result.Unresolved += page.Unresolved
		if first == nil {
			first = err
		}
		if page.Done || ctx.Err() != nil || page.Next == cursor {
			return result, errors.Join(first, ctx.Err())
		}
		cursor = page.Next
	}
}

// Sweep rechecks terminal state before contacting the repository. Execution
// expiry alone never proves whole-service cleanup or authorizes deletion.
func (s *CaptureRecovery) Sweep(ctx context.Context, after string) (CaptureRecoveryPage, error) {
	page := CaptureRecoveryPage{Next: after}
	entries, err := s.store.CaptureJobs(ctx, after, s.pageSize)
	if err != nil {
		return page, err
	}
	var first error
	for _, key := range entries {
		if operationErr := ctx.Err(); operationErr != nil {
			return page, errors.Join(first, operationErr)
		}
		if checkErr := s.check(ctx, key, &page.CaptureRecoveryReport); checkErr != nil {
			page.Unresolved++
			if first == nil {
				first = fmt.Errorf("cortex: recover sandbox capture %s: %w", key, checkErr)
			}
		}
		page.Examined++
		page.Next = key.String()
	}
	page.Done = ctx.Err() == nil && len(entries) < s.pageSize
	return page, errors.Join(first, ctx.Err())
}

func (s *CaptureRecovery) check(ctx context.Context, key id.ID, report *CaptureRecoveryReport) error {
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	job, err := s.store.Load(operation, key)
	if err != nil {
		return err
	}
	if job.Phase != workerstore.Terminal {
		report.Deferred++
		return nil
	}
	records, err := s.store.Captures(operation, key)
	if err != nil {
		return err
	}
	var first error
	for _, record := range records {
		if record.State == workerstore.CaptureDiscarded {
			continue
		}
		reason, reasonErr := captureDiscardReason(job, record, s.now())
		if reasonErr != nil {
			if first == nil {
				first = reasonErr
			}
			continue
		}
		if reason == "" {
			report.Retained++
			continue
		}
		if discardErr := s.discard(operation, job.Receipt.Job, record, reason); discardErr != nil {
			if first == nil {
				first = discardErr
			}
			continue
		}
		report.Discarded++
	}
	return errors.Join(first, operation.Err())
}

func captureDiscardReason(job workerstore.Record, record workerstore.CaptureRecord, now time.Time) (string, error) {
	if job.Result == nil {
		return "", workerstore.ErrCorrupt
	}
	for _, ref := range job.Result.Artifacts {
		if ref.ID != record.Ref.ID {
			continue
		}
		expected, err := record.Ref.BindingDigest()
		if err != nil {
			return "", err
		}
		actual, err := ref.BindingDigest()
		if err != nil {
			return "", err
		}
		if actual != expected || record.State != workerstore.CaptureVerified {
			return "", workerstore.ErrCorrupt
		}
		if now.Before(record.Ref.RetainUntil) {
			return "", nil
		}
		return "sandbox capture retention expired", nil
	}
	return "sandbox capture abandoned", nil
}

func (s *CaptureRecovery) discard(ctx context.Context, job sandbox.Job, record workerstore.CaptureRecord, reason string) error {
	if err := s.allowed(ctx, job, record.Ref); err != nil {
		return err
	}
	proof, err := s.audit.GetEvidenceTombstone(s.scoped(ctx, job), detachedCaptureRef(record.Ref))
	if errors.Is(err, evidence.ErrNotFound) {
		if policyErr := s.allowed(ctx, job, record.Ref); policyErr != nil {
			return policyErr
		}
		if deleteErr := s.repository.Delete(s.scoped(ctx, job), detachedCaptureRef(record.Ref), reason); deleteErr != nil {
			return deleteErr
		}
		if policyErr := s.allowed(ctx, job, record.Ref); policyErr != nil {
			return policyErr
		}
		proof, err = s.audit.GetEvidenceTombstone(s.scoped(ctx, job), detachedCaptureRef(record.Ref))
	}
	if err != nil {
		return err
	}
	expected, err := record.Ref.BindingDigest()
	if err != nil {
		return err
	}
	actual, err := proof.Ref.BindingDigest()
	if err != nil {
		return err
	}
	if actual != expected || !validCleanupText(proof.Actor) || !validCleanupText(proof.Reason) || proof.DeletedAt.Before(record.Ref.CreatedAt) || proof.DeletedAt.After(s.now()) {
		return evidence.ErrInvalid
	}
	if policyErr := s.allowed(ctx, job, record.Ref); policyErr != nil {
		return policyErr
	}
	_, err = s.store.DiscardCapture(ctx, job.ID, record.Ref.ID, record.Revision, reason)
	return err
}

func (s *CaptureRecovery) allowed(ctx context.Context, job sandbox.Job, ref evidence.Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	candidate, err := sandbox.Seal(job)
	if err != nil {
		return err
	}
	if policyErr := s.authorize(s.scoped(ctx, job), candidate, detachedCaptureRef(ref)); policyErr != nil {
		return policyErr
	}
	return ctx.Err()
}

func (s *CaptureRecovery) scoped(ctx context.Context, job sandbox.Job) context.Context {
	return evidence.WithDeletionActor(cortex.WithScope(ctx, cortex.Scope{Levels: slices.Clone(job.Scope.Levels)}), s.actor)
}

func detachedCaptureRef(ref evidence.Ref) evidence.Ref {
	ref.Scope.Levels = slices.Clone(ref.Scope.Levels)
	return ref
}
func validCleanupText(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}
