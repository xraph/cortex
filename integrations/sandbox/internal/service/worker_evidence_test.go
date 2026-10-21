package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/sandbox"
)

type workerEvidenceRepository struct {
	data                []byte
	opens, puts, closes int
	open                func(context.Context, evidence.Ref) (io.ReadCloser, error)
	put                 func(context.Context, evidence.Artifact) (evidence.Ref, error)
}

func (r *workerEvidenceRepository) Open(ctx context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	r.opens++
	if r.open != nil {
		return r.open(ctx, ref)
	}
	return &workerEvidenceReader{Reader: bytes.NewReader(r.data), closed: func() { r.closes++ }}, ctx.Err()
}
func (r *workerEvidenceRepository) Put(ctx context.Context, a evidence.Artifact) (evidence.Ref, error) {
	r.puts++
	if r.put != nil {
		return r.put(ctx, a)
	}
	r.data = bytes.Clone(a.Data)
	return a.Ref, ctx.Err()
}
func (*workerEvidenceRepository) GetEvidenceTombstone(context.Context, evidence.Ref) (evidence.Tombstone, error) {
	return evidence.Tombstone{}, evidence.ErrDenied
}
func (*workerEvidenceRepository) Delete(context.Context, evidence.Ref, string) error {
	return evidence.ErrDenied
}

type workerEvidenceReader struct {
	io.Reader
	closed func()
}

func (r *workerEvidenceReader) Close() error { r.closed(); return nil }

func workerEvidenceFixture(t *testing.T) (*WorkerEvidence, *Controller, WorkerClaim, *fixtureWorkerPeer, evidence.Artifact, evidence.Artifact, *workerEvidenceRepository) {
	t.Helper()
	var input evidence.Artifact
	c, receipt, _ := controllerFixture(t, func(job *sandbox.Job) {
		declareCapture(job)
		now := time.Now().UTC()
		var err error
		input, err = evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: job.Scope, MediaType: "text/plain", Version: "source:1", Producer: "host", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("approved input")})
		if err != nil {
			t.Fatal(err)
		}
		job.Inputs = []sandbox.Input{{Path: "/inputs/data", Ref: input.Ref}}
	})
	repo := &workerEvidenceRepository{data: input.Data}
	c.capturePolicy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil }
	recovery, recoveryErr := NewCaptureRecovery(CaptureRecoveryConfig{Store: c.launcher.store, Repository: repo, Audit: repo, Actor: "service:fixture", PageSize: 1, Authorize: func(context.Context, sandbox.Job, evidence.Ref) error { return evidence.ErrDenied }})
	if recoveryErr != nil {
		t.Fatal(recoveryErr)
	}
	c.scanner.captures = recovery
	c, claim, peer, _ := authorizationControllerFixture(t, c, receipt)
	output := evidence.Artifact{Ref: captureRefForJob(t, receipt.Job), Data: []byte("candidate")}
	access, err := NewWorkerEvidence(c, repo, func(ctx context.Context, job sandbox.Job, action evidence.Action, ref evidence.Ref) error {
		if action != evidence.ActionRead || job.Principal != receipt.Job.Principal || cortex.ScopeFromContext(ctx).Canonical() != job.Scope.Canonical() {
			t.Fatal("input policy lost identity")
		}
		job.Inputs[0].Ref.Scope.Levels[0].Value = "changed"
		ref.Scope.Levels[0].Value = "changed"
		return ctx.Err()
	}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return access, c, claim, peer, input, output, repo
}

func TestWorkerEvidenceTransfersOnlyDeclaredAndJournaledContent(t *testing.T) {
	access, c, claim, peer, input, output, repo := workerEvidenceFixture(t)
	var copied bytes.Buffer
	if err := access.read(t.Context(), peer, claim, input.Ref, false, &copied); err != nil || !bytes.Equal(copied.Bytes(), input.Data) || repo.closes != 1 {
		t.Fatal("input transfer", err)
	}
	if _, err := access.put(t.Context(), peer, claim, output); err == nil || repo.puts != 0 {
		t.Fatal("upload without intent")
	}
	if _, err := c.captureWorker(t.Context(), peer, claim, output.Ref, false); err != nil {
		t.Fatal(err)
	}
	if ref, err := access.put(t.Context(), peer, claim, output); err != nil || ref.ID != output.Ref.ID || repo.puts != 1 {
		t.Fatal("journaled upload", err)
	}
	copied.Reset()
	if err := access.read(t.Context(), peer, claim, output.Ref, true, &copied); err != nil || !bytes.Equal(copied.Bytes(), output.Data) {
		t.Fatal("readback", err)
	}
	if _, err := c.captureWorker(t.Context(), peer, claim, output.Ref, true); err != nil {
		t.Fatal(err)
	}
	if _, err := access.put(t.Context(), peer, claim, output); err == nil || repo.puts != 1 {
		t.Fatal("upload after confirmation")
	}
}

func TestWorkerEvidenceDeniesUnboundRequestsBeforeProviderAccess(t *testing.T) {
	for _, mode := range []string{"foreign-input", "changed-input", "oversized", "peer", "claim", "policy", "expired"} {
		t.Run(mode, func(t *testing.T) {
			access, _, claim, peer, input, _, repo := workerEvidenceFixture(t)
			switch mode {
			case "foreign-input":
				input.Ref.ID = id.New(evidence.PrefixArtifact)
			case "changed-input":
				input.Ref.Version = "changed"
			case "oversized":
				access.maxInput = 1
			case "peer":
				peer.dieAfterFirst.Store(true)
			case "claim":
				claim.Revision++
			case "policy":
				access.inputPolicy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return evidence.ErrDenied }
			case "expired":
				access.now = func() time.Time { return input.Ref.RetainUntil }
			}
			if err := access.read(t.Context(), peer, claim, input.Ref, false, io.Discard); err == nil || repo.opens != 0 {
				t.Fatal("unauthorized provider access", err)
			}
		})
	}
}

func TestWorkerEvidenceRejectsCorruptAndRevokedTransferResults(t *testing.T) {
	for _, mode := range []string{"digest", "oversized", "nil-reader", "read-error", "close-error", "revoke"} {
		t.Run(mode, func(t *testing.T) {
			access, _, claim, peer, input, _, repo := workerEvidenceFixture(t)
			switch mode {
			case "digest":
				repo.data = []byte("wrong content")
			case "oversized":
				repo.data = append(bytes.Clone(input.Data), 'x')
			case "nil-reader":
				repo.open = func(context.Context, evidence.Ref) (io.ReadCloser, error) { return nil, nil }
			case "read-error":
				repo.open = func(context.Context, evidence.Ref) (io.ReadCloser, error) { return nil, errors.New("source failed") }
			case "close-error":
				repo.open = func(context.Context, evidence.Ref) (io.ReadCloser, error) {
					return closingEvidenceReader{Reader: bytes.NewReader(input.Data)}, nil
				}
			case "revoke":
				repo.open = func(context.Context, evidence.Ref) (io.ReadCloser, error) {
					access.inputPolicy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return evidence.ErrDenied }
					return io.NopCloser(bytes.NewReader(input.Data)), nil
				}
			}
			var copied bytes.Buffer
			if err := access.read(t.Context(), peer, claim, input.Ref, false, &copied); err == nil {
				t.Fatal("invalid transfer succeeded")
			}
			if mode == "revoke" && copied.Len() != 0 {
				t.Fatal("copied bytes after source revoked access")
			}
			if repo.opens != 1 {
				t.Fatal("source retried")
			}
		})
	}
}

type closingEvidenceReader struct{ io.Reader }

func (closingEvidenceReader) Close() error { return errors.New("source close failed") }

func TestWorkerEvidenceRejectsUncertainUploadWithoutRetry(t *testing.T) {
	for _, mode := range []string{"lost", "binding", "revoke", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			access, c, claim, peer, _, output, repo := workerEvidenceFixture(t)
			if _, err := c.captureWorker(t.Context(), peer, claim, output.Ref, false); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			repo.put = func(_ context.Context, a evidence.Artifact) (evidence.Ref, error) {
				switch mode {
				case "lost":
					return evidence.Ref{}, errors.New("uncertain upload")
				case "binding":
					a.Ref.SourceURI = "changed"
				case "revoke":
					c.capturePolicy = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return evidence.ErrDenied }
				case "cancel":
					cancel()
				}
				return a.Ref, nil
			}
			if ref, err := access.put(ctx, peer, claim, output); err == nil || !ref.ID.IsNil() || repo.puts != 1 {
				t.Fatal("uncertain upload acknowledged", ref, err)
			}
		})
	}
}

func TestWorkerEvidencePublicBoundaryRejectsMissingPeer(t *testing.T) {
	access, _, claim, _, input, output, _ := workerEvidenceFixture(t)
	if err := access.ReadInput(t.Context(), nil, claim, input.Ref, io.Discard); !errors.Is(err, broker.ErrDenied) {
		t.Fatal(err)
	}
	if err := access.ReadArtifact(t.Context(), nil, claim, output.Ref, io.Discard); !errors.Is(err, broker.ErrDenied) {
		t.Fatal(err)
	}
	if _, err := access.PutArtifact(t.Context(), nil, claim, output); !errors.Is(err, broker.ErrDenied) {
		t.Fatal(err)
	}
}

func TestWorkerEvidenceBoundsAndBindsUploadsBeforeProviderAccess(t *testing.T) {
	for _, mode := range []string{"data-limit", "metadata-limit", "undeclared", "changed-content", "changed-binding", "missing-service"} {
		t.Run(mode, func(t *testing.T) {
			access, c, claim, peer, _, output, repo := workerEvidenceFixture(t)
			if _, err := c.captureWorker(t.Context(), peer, claim, output.Ref, false); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "data-limit":
				output.Data = make([]byte, (1<<20)+1)
			case "metadata-limit":
				output.Ref.SourceURI = strings.Repeat("x", captureFrameLimit+1)
			case "undeclared":
				output.Ref.ID = id.New(evidence.PrefixArtifact)
			case "changed-content":
				output.Data = []byte("new content")
				var err error
				output, err = evidence.Seal(output)
				if err != nil {
					t.Fatal(err)
				}
			case "changed-binding":
				output.Ref.RetainUntil = output.Ref.RetainUntil.Add(time.Minute)
			case "missing-service":
				c.capturePolicy = nil
			}
			if _, err := access.put(t.Context(), peer, claim, output); err == nil || repo.puts != 0 {
				t.Fatal("unbound upload reached repository", err)
			}
		})
	}
}

func TestWorkerEvidenceRechecksIntentAfterPolicy(t *testing.T) {
	access, c, claim, peer, _, output, repo := workerEvidenceFixture(t)
	if _, err := c.captureWorker(t.Context(), peer, claim, output.Ref, false); err != nil {
		t.Fatal(err)
	}
	c.capturePolicy = func(ctx context.Context, _ sandbox.Job, action evidence.Action, ref evidence.Ref) error {
		if action == evidence.ActionPut {
			_, err := c.launcher.store.ConfirmCapture(ctx, claim.JobID, claim.Token, claim.Revision, ref)
			return err
		}
		return nil
	}
	if _, err := access.put(t.Context(), peer, claim, output); err == nil || repo.puts != 0 {
		t.Fatal("upload began after confirmation", err)
	}
}

type blockingEvidenceReader struct {
	entered, closed chan struct{}
	once            sync.Once
	closes          atomic.Int32
}

func (r *blockingEvidenceReader) Read([]byte) (int, error) {
	close(r.entered)
	<-r.closed
	return 0, io.EOF
}
func (r *blockingEvidenceReader) Close() error {
	r.closes.Add(1)
	r.once.Do(func() { close(r.closed) })
	return nil
}

func TestWorkerEvidenceCancellationClosesBlockedSourceOnce(t *testing.T) {
	access, _, claim, peer, input, _, repo := workerEvidenceFixture(t)
	source := &blockingEvidenceReader{entered: make(chan struct{}), closed: make(chan struct{})}
	repo.open = func(context.Context, evidence.Ref) (io.ReadCloser, error) { return source, nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- access.read(ctx, peer, claim, input.Ref, false, io.Discard) }()
	select {
	case <-source.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("source did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled source did not close")
	}
	if source.closes.Load() != 1 {
		t.Fatal("source closed more than once", source.closes.Load())
	}
}

func TestWorkerEvidenceClosesReaderReturnedWithError(t *testing.T) {
	access, _, claim, peer, input, _, repo := workerEvidenceFixture(t)
	repo.open = func(context.Context, evidence.Ref) (io.ReadCloser, error) {
		return &workerEvidenceReader{Reader: bytes.NewReader(input.Data), closed: func() { repo.closes++ }}, errors.New("partial source failure")
	}
	if err := access.read(t.Context(), peer, claim, input.Ref, false, io.Discard); err == nil || repo.closes != 1 {
		t.Fatal("partial source leaked", err)
	}
}

func TestWorkerEvidencePoliciesAndProvidersCannotMutateCaptureBinding(t *testing.T) {
	access, c, claim, peer, _, output, repo := workerEvidenceFixture(t)
	if _, err := c.captureWorker(t.Context(), peer, claim, output.Ref, false); err != nil {
		t.Fatal(err)
	}
	c.capturePolicy = func(_ context.Context, job sandbox.Job, _ evidence.Action, ref evidence.Ref) error {
		job.Scope.Levels[0].Value = "changed"
		job.Outputs[0].Path = "changed"
		ref.Scope.Levels[0].Value = "changed"
		return nil
	}
	repo.put = func(ctx context.Context, a evidence.Artifact) (evidence.Ref, error) {
		if cortex.ScopeFromContext(ctx).Canonical() != output.Ref.Scope.Canonical() {
			t.Fatal("provider scope corrupted")
		}
		ref := detachedCaptureRef(a.Ref)
		a.Ref.Scope.Levels[0].Value = "changed"
		a.Data[0] = 'X'
		return ref, nil
	}
	got, err := access.put(t.Context(), peer, claim, output)
	if err != nil || got.Scope.Canonical() != output.Ref.Scope.Canonical() || string(output.Data) != "candidate" {
		t.Fatal("provider changed caller data", err)
	}
	records, err := c.launcher.store.Captures(t.Context(), claim.JobID)
	if err != nil || records[0].Ref.Scope.Canonical() != output.Ref.Scope.Canonical() {
		t.Fatal("provider changed retained binding", err)
	}
}
