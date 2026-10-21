//go:build linux && sandbox_mount_kernel

package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/integrations/sandbox/internal/staging"
	"github.com/xraph/cortex/integrations/sandbox/internal/supervisor"
	"github.com/xraph/cortex/integrations/sandbox/internal/workspace"
	"github.com/xraph/cortex/sandbox"
)

type fixtureBlobs map[string][]byte

func (b fixtureBlobs) Open(_ context.Context, pin string) (io.ReadCloser, error) {
	body, ok := b[pin]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

type fixtureEvidence struct{ artifact evidence.Artifact }

func (f fixtureEvidence) Open(_ context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	if ref.SHA256 != f.artifact.Ref.SHA256 {
		return nil, evidence.ErrDenied
	}
	return io.NopCloser(bytes.NewReader(f.artifact.Data)), nil
}

func pipelineImage(t *testing.T) (string, fixtureBlobs) {
	t.Helper()
	var layer bytes.Buffer
	archive := tar.NewWriter(&layer)
	if err := archive.WriteHeader(&tar.Header{Name: "check", Mode: 0o444, Size: 7, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("fixture")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	pin := func(body []byte) string { return "sha256:" + evidence.Digest(body) }
	imageConfig := imagespec.Image{Platform: imagespec.Platform{OS: "linux", Architecture: runtime.GOARCH}, RootFS: imagespec.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.Digest(pin(layer.Bytes()))}}}
	config, err := json.Marshal(imageConfig)
	if err != nil {
		t.Fatal(err)
	}
	manifest := imagespec.Manifest{MediaType: imagespec.MediaTypeImageManifest, Config: imagespec.Descriptor{MediaType: imagespec.MediaTypeImageConfig, Digest: digest.Digest(pin(config)), Size: int64(len(config))}, Layers: []imagespec.Descriptor{{MediaType: imagespec.MediaTypeImageLayer, Digest: digest.Digest(pin(layer.Bytes())), Size: int64(layer.Len())}}}
	manifest.SchemaVersion = 2
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return pin(body), fixtureBlobs{pin(body): body, pin(config): config, pin(layer.Bytes()): layer.Bytes()}
}

// This fixture uses real image/input staging and bundle writes. Its session,
// workspace and executor are explicit seams; it does not qualify runsc isolation.
func TestWorkerKernelStagesVerifiedContent(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("worker staging fixture requires root")
	}
	s, sources, deps, calls := pipelineFixture(t, "")
	manifest, blobs := pipelineImage(t)
	now := time.Now().UTC()
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: s.job.Scope, MediaType: "text/plain", Version: "1", Producer: "fixture", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("verified input")})
	if err != nil {
		t.Fatal(err)
	}
	s.job.Image = manifest
	s.job.Inputs = []sandbox.Input{{Path: "/inputs/data", Ref: artifact.Ref}}
	s.job.Outputs = []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/report", MediaType: "text/plain", MaxBytes: 1024}}
	s.job.Limits.ArtifactBytes = 1024
	s.cfg.Settings.CaptureRetention = time.Hour
	s.job, err = sandbox.Seal(s.job)
	if err != nil {
		t.Fatal(err)
	}
	sources.Images = blobs
	sources.Inputs = fixtureEvidence{artifact: artifact}
	repository := &captureRepository{}
	sources.Artifacts = repository
	s.journal = captureJournal{}
	sources.AuthorizeOutput = func(_ context.Context, candidate sandbox.Job, _ evidence.Action, ref evidence.Ref) error {
		if candidate.Principal != s.job.Principal || ref.SourceURI != s.job.OutputSource(s.job.Outputs[0]) {
			t.Fatal("capture lost signed identity")
		}
		candidate.Scope.Levels[0].Value = "changed"
		ref.Scope.Levels[0].Value = "changed"
		return nil
	}
	sources.AuthorizeImage = func(ctx context.Context, candidate sandbox.Job, pin string) error {
		if candidate.Digest != s.job.Digest || candidate.Principal != s.job.Principal || pin != manifest || cortex.ScopeFromContext(ctx).Canonical() != s.job.Scope.Canonical() {
			t.Fatal("image lost job authority")
		}
		return ctx.Err()
	}
	sources.AuthorizeInput = func(ctx context.Context, candidate sandbox.Job, action evidence.Action, ref evidence.Ref) error {
		if candidate.Digest != s.job.Digest || candidate.Principal != s.job.Principal || action != evidence.ActionRead || ref.SHA256 != artifact.Ref.SHA256 {
			t.Fatal("input source policy lost signed subject")
		}
		candidate.Inputs[0].Ref.Scope.Levels[0].Value = "other"
		ref.Scope.Levels[0].Value = "other"
		return ctx.Err()
	}
	var paths workspace.Paths
	deps.workspace = func(ctx context.Context, cfg workspace.Config) (work, error) {
		if authErr := cfg.Authorize(ctx, cfg.Job); authErr != nil {
			return nil, authErr
		}
		root := filepath.Join(cfg.Root, cfg.Job.ID.String())
		paths = workspace.Paths{JobID: cfg.Job.ID, JobDigest: cfg.Job.Digest, Directory: root, Images: filepath.Join(root, "stage", "images"), Inputs: filepath.Join(root, "stage", "inputs"), Bundle: filepath.Join(root, "stage", "bundle"), Runtime: filepath.Join(root, "stage", "runtime"), Scratch: filepath.Join(root, "scratch")}
		for _, directory := range []string{paths.Images, paths.Inputs, paths.Bundle, paths.Runtime, paths.Scratch} {
			if mkdirErr := os.MkdirAll(directory, 0o700); mkdirErr != nil {
				return nil, mkdirErr
			}
		}
		return &fixtureWorkspace{paths: paths, calls: calls}, nil
	}
	deps.image = image.Stage
	deps.inputs = staging.Stage
	deps.bundle = writeBundle
	deps.capture = capture.Collect
	deps.driver = func(_ context.Context, cfg supervisor.RunscConfig, _ work) (supervisor.Driver, error) {
		contents, readErr := os.ReadFile(filepath.Join(cfg.Rootfs, "check"))
		if readErr != nil || string(contents) != "fixture" {
			t.Fatal("unverified image reached runtime", readErr)
		}
		contents, readErr = os.ReadFile(cfg.Inputs["/inputs/data"])
		if readErr != nil || string(contents) != "verified input" {
			t.Fatal("unverified evidence reached runtime", readErr)
		}
		body, readErr := os.ReadFile(filepath.Join(cfg.Bundle, "config.json"))
		if readErr != nil || !bytes.Contains(body, []byte(paths.Scratch)) {
			t.Fatal("bundle lost scratch binding", readErr)
		}
		if writeErr := writeBundle(t.Context(), paths, s.job, cfg.Rootfs, cfg.Inputs); writeErr == nil {
			t.Fatal("existing OCI bundle was overwritten")
		}
		if mkdirErr := os.Mkdir(filepath.Join(paths.Scratch, "outputs"), 0o700); mkdirErr != nil {
			t.Fatal(mkdirErr)
		}
		if writeErr := os.WriteFile(filepath.Join(paths.Scratch, "outputs", "report"), []byte("untrusted captured result"), 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		return &fixtureDriver{calls: calls}, nil
	}
	receipt, err := execute(t.Context(), s, sources, deps)
	if err != nil || receipt.JobID != s.job.ID || len(s.reports) != 1 {
		t.Fatal("staging pipeline failed", receipt, err, *calls)
	}
	if string(repository.artifact.Data) != "untrusted captured result" || s.job.ValidateArtifacts(s.reports[0].Artifacts, true) != nil {
		t.Fatal("capture lost staged output")
	}
}
