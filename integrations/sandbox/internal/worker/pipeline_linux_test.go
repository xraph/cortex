//go:build linux

package worker

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/integrations/sandbox/internal/service"
	"github.com/xraph/cortex/integrations/sandbox/internal/staging"
	"github.com/xraph/cortex/integrations/sandbox/internal/supervisor"
	"github.com/xraph/cortex/integrations/sandbox/internal/workspace"
	"github.com/xraph/cortex/sandbox"
)

type fixtureSession struct {
	journal  capture.Journal
	job      sandbox.Job
	cfg      service.WorkerConfiguration
	deadline time.Time
	calls    *[]string
	fail     string
	reports  []sandbox.Result
	pending  bool
}

func (s *fixtureSession) CaptureJournal() capture.Journal { return s.journal }

func (s *fixtureSession) Job() (sandbox.Job, error)                  { return sandbox.Seal(s.job) }
func (s *fixtureSession) Configuration() service.WorkerConfiguration { return s.cfg }
func (s *fixtureSession) Deadline() time.Time                        { return s.deadline }
func (s *fixtureSession) BeginExecution(context.Context) error {
	*s.calls = append(*s.calls, "begin")
	if s.fail == "begin" {
		return errors.New("entry refused")
	}
	return nil
}
func (s *fixtureSession) Authorize(ctx context.Context) error {
	if s.fail == "authorize" {
		return errors.New("grant revoked")
	}
	return ctx.Err()
}
func (s *fixtureSession) Report(ctx context.Context, r sandbox.Result) (service.WorkerReportReceipt, error) {
	*s.calls = append(*s.calls, "report")
	s.reports = append(s.reports, r)
	if s.pending {
		s.pending = false
		return service.WorkerReportReceipt{}, broker.ErrPending
	}
	if s.fail == "wrapped-pending" {
		return service.WorkerReportReceipt{}, errors.Join(broker.ErrPending, errors.New("uncertain response"))
	}
	if s.fail == "always-pending" {
		return service.WorkerReportReceipt{}, broker.ErrPending
	}
	if s.fail == "report" {
		return service.WorkerReportReceipt{}, errors.New("acknowledgement lost")
	}
	return service.WorkerReportReceipt{SchemaVersion: 1, JobID: s.job.ID, ClaimRevision: 2, ProposalDigest: strings.Repeat("a", 64)}, ctx.Err()
}

type fixtureWorkspace struct {
	paths workspace.Paths
	calls *[]string
	fail  string
}

func (w *fixtureWorkspace) Paths() (workspace.Paths, error) { return w.paths, nil }
func (w *fixtureWorkspace) Check(ctx context.Context) error { return ctx.Err() }
func (w *fixtureWorkspace) Close(ctx context.Context) error {
	*w.calls = append(*w.calls, "workspace-cleanup")
	if ctx.Err() != nil {
		return errors.New("cleanup inherited cancelled context")
	}
	if w.fail == "workspace-cleanup" {
		return errors.New("mount still busy")
	}
	return nil
}

type fixtureDriver struct {
	calls *[]string
	fail  string
}

func (d *fixtureDriver) Prepare(_ context.Context, _ sandbox.Job, out, _ io.Writer) error {
	*d.calls = append(*d.calls, "create")
	_, err := io.WriteString(out, "candidate output")
	return err
}
func (d *fixtureDriver) Start(context.Context) error {
	*d.calls = append(*d.calls, "start")
	if d.fail == "start" {
		return errors.New("start failed")
	}
	return nil
}
func (d *fixtureDriver) Wait(context.Context) (int, error) {
	*d.calls = append(*d.calls, "wait")
	if d.fail == "wait" {
		return -1, errors.New("wait failed")
	}
	return 0, nil
}
func (d *fixtureDriver) Cleanup(context.Context) error {
	*d.calls = append(*d.calls, "runtime-cleanup")
	if d.fail == "runtime-cleanup" {
		return errors.New("runtime still live")
	}
	return nil
}

type imageSource struct{}

func (imageSource) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

type inputSource struct{}

func (inputSource) Open(context.Context, evidence.Ref) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func pipelineFixture(t *testing.T, fail string) (*fixtureSession, workerSources, dependencies, *[]string) {
	t.Helper()
	job, err := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: "pipeline", Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}, Principal: "host", Image: "sha256:" + strings.Repeat("a", 64), Argv: []string{"/check"}, Network: sandbox.NetworkDenied, Limits: sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 1 << 20, WallTime: time.Minute, OutputBytes: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	cfg := service.WorkerConfiguration{Settings: service.WorkerConfig{Service: service.WorkerService{StateDirectory: t.TempDir(), MemoryBytes: 512 << 20}, Workspace: workspace.Policy{StagingBytes: 32 << 20, StagingInodes: 4096, ScratchInodes: 1024, ReserveMemoryBytes: 64 << 20}, ImageStaging: image.Limits{CompressedBytes: 1 << 20, ExpandedBytes: 2 << 20, Entries: 100, Timeout: time.Minute}, InputStaging: staging.Limits{PerInputBytes: 1 << 20, TotalBytes: 2 << 20}}}
	s := &fixtureSession{job: job, cfg: cfg, deadline: time.Now().Add(time.Minute), calls: &calls, fail: fail}
	root := filepath.Join(cfg.Settings.Service.StateDirectory, "workspaces", job.ID.String())
	paths := workspace.Paths{JobID: job.ID, JobDigest: job.Digest, Directory: root, Images: filepath.Join(root, "stage", "images"), Inputs: filepath.Join(root, "stage", "inputs"), Bundle: filepath.Join(root, "stage", "bundle"), Runtime: filepath.Join(root, "stage", "runtime"), Scratch: filepath.Join(root, "scratch")}
	w := &fixtureWorkspace{paths: paths, calls: &calls, fail: fail}
	sources := workerSources{Images: imageSource{}, AuthorizeImage: func(context.Context, sandbox.Job, string) error { return nil }, Inputs: inputSource{}, AuthorizeInput: func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error { return nil }}
	deps := dependencies{
		workspace: func(ctx context.Context, c workspace.Config) (work, error) {
			calls = append(calls, "workspace")
			if c.Deadline != s.deadline || c.Root != filepath.Join(cfg.Settings.Service.StateDirectory, "workspaces") || c.Job.Digest != job.Digest {
				t.Fatal("workspace lost configuration binding")
			}
			if err := c.Authorize(ctx, c.Job); err != nil {
				return nil, err
			}
			if fail == "workspace" {
				return nil, errors.New("mount refused")
			}
			return w, nil
		},
		image: func(ctx context.Context, c image.Config, digest string) (*image.Filesystem, error) {
			calls = append(calls, "image")
			if c.Root != paths.Images || c.CompressedBytes != cfg.Settings.ImageStaging.CompressedBytes {
				t.Fatal("image policy not pinned")
			}
			if err := c.Authorize(ctx, digest); err != nil {
				return nil, err
			}
			if fail == "image" {
				return nil, errors.New("secret source failure")
			}
			return &image.Filesystem{Digest: digest, Rootfs: filepath.Join(paths.Images, "verified", "rootfs")}, nil
		},
		inputs: func(ctx context.Context, c staging.Config, j sandbox.Job) (*staging.Inputs, error) {
			calls = append(calls, "inputs")
			if c.Root != paths.Inputs || c.PerInputBytes != cfg.Settings.InputStaging.PerInputBytes {
				t.Fatal("input policy not pinned")
			}
			if err := c.AuthorizeJob(ctx, j); err != nil {
				return nil, err
			}
			if fail == "inputs" {
				return nil, errors.New("input failed")
			}
			return &staging.Inputs{JobDigest: j.Digest, Paths: map[string]string{}}, nil
		},
		bundle: func(ctx context.Context, p workspace.Paths, _ sandbox.Job, rootfs string, _ map[string]string) error {
			calls = append(calls, "bundle")
			if p != paths || !strings.HasPrefix(rootfs, paths.Images+"/") {
				t.Fatal("bundle lost workspace")
			}
			if fail == "bundle" {
				return errors.New("bundle failed")
			}
			return ctx.Err()
		},
		driver: func(_ context.Context, c supervisor.RunscConfig, _ work) (supervisor.Driver, error) {
			calls = append(calls, "driver")
			if c.Bundle != paths.Bundle || c.StateRoot != paths.Runtime || c.JobDigest != job.Digest {
				t.Fatal("runtime lost job binding")
			}
			if fail == "nil-driver" {
				return nil, nil
			}
			if fail == "driver" {
				return nil, errors.New("runtime refused")
			}
			return &fixtureDriver{calls: &calls, fail: fail}, nil
		},
	}
	return s, sources, deps, &calls
}

func TestPipelineStagesExecutesCleansThenReports(t *testing.T) {
	s, sources, deps, calls := pipelineFixture(t, "")
	receipt, err := execute(t.Context(), s, sources, deps)
	want := []string{"begin", "workspace", "image", "inputs", "bundle", "driver", "create", "start", "wait", "runtime-cleanup", "workspace-cleanup", "report"}
	if err != nil || receipt.JobID != s.job.ID || !slices.Equal(*calls, want) {
		t.Fatal(receipt, err, *calls)
	}
	if len(s.reports) != 1 || string(s.reports[0].Stdout) != "candidate output" || s.reports[0].State != sandbox.StateExited {
		t.Fatal("execution report lost output")
	}
}

func TestPipelineFailureMatrixNeverReportsUnresolvedCleanup(t *testing.T) {
	for _, fail := range []string{"begin", "workspace", "image", "inputs", "bundle", "driver", "nil-driver", "start", "wait", "runtime-cleanup", "workspace-cleanup", "authorize", "report"} {
		t.Run(fail, func(t *testing.T) {
			s, sources, deps, calls := pipelineFixture(t, fail)
			receipt, err := execute(t.Context(), s, sources, deps)
			if err == nil {
				t.Fatal("failure disappeared")
			}
			if fail == "runtime-cleanup" || fail == "workspace-cleanup" {
				if !errors.Is(err, supervisor.ErrCleanup) || len(s.reports) != 0 || !receipt.JobID.IsNil() {
					t.Fatal("unclean execution reported", receipt, err, *calls)
				}
				if fail == "runtime-cleanup" && slices.Contains(*calls, "workspace-cleanup") {
					t.Fatal("workspace removed while runtime stop was unresolved")
				}
			} else if fail == "begin" || fail == "authorize" {
				if len(s.reports) != 0 {
					t.Fatal("denied worker reported")
				}
			} else if len(s.reports) != 1 {
				t.Fatal("safe failure was not reported", err, *calls)
			}
			for _, report := range s.reports {
				if report.JobDigest != s.job.Digest || report.State == "" {
					t.Fatal("unbound execution report")
				}
				if strings.Contains(report.Reason, "secret") {
					t.Fatal("source details leaked to report")
				}
			}
		})
	}
}

func TestPipelineRetriesOnlyExplicitReportBackpressure(t *testing.T) {
	s, sources, deps, calls := pipelineFixture(t, "")
	s.pending = true
	receipt, err := execute(t.Context(), s, sources, deps)
	if err != nil || receipt.JobID != s.job.ID || len(s.reports) != 2 || strings.Count(strings.Join(*calls, ","), "create") != 1 {
		t.Fatal("report retry repeated execution", receipt, err, *calls)
	}
}

func TestPipelineCancellationStillCleansWorkspace(t *testing.T) {
	s, sources, deps, calls := pipelineFixture(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	deps.image = func(context.Context, image.Config, string) (*image.Filesystem, error) {
		cancel()
		return nil, context.Canceled
	}
	receipt, err := execute(ctx, s, sources, deps)
	if !errors.Is(err, context.Canceled) || !receipt.JobID.IsNil() || !slices.Contains(*calls, "workspace-cleanup") || slices.Contains(*calls, "report") {
		t.Fatal("cancelled staging did not clean independently", receipt, err, *calls)
	}
}

func TestPipelineReportsDoNotReplayAfterUncertainOutcome(t *testing.T) {
	for _, mode := range []string{"report", "wrapped-pending"} {
		s, sources, deps, calls := pipelineFixture(t, mode)
		_, err := execute(t.Context(), s, sources, deps)
		if err == nil || len(s.reports) != 1 || strings.Count(strings.Join(*calls, ","), "create") != 1 {
			t.Fatal("uncertain report retried execution", err, *calls)
		}
	}
}

func TestPipelineMissingSourcesNeverStarts(t *testing.T) {
	s, _, deps, calls := pipelineFixture(t, "")
	if _, err := execute(t.Context(), s, workerSources{}, deps); err == nil || len(*calls) != 0 {
		t.Fatal("missing sources reached execution")
	}
	if receipt, err := Run(t.Context(), nil); err == nil || !receipt.JobID.IsNil() {
		t.Fatal("nil worker session accepted")
	}
}

func TestPipelineSourceErrorCannotSuppressWorkspaceCleanup(t *testing.T) {
	s, sources, deps, calls := pipelineFixture(t, "")
	deps.image = func(context.Context, image.Config, string) (*image.Filesystem, error) {
		return nil, supervisor.ErrCleanup
	}
	receipt, err := execute(t.Context(), s, sources, deps)
	if err == nil || receipt.JobID != s.job.ID || !slices.Contains(*calls, "workspace-cleanup") || len(s.reports) != 1 {
		t.Fatal("source error impersonated runtime cleanup", receipt, err, *calls)
	}
}

func TestPipelineSourcePolicyReceivesDetachedSignedIdentity(t *testing.T) {
	s, sources, deps, _ := pipelineFixture(t, "")
	sources.AuthorizeImage = func(ctx context.Context, candidate sandbox.Job, digest string) error {
		if candidate.Digest != s.job.Digest || candidate.Principal != s.job.Principal || digest != s.job.Image || cortex.ScopeFromContext(ctx).Canonical() != s.job.Scope.Canonical() {
			t.Fatal("source policy lost signed subject")
		}
		candidate.Argv[0] = "changed"
		candidate.Scope.Levels[0].Value = "other"
		return nil
	}
	if _, err := execute(t.Context(), s, sources, deps); err != nil {
		t.Fatal("policy callback mutated pipeline state", err)
	}
}

func TestPipelineBackpressureKeepsOriginalDeadline(t *testing.T) {
	s, sources, deps, calls := pipelineFixture(t, "always-pending")
	s.deadline = time.Now().Add(100 * time.Millisecond)
	_, err := execute(t.Context(), s, sources, deps)
	if !errors.Is(err, context.DeadlineExceeded) || strings.Count(strings.Join(*calls, ","), "create") != 1 || !slices.Contains(*calls, "workspace-cleanup") {
		t.Fatal("report wait extended execution", err, *calls)
	}
}
