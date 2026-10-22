//go:build linux

package worker

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/integrations/sandbox/internal/staging"
	"github.com/xraph/cortex/integrations/sandbox/internal/supervisor"
	"github.com/xraph/cortex/integrations/sandbox/internal/workspace"
	"github.com/xraph/cortex/sandbox"
)

type captureRepository struct{ artifact evidence.Artifact }

type captureJournal struct{}

func (captureJournal) Record(ctx context.Context, _ sandbox.Job, _ evidence.Ref) error {
	return ctx.Err()
}
func (captureJournal) Confirm(ctx context.Context, _ sandbox.Job, _ evidence.Ref) error {
	return ctx.Err()
}

func (r *captureRepository) Put(_ context.Context, a evidence.Artifact) (evidence.Ref, error) {
	r.artifact = a
	return a.Ref, nil
}
func (r *captureRepository) Open(context.Context, evidence.Ref) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(string(r.artifact.Data))), nil
}

func TestPipelineCapturesOnlyAfterRuntimeCleanupAndBeforeWorkspaceCleanup(t *testing.T) {
	for _, failure := range []string{"", "runtime-cleanup", "capture", "capture-cleanup-sentinel", "capture-disabled", "capture-provider", "capture-policy", "capture-journal"} {
		t.Run(failure, func(t *testing.T) {
			s, sources, deps, calls := pipelineFixture(t, "")
			s.job.Outputs = []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/report", MediaType: "text/plain", MaxBytes: 64}}
			s.job.Limits.ArtifactBytes = 64
			var err error
			s.job, err = sandbox.Seal(s.job)
			if err != nil {
				t.Fatal(err)
			}
			s.cfg.Settings.CaptureRetention = time.Hour
			sources.Artifacts = &captureRepository{}
			s.journal = captureJournal{}
			sources.AuthorizeOutput = func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil }
			paths := workspace.Paths{JobID: s.job.ID, JobDigest: s.job.Digest, Scratch: "/fixture/scratch"}
			deps.workspace = func(context.Context, workspace.Config) (work, error) {
				return &fixtureWorkspace{paths: paths, calls: calls}, nil
			}
			deps.bundle = func(context.Context, workspace.Paths, sandbox.Job, string, map[string]string) error { return nil }
			deps.image = func(context.Context, image.Config, string) (*image.Filesystem, error) {
				return &image.Filesystem{Digest: s.job.Image}, nil
			}
			deps.inputs = func(context.Context, staging.Config, sandbox.Job) (*staging.Inputs, error) {
				return &staging.Inputs{JobDigest: s.job.Digest}, nil
			}
			deps.driver = func(context.Context, supervisor.RunscConfig, work) (supervisor.Driver, error) {
				return &fixtureDriver{calls: calls, fail: failure}, nil
			}
			deps.capture = func(ctx context.Context, cfg capture.Config, j sandbox.Job) ([]evidence.Ref, error) {
				*calls = append(*calls, "capture")
				if cfg.Root != paths.Scratch || cfg.Retention != time.Hour || j.Digest != s.job.Digest {
					t.Fatal("capture lost signed policy")
				}
				if err = cfg.Check(ctx); err != nil {
					t.Fatal(err)
				}
				if err = cfg.Authorize(ctx, j); err != nil {
					t.Fatal(err)
				}
				if failure == "capture" {
					return nil, errors.New("secret repository error")
				}
				if failure == "capture-cleanup-sentinel" {
					return nil, supervisor.ErrCleanup
				}
				now := time.Now().UTC()
				a, sealErr := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: j.Outputs[0].ID, Scope: j.Scope, MediaType: "text/plain", Version: j.Digest, Producer: sandbox.ArtifactProducer, SourceURI: j.OutputSource(j.Outputs[0]), Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("result")})
				return []evidence.Ref{a.Ref}, sealErr
			}
			switch failure {
			case "capture-journal":
				s.journal = nil
			case "capture-disabled":
				s.cfg.Settings.CaptureRetention = 0
			case "capture-provider":
				sources.Artifacts = nil
			case "capture-policy":
				sources.AuthorizeOutput = nil
			}
			_, err = execute(t.Context(), s, sources, deps)
			if strings.HasPrefix(failure, "capture-") && failure != "capture-cleanup-sentinel" {
				if err == nil || len(*calls) != 0 {
					t.Fatalf("unavailable capture executed: %v %v", err, *calls)
				}
				return
			}
			if failure == "runtime-cleanup" {
				if err == nil || slices.Contains(*calls, "capture") || slices.Contains(*calls, "workspace-cleanup") || len(s.reports) != 0 {
					t.Fatal("captured before runtime stopped", err, *calls)
				}
				return
			}
			end := []string{"runtime-cleanup", "capture", "workspace-cleanup", "report"}
			if len(*calls) < 4 || !slices.Equal((*calls)[len(*calls)-4:], end) || len(s.reports) != 1 {
				t.Fatal("capture lifecycle changed", err, *calls)
			}
			if failure != "" {
				if err == nil || s.reports[0].State != sandbox.StateFailed || s.reports[0].Reason != "sandbox artifact capture failed" {
					t.Fatal("capture failure hidden", err, s.reports)
				}
			} else if err != nil || len(s.reports[0].Artifacts) != 1 {
				t.Fatal("capture missing", err, s.reports)
			}
		})
	}
}
