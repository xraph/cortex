//go:build linux || darwin

package capture

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

type fixtureRepository struct {
	artifacts []evidence.Artifact
	fail      string
	afterPut  func()
}

func (r *fixtureRepository) Put(_ context.Context, a evidence.Artifact) (evidence.Ref, error) {
	r.artifacts = append(r.artifacts, a)
	if r.afterPut != nil {
		r.afterPut()
	}
	if r.fail == "put" {
		return evidence.Ref{}, errors.New("ack lost")
	}
	ref := a.Ref
	if r.fail == "ref" {
		ref.Version = "other"
	}
	return ref, nil
}

func TestCaptureKeepsVerifiedSubsetWhenLaterOutputIsMissing(t *testing.T) {
	ctx, cfg, j, repo := captureFixture(t)
	j.Limits.ArtifactBytes += 64
	j.Outputs = append(j.Outputs, sandbox.Output{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/missing", MediaType: "text/plain", MaxBytes: 64})
	var err error
	j, err = sandbox.Seal(j)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := Collect(ctx, cfg, j)
	if err == nil || len(refs) != 1 || len(repo.artifacts) != 1 {
		t.Fatal("partial capture changed", refs, err)
	}
	if err = j.ValidateArtifacts(refs, false); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureRechecksCancellationAfterUpload(t *testing.T) {
	ctx, cfg, j, repo := captureFixture(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	repo.afterPut = cancel
	refs, err := Collect(ctx, cfg, j)
	if !errors.Is(err, context.Canceled) || len(refs) != 0 || len(repo.artifacts) != 1 {
		t.Fatal("cancelled capture published", refs, err)
	}
}
func (r *fixtureRepository) Open(_ context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	if r.fail == "open" {
		return nil, errors.New("read denied")
	}
	if r.fail == "bytes" {
		return io.NopCloser(strings.NewReader("changed")), nil
	}
	for _, a := range r.artifacts {
		if a.Ref.ID == ref.ID {
			return io.NopCloser(bytes.NewReader(a.Data)), nil
		}
	}
	return nil, errors.New("missing")
}

func captureFixture(t *testing.T) (context.Context, Config, sandbox.Job, *fixtureRepository) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "outputs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outputs", "report"), []byte("candidate evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}
	j, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "capture", Scope: scope, Principal: "alice", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/check"}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 500, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 1 << 20, WallTime: time.Minute, OutputBytes: 1024, ArtifactBytes: 1024}, Outputs: []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/report", MediaType: "text/plain", MaxBytes: 1024}}})
	if err != nil {
		t.Fatal(err)
	}
	repo := &fixtureRepository{}
	cfg := Config{Root: root, Repository: repo, Retention: time.Hour, Authorize: func(ctx context.Context, _ sandbox.Job) error { return ctx.Err() }, Check: func(ctx context.Context) error { return ctx.Err() }, Policy: func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil }}
	return cortex.WithScope(context.Background(), scope), cfg, j, repo
}

func TestCapturePersistsBoundRestrictedEvidence(t *testing.T) {
	ctx, cfg, j, repo := captureFixture(t)
	refs, err := Collect(ctx, cfg, j)
	if err != nil || len(refs) != 1 || len(repo.artifacts) != 1 {
		t.Fatalf("capture: %v %v", refs, err)
	}
	if err = j.ValidateArtifacts(refs, true); err != nil {
		t.Fatal(err)
	}
	if string(repo.artifacts[0].Data) != "candidate evidence" {
		t.Fatal("wrong captured bytes")
	}
	if refs[0].RetainUntil.Sub(refs[0].CreatedAt) != time.Hour {
		t.Fatal("retention changed")
	}
}

func TestCaptureRejectsUnsafeFilesBeforeUpload(t *testing.T) {
	for _, kind := range []string{"missing", "symlink", "parent-link", "hardlink", "fifo", "directory", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cfg, j, repo := captureFixture(t)
			name := filepath.Join(cfg.Root, "outputs", "report")
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(outside, name)
			case "parent-link":
				if err = os.Remove(filepath.Dir(name)); err == nil {
					err = os.Symlink(filepath.Dir(outside), filepath.Dir(name))
				}
			case "hardlink":
				err = os.Link(outside, name)
			case "fifo":
				err = unix.Mkfifo(name, 0o600)
			case "directory":
				err = os.Mkdir(name, 0o700)
			case "oversized":
				err = os.WriteFile(name, make([]byte, 1025), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Collect(ctx, cfg, j); err == nil {
				t.Fatal("unsafe file captured")
			}
			if len(repo.artifacts) != 0 {
				t.Fatal("unsafe capture uploaded")
			}
		})
	}
}

func TestCaptureDoesNotRetryUncertainOrInvalidRepositoryResults(t *testing.T) {
	for _, fail := range []string{"put", "ref", "open", "bytes"} {
		t.Run(fail, func(t *testing.T) {
			ctx, cfg, j, repo := captureFixture(t)
			repo.fail = fail
			refs, err := Collect(ctx, cfg, j)
			if err == nil || len(refs) != 0 || len(repo.artifacts) != 1 {
				t.Fatalf("uncertain upload replayed or reported: %v %v %d", refs, err, len(repo.artifacts))
			}
		})
	}
}

func TestCaptureRechecksAuthorityAndDetachesPolicyData(t *testing.T) {
	ctx, cfg, j, repo := captureFixture(t)
	cfg.Policy = func(_ context.Context, candidate sandbox.Job, action evidence.Action, ref evidence.Ref) error {
		if candidate.Principal != "alice" || candidate.Scope.Canonical() != j.Scope.Canonical() {
			t.Fatal("signed identity missing")
		}
		candidate.Scope.Levels[0].Value = "changed"
		candidate.Outputs[0].Path = "changed"
		ref.Scope.Levels[0].Value = "changed"
		if action == evidence.ActionRead {
			return evidence.ErrDenied
		}
		return nil
	}
	refs, err := Collect(ctx, cfg, j)
	if !errors.Is(err, evidence.ErrDenied) || len(refs) != 0 || len(repo.artifacts) != 1 {
		t.Fatalf("revoked capture accepted: %v %v", refs, err)
	}
	if err = j.Validate(); err != nil {
		t.Fatal("policy changed retained job")
	}
}

func TestCaptureRequiresScopeAndCurrentWorkspace(t *testing.T) {
	for _, kind := range []string{"scope", "cancel", "workspace", "job", "policy", "retention"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cfg, j, repo := captureFixture(t)
			switch kind {
			case "scope":
				ctx = context.Background()
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "workspace":
				cfg.Check = func(context.Context) error { return errors.New("replaced mount") }
			case "job":
				cfg.Authorize = func(context.Context, sandbox.Job) error { return evidence.ErrDenied }
			case "policy":
				cfg.Policy = nil
			case "retention":
				cfg.Retention = 25 * time.Hour
			}
			if _, err := Collect(ctx, cfg, j); err == nil {
				t.Fatal("invalid capture admitted")
			}
			if len(repo.artifacts) != 0 {
				t.Fatal("unauthorized write")
			}
		})
	}
}
