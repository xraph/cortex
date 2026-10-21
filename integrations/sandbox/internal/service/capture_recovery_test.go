package service

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

type captureRecoveryRepository struct {
	tombstone  *evidence.Tombstone
	deletes    int
	failDelete bool
	failAudit  bool
	mutate     func(*evidence.Tombstone)
}

func (*captureRecoveryRepository) Put(context.Context, evidence.Artifact) (evidence.Ref, error) {
	return evidence.Ref{}, evidence.ErrDenied
}
func (*captureRecoveryRepository) Open(context.Context, evidence.Ref) (io.ReadCloser, error) {
	return nil, evidence.ErrDenied
}
func (r *captureRecoveryRepository) Delete(ctx context.Context, ref evidence.Ref, reason string) error {
	r.deletes++
	r.tombstone = &evidence.Tombstone{Ref: ref, Actor: evidence.DeletionActor(ctx), Reason: reason, DeletedAt: time.Now().UTC()}
	if r.mutate != nil {
		r.mutate(r.tombstone)
	}
	if r.failDelete {
		return errors.New("lost deletion acknowledgement")
	}
	return ctx.Err()
}
func (r *captureRecoveryRepository) GetEvidenceTombstone(ctx context.Context, ref evidence.Ref) (evidence.Tombstone, error) {
	if r.failAudit {
		return evidence.Tombstone{}, evidence.ErrDenied
	}
	if r.tombstone == nil || r.tombstone.Ref.ID != ref.ID {
		return evidence.Tombstone{}, evidence.ErrNotFound
	}
	return *r.tombstone, ctx.Err()
}

func captureRecoveryFixture(t *testing.T, phase string) (*CaptureRecovery, *workerstore.Store, sandbox.Job, evidence.Ref, *captureRecoveryRepository) {
	t.Helper()
	s, receipt, _ := recoveryStore(t, declareCapture)
	job := receipt.Job
	if _, err := s.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref := captureRefForJob(t, job)
	if _, err = s.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
		t.Fatal(err)
	}
	if phase == "reported" || phase == "finalizing" {
		if _, err = s.ConfirmCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
			t.Fatal(err)
		}
		proposed, proposalErr := s.Propose(t.Context(), job.ID, claim.Token, claim.Record.Revision, sandbox.Result{JobDigest: job.Digest, State: sandbox.StateExited, Artifacts: []evidence.Ref{ref}})
		if proposalErr != nil {
			t.Fatal(proposalErr)
		}
		if phase == "reported" {
			if _, err = s.Finalized(t.Context(), job.ID, proposed.Revision); err != nil {
				t.Fatal(err)
			}
		}
	} else if phase != "live" {
		stopped, stopErr := s.Cancel(t.Context(), job.ID)
		if stopErr != nil {
			t.Fatal(stopErr)
		}
		if _, err = s.Reaped(t.Context(), job.ID, stopped.Revision); err != nil {
			t.Fatal(err)
		}
	}
	repo := &captureRecoveryRepository{}
	scanner, err := NewCaptureRecovery(CaptureRecoveryConfig{Store: s, Repository: repo, Audit: repo, Actor: "service:capture-recovery", PageSize: 1, Authorize: func(ctx context.Context, candidate sandbox.Job, r evidence.Ref) error {
		if candidate.Principal != job.Principal || cortex.ScopeFromContext(ctx).Canonical() != job.Scope.Canonical() || evidence.DeletionActor(ctx) != "service:capture-recovery" {
			t.Fatal("recovery lost signed subject")
		}
		candidate.Scope.Levels[0].Value = "changed"
		r.Scope.Levels[0].Value = "changed"
		return ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	return scanner, s, job, ref, repo
}

func TestCaptureRecoveryWaitsForTerminalCleanupAndRetainsReportedEvidence(t *testing.T) {
	for _, phase := range []string{"live", "finalizing", "reported", "abandoned"} {
		t.Run(phase, func(t *testing.T) {
			scanner, s, job, ref, repo := captureRecoveryFixture(t, phase)
			page, err := scanner.Sweep(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			if page.Next != job.ID.String() || page.Done || page.Examined != 1 {
				t.Fatal("bad bounded page", page)
			}
			if phase == "abandoned" {
				if page.Discarded != 1 || repo.deletes != 1 {
					t.Fatal("abandoned capture retained", page)
				}
				records, readErr := s.Captures(t.Context(), job.ID)
				if readErr != nil || records[0].State != workerstore.CaptureDiscarded {
					t.Fatal(records, readErr)
				}
			} else if repo.deletes != 0 {
				t.Fatal("live or retained evidence deleted", phase)
			}
			if phase == "reported" {
				scanner.now = func() time.Time { return ref.RetainUntil }
				page, err = scanner.Sweep(t.Context(), "")
				if err != nil || page.Discarded != 1 || repo.deletes != 1 {
					t.Fatal("expired reported capture retained", page, err)
				}
			}
		})
	}
}

func TestCaptureRecoveryResolvesLostDeletionAckWithoutRepeatingDeletion(t *testing.T) {
	scanner, s, job, _, repo := captureRecoveryFixture(t, "abandoned")
	repo.failDelete = true
	page, err := scanner.Sweep(t.Context(), "")
	if err == nil || page.Unresolved != 1 || page.Discarded != 0 {
		t.Fatal("uncertain deletion marked clean", page, err)
	}
	records, err := s.Captures(t.Context(), job.ID)
	if err != nil || records[0].State != workerstore.CaptureIntent {
		t.Fatal(records, err)
	}
	page, err = scanner.Sweep(t.Context(), "")
	if err != nil || page.Discarded != 1 || repo.deletes != 1 {
		t.Fatal("deletion replayed instead of reading tombstone", page, err)
	}
}

func TestCaptureRecoveryRequiresCurrentAuthorityAndExactDurableProof(t *testing.T) {
	for _, mode := range []string{"policy", "audit", "missing-proof", "binding", "actor", "time", "revoked-after-delete", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			scanner, s, job, _, repo := captureRecoveryFixture(t, "abandoned")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "policy":
				scanner.authorize = func(context.Context, sandbox.Job, evidence.Ref) error { return evidence.ErrDenied }
			case "audit":
				repo.failAudit = true
			case "cancel":
				cancel()
			default:
				repo.mutate = func(tombstone *evidence.Tombstone) {
					switch mode {
					case "missing-proof":
						repo.tombstone = nil
					case "binding":
						tombstone.Ref.Version = "different"
					case "actor":
						tombstone.Actor = ""
					case "time":
						tombstone.DeletedAt = time.Now().Add(time.Hour)
					case "revoked-after-delete":
						scanner.authorize = func(context.Context, sandbox.Job, evidence.Ref) error { return evidence.ErrDenied }
					}
				}
			}
			page, err := scanner.Sweep(ctx, "")
			if err == nil || page.Discarded != 0 {
				t.Fatal("invalid cleanup proof accepted", page, err)
			}
			records, err := s.Captures(t.Context(), job.ID)
			if err != nil || records[0].State != workerstore.CaptureIntent {
				t.Fatal(records, err)
			}
			if (mode == "policy" || mode == "audit" || mode == "cancel") && repo.deletes != 0 {
				t.Fatal("unauthorized deletion")
			}
		})
	}
}

func TestControllerRunsCaptureRecoveryBeforeAdmissionAndOnSweep(t *testing.T) {
	for _, mode := range []string{"startup", "sweep", "unresolved"} {
		t.Run(mode, func(t *testing.T) {
			c, receipt, _ := controllerFixture(t, declareCapture)
			job := receipt.Job
			if _, err := c.launcher.store.Enroll(t.Context(), receipt); err != nil {
				t.Fatal(err)
			}
			claim, err := c.launcher.store.Claim(t.Context(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			ref := captureRefForJob(t, job)
			if _, err = c.launcher.store.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
				t.Fatal(err)
			}
			if mode == "sweep" {
				if _, err = c.launcher.store.ConfirmCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
					t.Fatal(err)
				}
				if _, err = c.launcher.store.Propose(t.Context(), job.ID, claim.Token, claim.Record.Revision, sandbox.Result{JobDigest: job.Digest, State: sandbox.StateExited, Artifacts: []evidence.Ref{ref}}); err != nil {
					t.Fatal(err)
				}
			}
			repo := &captureRecoveryRepository{failAudit: mode == "unresolved"}
			var expired atomic.Bool
			recovery, err := NewCaptureRecovery(CaptureRecoveryConfig{Store: c.launcher.store, Repository: repo, Audit: repo, Actor: "service:recovery", PageSize: 1, Authorize: func(context.Context, sandbox.Job, evidence.Ref) error { return nil }, Now: func() time.Time {
				if expired.Load() {
					return ref.RetainUntil
				}
				return time.Now()
			}})
			if err != nil {
				t.Fatal(err)
			}
			scanner, scannerErr := NewRecoveryScanner(RecoveryConfig{Store: c.launcher.store, Reaper: c.launcher.reaper, Revalidate: c.scanner.revalidate, PageSize: 2, Captures: recovery})
			if scannerErr != nil {
				t.Fatal(scannerErr)
			}
			c, err = NewController(c.launcher, scanner, 10*time.Millisecond, CapturePolicy(func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil }))
			if err != nil {
				t.Fatal(err)
			}
			cancel, done := runController(t, c)
			if mode == "unresolved" {
				if err = awaitControllerStop(t, done); err == nil || c.Status().Ready {
					t.Fatal("unresolved cleanup admitted work", err)
				}
				return
			}
			awaitControllerReady(t, c)
			if mode == "sweep" {
				expired.Store(true)
			}
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				records, readErr := c.launcher.store.Captures(t.Context(), job.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if records[0].State == workerstore.CaptureDiscarded {
					break
				}
				select {
				case <-deadline.C:
					t.Fatal("controller did not recover capture")
				case <-ticker.C:
				}
			}
			cancel()
			_ = awaitControllerStop(t, done)
			if repo.deletes != 1 {
				t.Fatal("capture deletion repeated", repo.deletes)
			}
		})
	}
}

func TestControllerCapturePolicyRequiresMatchingRecovery(t *testing.T) {
	c, _, _ := controllerFixture(t)
	policy := CapturePolicy(func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil })
	if _, err := NewController(c.launcher, c.scanner, time.Second, policy); err == nil {
		t.Fatal("capture enabled without recovery")
	}
}

func TestCaptureRecoveryMissingProviderKeepsAdmissionClosed(t *testing.T) {
	c, receipt, _ := controllerFixture(t, declareCapture)
	if _, err := c.launcher.store.Enroll(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	claim, err := c.launcher.store.Claim(t.Context(), receipt.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref := captureRefForJob(t, receipt.Job)
	if _, err = c.launcher.store.RecordCapture(t.Context(), receipt.Job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
		t.Fatal(err)
	}
	_, done := runController(t, c)
	if err = awaitControllerStop(t, done); err == nil || c.Status().Ready {
		t.Fatal("pending captures lost on startup", err)
	}
}

func TestCaptureRecoveryContinuesPastFailedArtifactsAndPages(t *testing.T) {
	base, receipts, _ := scannerFixture(t, 3, func(receipt *sandbox.Receipt) {
		declareCapture(&receipt.Job)
		receipt.Job.Outputs = append(receipt.Job.Outputs, sandbox.Output{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/second", MediaType: "text/plain", MaxBytes: 1024})
		receipt.Job.Limits.ArtifactBytes = 2048
		var err error
		receipt.Job, err = sandbox.Seal(receipt.Job)
		if err != nil {
			t.Fatal(err)
		}
		receipt.Capabilities.Limits = receipt.Job.Limits
	})
	for _, receipt := range receipts {
		job := receipt.Job
		claim, err := base.store.Claim(t.Context(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, output := range job.Outputs {
			candidate := job
			candidate.Outputs = []sandbox.Output{output}
			ref := captureRefForJob(t, candidate)
			if _, err = base.store.RecordCapture(t.Context(), job.ID, claim.Token, claim.Record.Revision, ref); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = base.reaper.Reap(t.Context(), base.store, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	repo := &captureRecoveryRepository{}
	scanner, err := NewCaptureRecovery(CaptureRecoveryConfig{Store: base.store, Repository: repo, Audit: repo, Actor: "service:cleanup", PageSize: 1, Authorize: func(_ context.Context, job sandbox.Job, ref evidence.Ref) error {
		if job.ID == receipts[0].Job.ID && ref.ID == receipts[0].Job.Outputs[0].ID {
			return evidence.ErrDenied
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := scanner.Recover(t.Context())
	if !errors.Is(err, evidence.ErrDenied) || report.Examined != 3 || report.Unresolved != 1 || report.Discarded != 5 || repo.deletes != 5 {
		t.Fatal("failed capture starved later work", report, err)
	}
}
