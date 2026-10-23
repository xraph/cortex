//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

func evidenceBackend(t *testing.T) *sqlitestore.Store {
	t.Helper()
	drv := sqlitedriver.New()
	if err := drv.Open(t.Context(), filepath.Join(t.TempDir(), "evidence.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatal(err)
	}
	s := sqlitestore.New(db)
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return s
}

func evidenceAdapter(t *testing.T, s *sqlitestore.Store) *EvidenceRepository {
	t.Helper()
	policy := func(ctx context.Context, job sandbox.Job, _ evidence.Action, ref evidence.Ref) error {
		if cortex.PrincipalFromContext(ctx) != job.Principal {
			t.Fatal("lost worker principal")
		}
		job.Scope.Levels[0].Value = "policy mutation"
		ref.Scope.Levels[0].Value = "policy mutation"
		return ctx.Err()
	}
	r, err := NewEvidenceRepository(EvidenceRepositoryConfig{Storage: s, Audit: s, Inputs: policy, Captures: policy, Cleanup: func(ctx context.Context, _ sandbox.Job, _ evidence.Ref) error { return ctx.Err() }, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestEvidenceRepositoryWorkerTransferUsesStoredArtifacts(t *testing.T) {
	access, c, claim, peer, input, output, _ := workerEvidenceFixture(t)
	s := evidenceBackend(t)
	r := evidenceAdapter(t, s)
	if err := s.SaveEvidence(cortex.WithScope(t.Context(), input.Ref.Scope), input); err != nil {
		t.Fatal(err)
	}
	access.inputs = r
	c.scanner.captures.repository = r
	c.scanner.captures.audit = r
	var copied bytes.Buffer
	if err := access.read(t.Context(), peer, claim, input.Ref, false, &copied); err != nil || !bytes.Equal(input.Data, copied.Bytes()) {
		t.Fatal("input transfer", err)
	}
	if _, err := access.put(t.Context(), peer, claim, output); err == nil {
		t.Fatal("accepted missing intent")
	}
	if _, err := c.captureWorker(t.Context(), peer, claim, output.Ref, false); err != nil {
		t.Fatal(err)
	}
	if _, err := access.put(t.Context(), peer, claim, output); err != nil {
		t.Fatal(err)
	}
	copied.Reset()
	if err := access.read(t.Context(), peer, claim, output.Ref, true, &copied); err != nil || !bytes.Equal(output.Data, copied.Bytes()) {
		t.Fatal("output readback", err)
	}
	if _, err := c.captureWorker(t.Context(), peer, claim, output.Ref, true); err != nil {
		t.Fatal(err)
	}
	if _, err := access.put(t.Context(), peer, claim, output); err == nil {
		t.Fatal("accepted confirmed capture upload")
	}
}

func TestEvidenceRepositoryRejectsAmbientAndAlteredAuthority(t *testing.T) {
	_, c, claim, _, input, output, _ := workerEvidenceFixture(t)
	record, err := c.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	job := record.Receipt.Job
	r := evidenceAdapter(t, evidenceBackend(t))
	worker := evidenceContext(t.Context(), job)
	cleanup := cleanupEvidenceContext(t.Context(), job, "service:cleanup")
	ambient := evidence.WithDeletionActor(cortex.WithPrincipal(cortex.WithScope(t.Context(), job.Scope), job.Principal), "service:cleanup")
	for name, ctx := range map[string]context.Context{
		"ambient":       ambient,
		"principal":     cortex.WithPrincipal(worker, "other"),
		"scope":         cortex.WithScope(worker, cortex.Scope{}),
		"worker-actor":  evidence.WithDeletionActor(worker, "service:cleanup"),
		"cleanup-actor": evidence.WithDeletionActor(cleanup, "other"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := r.Put(ctx, output); err == nil {
				t.Fatal("put allowed")
			}
			if reader, err := r.Open(ctx, input.Ref); err == nil || reader != nil {
				t.Fatal("read allowed", err)
			}
			if err := r.Delete(ctx, output.Ref, "abandoned"); err == nil {
				t.Fatal("delete allowed")
			}
			if proof, err := r.GetEvidenceTombstone(ctx, output.Ref); err == nil || !proof.Ref.ID.IsNil() {
				t.Fatal("audit allowed", err)
			}
		})
	}
	if err := r.Delete(worker, output.Ref, "abandoned"); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("worker delete", err)
	}
	if _, err := r.GetEvidenceTombstone(worker, output.Ref); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("worker audit", err)
	}
	if _, err := r.Put(cleanup, output); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("cleanup put", err)
	}
	if _, err := r.Open(cleanup, output.Ref); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("cleanup read", err)
	}
	if err := r.Delete(cleanup, input.Ref, "abandoned"); err == nil {
		t.Fatal("cleanup deleted input")
	}
	if _, err := r.Put(worker, input); err == nil {
		t.Fatal("worker replaced input")
	}
	job.Principal = "tampered"
	if _, err := r.Put(evidenceContext(worker, job), output); err == nil {
		t.Fatal("invalid job inherited authority")
	}
}

func TestEvidenceRepositoryRecoveryFencesDelayedUpload(t *testing.T) {
	for _, stored := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "stored"}[stored], func(t *testing.T) {
			scanner, workers, job, ref, _ := captureRecoveryFixture(t, "abandoned")
			s := evidenceBackend(t)
			r := evidenceAdapter(t, s)
			ctx := cortex.WithScope(t.Context(), job.Scope)
			artifact := evidence.Artifact{Ref: ref, Data: []byte("candidate")}
			if stored {
				if err := s.SaveEvidence(ctx, artifact); err != nil {
					t.Fatal(err)
				}
			}
			scanner.repository, scanner.audit = r, r
			report, err := scanner.Recover(t.Context())
			if err != nil || report.Discarded != 1 {
				t.Fatal(report, err)
			}
			captures, err := workers.Captures(t.Context(), job.ID)
			if err != nil || captures[0].State != workerstore.CaptureDiscarded {
				t.Fatal(captures, err)
			}
			if saveErr := s.SaveEvidence(ctx, artifact); !errors.Is(saveErr, evidence.ErrDeleted) {
				t.Fatal("late upload", saveErr)
			}
			proof, err := r.GetEvidenceTombstone(cleanupEvidenceContext(t.Context(), job, scanner.actor), ref)
			if err != nil || proof.Actor != scanner.actor || proof.Reason == "" {
				t.Fatal(proof, err)
			}
		})
	}
}

func TestEvidenceRepositoryDetachesJobAndDisablesCapture(t *testing.T) {
	_, c, claim, _, input, output, _ := workerEvidenceFixture(t)
	record, err := c.launcher.store.Load(t.Context(), claim.JobID)
	if err != nil {
		t.Fatal(err)
	}
	job := record.Receipt.Job
	s := evidenceBackend(t)
	r := evidenceAdapter(t, s)
	ctx := evidenceContext(t.Context(), job)
	job.Scope.Levels[0].Value = "caller mutation"
	if _, err = r.Put(ctx, output); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveEvidence(cortex.WithScope(t.Context(), input.Ref.Scope), input); err != nil {
		t.Fatal(err)
	}
	reader, err := r.Open(ctx, input.Ref)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(body, input.Data) {
		t.Fatal(err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	r.captures = nil
	if _, err = r.Put(ctx, output); !errors.Is(err, evidence.ErrDenied) {
		t.Fatal("disabled capture", err)
	}
}

type interceptedEvidenceStorage struct {
	evidence.Storage
	afterLoad func()
	loads     int
}

func (s *interceptedEvidenceStorage) LoadEvidence(ctx context.Context, ref evidence.Ref) (evidence.Artifact, error) {
	s.loads++
	a, err := s.Storage.LoadEvidence(ctx, ref)
	if s.afterLoad != nil {
		s.afterLoad()
	}
	return a, err
}

type interceptedEvidenceAudit struct {
	evidence.TombstoneReader
	afterRead func(*evidence.Tombstone)
	reads     int
}

func (a *interceptedEvidenceAudit) GetEvidenceTombstone(ctx context.Context, ref evidence.Ref) (evidence.Tombstone, error) {
	a.reads++
	proof, err := a.TombstoneReader.GetEvidenceTombstone(ctx, ref)
	if a.afterRead != nil {
		a.afterRead(&proof)
	}
	return proof, err
}

func TestEvidenceRepositoryRechecksReadAuthority(t *testing.T) {
	for _, mode := range []string{"revoked", "cancelled", "undeclared", "changed-binding", "ambient"} {
		t.Run(mode, func(t *testing.T) {
			_, c, claim, _, input, _, _ := workerEvidenceFixture(t)
			record, err := c.launcher.store.Load(t.Context(), claim.JobID)
			if err != nil {
				t.Fatal(err)
			}
			s := evidenceBackend(t)
			if err = s.SaveEvidence(cortex.WithScope(t.Context(), input.Ref.Scope), input); err != nil {
				t.Fatal(err)
			}
			r := evidenceAdapter(t, s)
			storage := &interceptedEvidenceStorage{Storage: s}
			r.repository, err = evidence.NewRepository(storage, r, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(evidenceContext(t.Context(), record.Receipt.Job))
			defer cancel()
			ref := detachedCaptureRef(input.Ref)
			expectedLoads := 1
			switch mode {
			case "revoked":
				storage.afterLoad = func() {
					r.inputs = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return evidence.ErrDenied }
				}
			case "cancelled":
				storage.afterLoad = cancel
			case "undeclared":
				ref.ID = id.New(evidence.PrefixArtifact)
				expectedLoads = 0
			case "changed-binding":
				ref.Version = "changed"
				expectedLoads = 0
			case "ambient":
				ctx = cortex.WithPrincipal(cortex.WithScope(t.Context(), input.Ref.Scope), record.Receipt.Job.Principal)
				expectedLoads = 0
			}
			reader, err := r.Open(ctx, ref)
			if err == nil || reader != nil || storage.loads != expectedLoads {
				t.Fatal("read leaked", err, storage.loads)
			}
		})
	}
}

func TestEvidenceRepositoryRechecksTombstoneAuthorityAndBinding(t *testing.T) {
	for _, mode := range []string{"revoked", "cancelled", "binding", "worker", "ambient"} {
		t.Run(mode, func(t *testing.T) {
			_, _, job, ref, _ := captureRecoveryFixture(t, "abandoned")
			s := evidenceBackend(t)
			r := evidenceAdapter(t, s)
			ctx, cancel := context.WithCancel(cleanupEvidenceContext(t.Context(), job, "service:cleanup"))
			defer cancel()
			if err := r.Delete(ctx, ref, "abandoned"); err != nil {
				t.Fatal(err)
			}
			audit := &interceptedEvidenceAudit{TombstoneReader: s}
			r.audit = audit
			expectedReads := 1
			switch mode {
			case "revoked":
				audit.afterRead = func(*evidence.Tombstone) {
					r.cleanup = func(context.Context, sandbox.Job, evidence.Ref) error { return evidence.ErrDenied }
				}
			case "cancelled":
				audit.afterRead = func(*evidence.Tombstone) { cancel() }
			case "binding":
				audit.afterRead = func(p *evidence.Tombstone) { p.Ref.Version = "changed" }
			case "worker":
				ctx = evidenceContext(ctx, job)
				expectedReads = 0
			case "ambient":
				ctx = evidence.WithDeletionActor(cortex.WithScope(t.Context(), job.Scope), "service:cleanup")
				expectedReads = 0
			}
			proof, err := r.GetEvidenceTombstone(ctx, ref)
			if err == nil || !proof.Ref.ID.IsNil() || proof.Actor != "" || audit.reads != expectedReads {
				t.Fatal("audit leaked", proof, err, audit.reads)
			}
		})
	}
}

func TestEvidenceRepositoryRequiresExplicitServicesAndBounds(t *testing.T) {
	s := evidenceBackend(t)
	policy := func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil }
	cleanup := func(context.Context, sandbox.Job, evidence.Ref) error { return nil }
	base := EvidenceRepositoryConfig{Storage: s, Audit: s, Inputs: policy, Cleanup: cleanup, MaxBytes: 1 << 20}
	for _, mode := range []string{"storage", "audit", "input", "cleanup", "zero", "negative", "large"} {
		t.Run(mode, func(t *testing.T) {
			cfg := base
			switch mode {
			case "storage":
				cfg.Storage = nil
			case "audit":
				cfg.Audit = nil
			case "input":
				cfg.Inputs = nil
			case "cleanup":
				cfg.Cleanup = nil
			case "zero":
				cfg.MaxBytes = 0
			case "negative":
				cfg.MaxBytes = -1
			case "large":
				cfg.MaxBytes = 1<<30 + 1
			}
			if r, err := NewEvidenceRepository(cfg); err == nil || r != nil {
				t.Fatal("invalid configuration accepted", err)
			}
		})
	}
	if _, err := NewEvidenceRepository(base); err != nil {
		t.Fatal("disabled captures should be allowed", err)
	}
}
