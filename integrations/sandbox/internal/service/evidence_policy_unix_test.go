//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/sandbox"
)

func evidencePolicyFixture(t *testing.T, job sandbox.Job) (*EvidencePolicy, EvidencePolicyDocument) {
	t.Helper()
	now := time.Now().UTC()
	grant := EvidenceSubjectGrant{Principal: job.Principal, Scope: job.Scope, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), Captures: &CaptureGrant{MaxBytes: 1024, MaxRetention: time.Hour}}
	for _, input := range job.Inputs {
		binding, err := input.Ref.BindingDigest()
		if err != nil {
			t.Fatal(err)
		}
		grant.InputBindings = append(grant.InputBindings, binding)
	}
	doc := EvidencePolicyDocument{SchemaVersion: 1, NotBefore: grant.NotBefore, NotAfter: grant.NotAfter, Subjects: []EvidenceSubjectGrant{grant}, Cleanup: []EvidenceCleanupGrant{{Principal: job.Principal, Scope: job.Scope, Actor: "service:cleanup", NotBefore: grant.NotBefore, NotAfter: grant.NotAfter}}}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(directory, "evidence-policy.json")
	writeInstalledPolicy(t, name, doc)
	p, err := newEvidencePolicy(t.Context(), name, uint32(os.Geteuid()), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return p, doc
}

func TestEvidencePolicyWorkerAndRecoveryIntegration(t *testing.T) {
	access, c, claim, peer, input, output, _ := workerEvidenceFixture(t)
	record, err := c.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	job := record.Receipt.Job
	p, doc := evidencePolicyFixture(t, job)
	s := evidenceBackend(t)
	repository, err := NewEvidenceRepository(EvidenceRepositoryConfig{Storage: s, Audit: s, Inputs: p.AuthorizeInput, Captures: p.AuthorizeCapture, Cleanup: p.AuthorizeCleanup, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveEvidence(cortex.WithScope(t.Context(), job.Scope), input); err != nil {
		t.Fatal(err)
	}
	access.inputs, access.inputPolicy = repository, p.AuthorizeInput
	c.capturePolicy = p.AuthorizeCapture
	c.scanner.captures.repository, c.scanner.captures.audit = repository, repository
	c.scanner.captures.actor, c.scanner.captures.authorize = "service:cleanup", p.AuthorizeCleanup
	var body bytes.Buffer
	if err = access.read(t.Context(), peer, claim, input.Ref, false, &body); err != nil || !bytes.Equal(body.Bytes(), input.Data) {
		t.Fatal("read", err)
	}
	if _, err = c.captureWorker(t.Context(), peer, claim, output.Ref, false); err != nil {
		t.Fatal("intent", err)
	}
	if _, err = access.put(t.Context(), peer, claim, output); err != nil {
		t.Fatal("capture", err)
	}
	body.Reset()
	if err = access.read(t.Context(), peer, claim, output.Ref, true, &body); err != nil || !bytes.Equal(body.Bytes(), output.Data) {
		t.Fatal("readback", err)
	}
	if _, err = c.captureWorker(t.Context(), peer, claim, output.Ref, true); err != nil {
		t.Fatal("confirmation", err)
	}
	doc.Subjects = nil
	writeInstalledPolicy(t, p.path, doc)
	if err = access.read(t.Context(), peer, claim, input.Ref, false, &body); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("revoked read", err)
	}
	stopped, err := c.launcher.store.Cancel(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.launcher.store.Reaped(t.Context(), job.ID, stopped.Revision); err != nil {
		t.Fatal(err)
	}
	report, err := c.scanner.captures.Recover(t.Context())
	if err != nil || report.Discarded != 1 {
		t.Fatal("cleanup after subject revocation", report, err)
	}
	if err = s.SaveEvidence(cortex.WithScope(t.Context(), job.Scope), output); !errors.Is(err, evidence.ErrDeleted) {
		t.Fatal("late upload", err)
	}
}

func TestEvidencePolicySeparatesDeclaredInputsCapturesAndCleanup(t *testing.T) {
	_, c, claim, _, input, output, _ := workerEvidenceFixture(t)
	record, err := c.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	job := record.Receipt.Job
	p, _ := evidencePolicyFixture(t, job)
	worker := evidenceContext(t.Context(), job)
	cleanup := cleanupEvidenceContext(t.Context(), job, "service:cleanup")
	if err = p.AuthorizeInput(worker, job, evidence.ActionRead, input.Ref); err != nil {
		t.Fatal(err)
	}
	if err = p.AuthorizeCapture(worker, job, evidence.ActionPut, output.Ref); err != nil {
		t.Fatal(err)
	}
	if err = p.AuthorizeCleanup(cleanup, job, output.Ref); err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]func() error{
		"ambient": func() error {
			return p.AuthorizeInput(cortex.WithScope(t.Context(), job.Scope), job, evidence.ActionRead, input.Ref)
		},
		"input-write":      func() error { return p.AuthorizeInput(worker, job, evidence.ActionPut, input.Ref) },
		"input-as-capture": func() error { return p.AuthorizeCapture(worker, job, evidence.ActionRead, input.Ref) },
		"capture-as-input": func() error { return p.AuthorizeInput(worker, job, evidence.ActionRead, output.Ref) },
		"worker-delete":    func() error { return p.AuthorizeCapture(worker, job, evidence.ActionDelete, output.Ref) },
		"worker-cleanup":   func() error { return p.AuthorizeCleanup(worker, job, output.Ref) },
		"cleanup-input":    func() error { return p.AuthorizeCleanup(cleanup, job, input.Ref) },
		"cleanup-content":  func() error { return p.AuthorizeCapture(cleanup, job, evidence.ActionRead, output.Ref) },
		"wrong-actor": func() error {
			return p.AuthorizeCleanup(cleanupEvidenceContext(t.Context(), job, "other"), job, output.Ref)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if checkErr := check(); !errors.Is(checkErr, evidence.ErrDenied) {
				t.Fatal(checkErr)
			}
		})
	}
	changed := detachedCaptureRef(input.Ref)
	changed.Version = "different source version"
	if err = p.AuthorizeInput(worker, job, evidence.ActionRead, changed); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("changed binding", err)
	}
	job.Principal = "different subject"
	if err = p.AuthorizeCapture(worker, job, evidence.ActionPut, output.Ref); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("mutated job", err)
	}
}

func TestEvidencePolicyRevocationAndCurrentBounds(t *testing.T) {
	for _, mode := range []string{"input-revoked", "capture-disabled", "capture-bytes", "capture-retention", "cleanup-revoked", "document-expired", "subject-expired", "cleanup-expired", "subject-scope", "cleanup-scope", "future-evidence", "expired-evidence", "cancelled", "expiry-during-check"} {
		t.Run(mode, func(t *testing.T) {
			_, c, claim, _, input, output, _ := workerEvidenceFixture(t)
			record, err := c.launcher.store.Load(t.Context(), claim.JobID)
			if err != nil {
				t.Fatal(err)
			}
			job := record.Receipt.Job
			p, doc := evidencePolicyFixture(t, job)
			ctx := evidenceContext(t.Context(), job)
			check := func() error { return p.AuthorizeCapture(ctx, job, evidence.ActionRead, output.Ref) }
			now := p.now()
			switch mode {
			case "input-revoked":
				doc.Subjects[0].InputBindings = nil
				check = func() error { return p.AuthorizeInput(ctx, job, evidence.ActionRead, input.Ref) }
			case "capture-disabled":
				doc.Subjects[0].Captures = nil
			case "capture-bytes":
				doc.Subjects[0].Captures.MaxBytes = output.Ref.Bytes - 1
			case "capture-retention":
				doc.Subjects[0].Captures.MaxRetention = time.Minute
			case "cleanup-revoked":
				doc.Cleanup = nil
				check = func() error {
					return p.AuthorizeCleanup(cleanupEvidenceContext(ctx, job, "service:cleanup"), job, output.Ref)
				}
			case "document-expired":
				p.now = func() time.Time { return doc.NotAfter }
			case "subject-expired":
				doc.Subjects[0].NotAfter = now
			case "cleanup-expired":
				doc.Cleanup[0].NotAfter = now
				check = func() error {
					return p.AuthorizeCleanup(cleanupEvidenceContext(ctx, job, "service:cleanup"), job, output.Ref)
				}
			case "subject-scope":
				doc.Subjects[0].Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "another"}}}
			case "cleanup-scope":
				doc.Cleanup[0].Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "another"}}}
				check = func() error {
					return p.AuthorizeCleanup(cleanupEvidenceContext(ctx, job, "service:cleanup"), job, output.Ref)
				}
			case "future-evidence":
				output.Ref.CreatedAt = now.Add(time.Minute)
				output.Ref.RetainUntil = now.Add(2 * time.Minute)
			case "expired-evidence":
				output.Ref.CreatedAt = now.Add(-time.Hour)
				output.Ref.RetainUntil = now
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "expiry-during-check":
				calls := 0
				p.now = func() time.Time {
					calls++
					if calls > 1 {
						return doc.NotAfter
					}
					return now
				}
			}
			writeInstalledPolicy(t, p.path, doc)
			if err = check(); err == nil {
				t.Fatal("accepted revoked access")
			}
		})
	}
}

func TestEvidencePolicyRejectsInvalidDocumentsWithoutFallback(t *testing.T) {
	for _, mode := range []string{"schema", "document-window", "duplicate-subject", "duplicate-cleanup", "duplicate-binding", "bad-binding", "subject-window", "cleanup-window", "bad-principal", "bad-actor", "zero-capture", "large-capture", "long-retention", "negative-retention", "too-many-subjects", "too-many-cleanups", "too-many-bindings", "missing", "mode", "wrong-owner", "oversized", "symlink", "hardlink", "trailing", "unknown", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			_, c, claim, _, input, _, _ := workerEvidenceFixture(t)
			record, err := c.launcher.store.Load(t.Context(), claim.JobID)
			if err != nil {
				t.Fatal(err)
			}
			job := record.Receipt.Job
			p, doc := evidencePolicyFixture(t, job)
			ctx := evidenceContext(t.Context(), job)
			if err = p.AuthorizeInput(ctx, job, evidence.ActionRead, input.Ref); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "schema":
				doc.SchemaVersion++
			case "document-window":
				doc.NotBefore = doc.NotAfter
			case "duplicate-subject":
				doc.Subjects = append(doc.Subjects, doc.Subjects[0])
			case "duplicate-cleanup":
				doc.Cleanup = append(doc.Cleanup, doc.Cleanup[0])
			case "duplicate-binding":
				doc.Subjects[0].InputBindings = append(doc.Subjects[0].InputBindings, doc.Subjects[0].InputBindings[0])
			case "bad-binding":
				doc.Subjects[0].InputBindings[0] = "invalid"
			case "subject-window":
				doc.Subjects[0].NotAfter = doc.NotAfter.Add(time.Second)
			case "cleanup-window":
				doc.Cleanup[0].NotBefore = doc.NotBefore.Add(-time.Second)
			case "bad-principal":
				doc.Subjects[0].Principal = "\x00"
			case "bad-actor":
				doc.Cleanup[0].Actor = " "
			case "zero-capture":
				doc.Subjects[0].Captures.MaxBytes = 0
			case "large-capture":
				doc.Subjects[0].Captures.MaxBytes = 1<<20 + 1
			case "long-retention":
				doc.Subjects[0].Captures.MaxRetention = 25 * time.Hour
			case "negative-retention":
				doc.Subjects[0].Captures.MaxRetention = -time.Second
			case "too-many-subjects":
				doc.Subjects = make([]EvidenceSubjectGrant, 513)
			case "too-many-cleanups":
				doc.Cleanup = make([]EvidenceCleanupGrant, 513)
			case "too-many-bindings":
				doc.Subjects[0].InputBindings = make([]string, 257)
			}
			writeInstalledPolicy(t, p.path, doc)
			switch mode {
			case "missing":
				err = os.Remove(p.path)
			case "mode":
				err = os.Chmod(p.path, 0o644)
			case "wrong-owner":
				p.owner++
			case "oversized":
				err = os.WriteFile(p.path, bytes.Repeat([]byte(" "), (1<<20)+1), 0o600)
			case "symlink":
				if err = os.Rename(p.path, p.path+".target"); err == nil {
					err = os.Symlink(p.path+".target", p.path)
				}
			case "hardlink":
				err = os.Link(p.path, p.path+".link")
			case "trailing", "unknown", "malformed":
				var body []byte
				body, err = os.ReadFile(p.path)
				if err == nil {
					switch mode {
					case "trailing":
						body = append(body, '\n')
					case "unknown":
						body = append([]byte(`{"extra":true,`), body[1:]...)
					case "malformed":
						body = []byte("{")
					}
					err = os.WriteFile(p.path, body, 0o600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = p.AuthorizeInput(ctx, job, evidence.ActionRead, input.Ref); !errors.Is(err, evidence.ErrDenied) {
				t.Fatal("retained old grant", err)
			}
			if candidate, loadErr := newEvidencePolicy(ctx, p.path, p.owner, p.now); loadErr == nil || candidate != nil {
				t.Fatal("loaded invalid document", loadErr)
			}
		})
	}
}

func TestEvidencePolicyCleanupAllowsExpiredEvidenceButNeedsCurrentGrant(t *testing.T) {
	_, _, job, ref, _ := captureRecoveryFixture(t, "abandoned")
	p, doc := evidencePolicyFixture(t, job)
	now := p.now()
	ref.CreatedAt = now.Add(-2 * time.Hour)
	ref.RetainUntil = now.Add(-time.Hour)
	doc.Subjects = nil
	writeInstalledPolicy(t, p.path, doc)
	ctx := cleanupEvidenceContext(t.Context(), job, "service:cleanup")
	if err := p.AuthorizeCleanup(ctx, job, ref); err != nil {
		t.Fatal(err)
	}
	doc.Cleanup = nil
	writeInstalledPolicy(t, p.path, doc)
	if err := p.AuthorizeCleanup(ctx, job, ref); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("revoked cleanup", err)
	}
}

func TestEvidencePolicyProductionRequiresRootOwnership(t *testing.T) {
	_, _, job, _, _ := captureRecoveryFixture(t, "abandoned")
	p, _ := evidencePolicyFixture(t, job)
	installed, err := NewEvidencePolicy(t.Context(), p.path)
	if os.Geteuid() == 0 {
		if err != nil || installed == nil {
			t.Fatal(err)
		}
	} else if err == nil || installed != nil {
		t.Fatal("accepted user-owned production policy")
	}
	var missing *EvidencePolicy
	if _, _, err = missing.load(t.Context()); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("nil policy", err)
	}
}
