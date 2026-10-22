//go:build linux

// Package worker assembles one authenticated sandbox execution attempt.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/internal/image"
	"github.com/xraph/cortex/integrations/sandbox/internal/service"
	"github.com/xraph/cortex/integrations/sandbox/internal/staging"
	"github.com/xraph/cortex/integrations/sandbox/internal/supervisor"
	"github.com/xraph/cortex/integrations/sandbox/internal/workspace"
	"github.com/xraph/cortex/integrations/sandbox/oci"
	"github.com/xraph/cortex/sandbox"
)

type workerSources struct {
	Images          image.Source
	AuthorizeImage  func(context.Context, sandbox.Job, string) error
	Inputs          staging.Source
	AuthorizeInput  func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error
	Artifacts       capture.Repository
	AuthorizeOutput func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error
}

type inputAuthorizer func(context.Context, evidence.Action, evidence.Ref) error

func (f inputAuthorizer) AuthorizeEvidence(ctx context.Context, action evidence.Action, ref evidence.Ref) error {
	return f(ctx, action, ref)
}

type session interface {
	CaptureJournal() capture.Journal
	Job() (sandbox.Job, error)
	Configuration() service.WorkerConfiguration
	Deadline() time.Time
	BeginExecution(context.Context) error
	Authorize(context.Context) error
	Report(context.Context, sandbox.Result) (service.WorkerReportReceipt, error)
}
type work interface {
	Paths() (workspace.Paths, error)
	Check(context.Context) error
	Close(context.Context) error
}
type dependencies struct {
	workspace func(context.Context, workspace.Config) (work, error)
	image     func(context.Context, image.Config, string) (*image.Filesystem, error)
	inputs    func(context.Context, staging.Config, sandbox.Job) (*staging.Inputs, error)
	bundle    func(context.Context, workspace.Paths, sandbox.Job, string, map[string]string) error
	driver    func(context.Context, supervisor.RunscConfig, work) (supervisor.Driver, error)
	capture   func(context.Context, capture.Config, sandbox.Job) ([]evidence.Ref, error)
}

// Run stages and executes one bootstrapped job, cleans up and submits its report.
// It captures bounded stdout/stderr and declared files as restricted evidence.
// Content sources come from the private session. The worker process must exit
// after this call.
// Failed runtime cleanup leaves storage intact for independent recovery. A report
// acknowledgement confirms durable receipt only, never result finalization.
func Run(ctx context.Context, worker *service.WorkerSession) (service.WorkerReportReceipt, error) {
	if worker == nil {
		return service.WorkerReportReceipt{}, fmt.Errorf("cortex: worker session is required")
	}
	return execute(ctx, worker, bindWorkerSources(worker), dependencies{
		workspace: func(ctx context.Context, cfg workspace.Config) (work, error) {
			w, err := workspace.Create(ctx, cfg)
			if w == nil {
				return nil, err
			}
			return w, err
		},
		image: image.Stage, inputs: staging.Stage, bundle: writeBundle, capture: capture.Collect,
		driver: func(ctx context.Context, cfg supervisor.RunscConfig, w work) (supervisor.Driver, error) {
			native, ok := w.(*workspace.Workspace)
			if !ok {
				return nil, fmt.Errorf("cortex: native worker workspace is required")
			}
			return supervisor.NewWorkspaceRunsc(ctx, cfg, native)
		},
	})
}

type contentWorker interface {
	ImageSource() image.Source
	session
	InputSource() staging.Source
	ArtifactRepository() capture.Repository
}

func bindWorkerSources(worker contentWorker) workerSources {
	return workerSources{Images: worker.ImageSource(), Inputs: worker.InputSource(), Artifacts: worker.ArtifactRepository(),
		AuthorizeImage: func(ctx context.Context, job sandbox.Job, pin string) error {
			pinned, err := worker.Job()
			if err != nil {
				return err
			}
			if job.Validate() != nil || job.Digest != pinned.Digest || pin != pinned.Image || cortex.ScopeFromContext(ctx).Canonical() != pinned.Scope.Canonical() {
				return broker.ErrDenied
			}
			return worker.Authorize(ctx)
		},
		AuthorizeInput: func(ctx context.Context, job sandbox.Job, action evidence.Action, ref evidence.Ref) error {
			return authorizeEvidence(ctx, worker, job, action, ref, false)
		},
		AuthorizeOutput: func(ctx context.Context, job sandbox.Job, action evidence.Action, ref evidence.Ref) error {
			return authorizeEvidence(ctx, worker, job, action, ref, true)
		},
	}
}

// Local guards bind staging/capture to the signed job. The daemon independently
// checks the current provider grant at each byte operation and after readback.
func authorizeEvidence(ctx context.Context, worker session, job sandbox.Job, action evidence.Action, ref evidence.Ref, artifact bool) error {
	pinned, err := worker.Job()
	if err != nil {
		return err
	}
	if job.Validate() != nil || job.Digest != pinned.Digest || cortex.ScopeFromContext(ctx).Canonical() != pinned.Scope.Canonical() {
		return broker.ErrDenied
	}
	if artifact {
		if action != evidence.ActionRead && action != evidence.ActionPut {
			return evidence.ErrDenied
		}
		if validationErr := pinned.ValidateArtifacts([]evidence.Ref{ref}, false); validationErr != nil {
			return validationErr
		}
	} else {
		if action != evidence.ActionRead {
			return evidence.ErrDenied
		}
		binding, bindingErr := ref.BindingDigest()
		if bindingErr != nil {
			return bindingErr
		}
		found := false
		for _, input := range pinned.Inputs {
			digest, digestErr := input.Ref.BindingDigest()
			if digestErr == nil && digest == binding {
				found = true
				break
			}
		}
		if !found {
			return evidence.ErrDenied
		}
	}
	return worker.Authorize(ctx)
}

func execute(ctx context.Context, worker session, sources workerSources, deps dependencies) (service.WorkerReportReceipt, error) {
	if sources.Images == nil || sources.Inputs == nil || sources.AuthorizeInput == nil || sources.AuthorizeImage == nil {
		return service.WorkerReportReceipt{}, fmt.Errorf("cortex: worker source services are incomplete")
	}
	job, err := worker.Job()
	if err != nil {
		return service.WorkerReportReceipt{}, err
	}
	if validationErr := job.Validate(); validationErr != nil {
		return service.WorkerReportReceipt{}, validationErr
	}
	cfg := worker.Configuration()
	if len(job.Outputs) != 0 && (sources.Artifacts == nil || worker.CaptureJournal() == nil || sources.AuthorizeOutput == nil || deps.capture == nil || capture.ValidateRetention(cfg.Settings.CaptureRetention) != nil) {
		return service.WorkerReportReceipt{}, fmt.Errorf("cortex: sandbox artifact capture is unavailable")
	}
	if entryErr := worker.BeginExecution(ctx); entryErr != nil {
		return service.WorkerReportReceipt{}, entryErr
	}
	operation, cancel := context.WithDeadline(scoped(ctx, job), worker.Deadline())
	defer cancel()
	authorize := func(ctx context.Context, candidate sandbox.Job) error {
		if candidate.Digest != job.Digest || candidate.Validate() != nil {
			return broker.ErrDenied
		}
		return worker.Authorize(scoped(ctx, job))
	}
	w, setupErr := deps.workspace(operation, workspace.Config{Root: filepath.Join(cfg.Settings.Service.StateDirectory, "workspaces"), Policy: cfg.Settings.Workspace, Job: job, ServiceMemory: cfg.Settings.Service.MemoryBytes, Deadline: worker.Deadline(), Authorize: authorize})
	result := sandbox.Result{JobDigest: job.Digest, State: sandbox.StateFailed, ExitCode: -1, Reason: "sandbox workspace setup failed"}
	if setupErr == nil {
		prepared := runPrepared(operation, worker, sources, deps, w, cfg, job, authorize)
		if prepared.cleanupPending {
			return service.WorkerReportReceipt{}, prepared.failure
		}
		result, setupErr = prepared.result, prepared.failure
	}
	if w != nil {
		cleanup, stop := context.WithTimeout(scoped(context.Background(), job), 10*time.Second)
		cleanupErr := w.Close(cleanup)
		cleanupErr = errors.Join(cleanupErr, cleanup.Err())
		stop()
		if cleanupErr != nil {
			return service.WorkerReportReceipt{}, errors.Join(supervisor.ErrCleanup, setupErr, cleanupErr)
		}
	}
	if currentErr := worker.Authorize(operation); currentErr != nil {
		return service.WorkerReportReceipt{}, errors.Join(setupErr, currentErr)
	}
	receipt, reportErr := report(operation, worker, result)
	return receipt, errors.Join(setupErr, reportErr)
}

type preparedResult struct {
	result         sandbox.Result
	failure        error
	cleanupPending bool
}

func runPrepared(ctx context.Context, worker session, sources workerSources, deps dependencies, w work, cfg service.WorkerConfiguration, job sandbox.Job, authorize func(context.Context, sandbox.Job) error) preparedResult {
	failure := func(reason string, err error) preparedResult {
		return preparedResult{result: sandbox.Result{JobDigest: job.Digest, State: sandbox.StateFailed, ExitCode: -1, Reason: reason}, failure: err}
	}
	if w == nil {
		return failure("sandbox workspace setup failed", fmt.Errorf("cortex: workspace factory returned no workspace"))
	}
	paths, err := w.Paths()
	if err != nil {
		return failure("sandbox workspace setup failed", err)
	}
	if paths.JobID != job.ID || paths.JobDigest != job.Digest {
		return failure("sandbox workspace setup failed", broker.ErrDenied)
	}
	if err = w.Check(ctx); err != nil {
		return failure("sandbox workspace verification failed", err)
	}
	imageLimits := cfg.Settings.ImageStaging
	filesystem, err := deps.image(ctx, image.Config{Root: paths.Images, Architecture: runtime.GOARCH, OwnerUID: 0, Source: sources.Images, CompressedBytes: imageLimits.CompressedBytes, ExpandedBytes: imageLimits.ExpandedBytes, Entries: imageLimits.Entries, Timeout: imageLimits.Timeout, Authorize: func(ctx context.Context, digest string) error {
		if digest != job.Image {
			return broker.ErrDenied
		}
		if currentErr := authorize(ctx, job); currentErr != nil {
			return currentErr
		}
		candidate, cloneErr := sandbox.Seal(job)
		if cloneErr != nil {
			return cloneErr
		}
		if sourceErr := sources.AuthorizeImage(scoped(ctx, job), candidate, digest); sourceErr != nil {
			return sourceErr
		}
		return ctx.Err()
	}}, job.Image)
	if err != nil {
		return failure("sandbox image staging failed", err)
	}
	if filesystem == nil || filesystem.Digest != job.Image {
		return failure("sandbox image staging failed", broker.ErrDenied)
	}
	if err = w.Check(ctx); err != nil {
		return failure("sandbox workspace verification failed", err)
	}
	limits := cfg.Settings.InputStaging
	inputs, err := deps.inputs(ctx, staging.Config{Root: paths.Inputs, OwnerUID: 0, PerInputBytes: limits.PerInputBytes, TotalBytes: limits.TotalBytes, Source: sources.Inputs, Policy: inputAuthorizer(func(ctx context.Context, action evidence.Action, ref evidence.Ref) error {
		if action != evidence.ActionRead {
			return evidence.ErrDenied
		}
		if currentErr := authorize(ctx, job); currentErr != nil {
			return currentErr
		}
		candidate, cloneErr := sandbox.Seal(job)
		if cloneErr != nil {
			return cloneErr
		}
		ref.Scope.Levels = slices.Clone(ref.Scope.Levels)
		if sourceErr := sources.AuthorizeInput(scoped(ctx, job), candidate, action, ref); sourceErr != nil {
			return sourceErr
		}
		return ctx.Err()
	}), AuthorizeJob: authorize}, job)
	if err != nil {
		return failure("sandbox input staging failed", err)
	}
	if inputs == nil || inputs.JobDigest != job.Digest {
		return failure("sandbox input staging failed", broker.ErrDenied)
	}
	if err = w.Check(ctx); err != nil {
		return failure("sandbox workspace verification failed", err)
	}
	if err = deps.bundle(ctx, paths, job, filesystem.Rootfs, inputs.Paths); err != nil {
		return failure("sandbox bundle preparation failed", err)
	}
	driver, err := deps.driver(ctx, supervisor.RunscConfig{Binary: filepath.Join(cfg.Settings.Runtime.Directory, "runsc"), StateRoot: paths.Runtime, Bundle: paths.Bundle, Rootfs: filesystem.Rootfs, Inputs: inputs.Paths, JobDigest: job.Digest, Manifest: cfg.Settings.Runtime.Manifest, DeploymentDigest: cfg.RuntimeDigest}, w)
	if err != nil {
		return failure("sandbox runtime preparation failed", err)
	}
	if driver == nil {
		return failure("sandbox runtime preparation failed", fmt.Errorf("cortex: runtime factory returned no driver"))
	}
	if err = authorize(ctx, job); err != nil {
		return failure("sandbox execution authorization failed", err)
	}
	result, runErr := supervisor.Run(ctx, driver, job, worker.Deadline())
	if errors.Is(runErr, supervisor.ErrCleanup) {
		return preparedResult{failure: runErr, cleanupPending: true}
	}
	if result.State == sandbox.StateExited && len(job.Outputs) != 0 {
		refs, captureErr := deps.capture(ctx, capture.Config{Root: paths.Scratch, Repository: sources.Artifacts, Journal: worker.CaptureJournal(), Retention: cfg.Settings.CaptureRetention, Authorize: authorize, Check: w.Check, Policy: sources.AuthorizeOutput}, job)
		if validationErr := job.ValidateArtifacts(refs, captureErr == nil); validationErr != nil {
			refs = nil
			captureErr = errors.Join(captureErr, validationErr)
		}
		result.Artifacts = refs
		if captureErr != nil {
			result.State, result.ExitCode, result.Reason = sandbox.StateFailed, -1, "sandbox artifact capture failed"
			runErr = errors.Join(runErr, captureErr)
		}
	}
	return preparedResult{result: result, failure: runErr}
}

func scoped(ctx context.Context, job sandbox.Job) context.Context {
	scope := job.Scope
	scope.Levels = slices.Clone(scope.Levels)
	return cortex.WithScope(ctx, scope)
}

func report(ctx context.Context, worker session, result sandbox.Result) (service.WorkerReportReceipt, error) {
	for {
		receipt, err := worker.Report(ctx, result)
		if err != broker.ErrPending { //nolint:errorlint // Only explicit backpressure permits replay of the same report.
			return receipt, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return service.WorkerReportReceipt{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func writeBundle(ctx context.Context, paths workspace.Paths, job sandbox.Job, rootfs string, inputs map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	spec, err := oci.WorkspaceSpec(job, rootfs, inputs, paths.Scratch)
	if err != nil {
		return err
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	if len(body) > 1<<20 {
		return fmt.Errorf("cortex: worker OCI bundle exceeds one MiB")
	}
	directory, err := hostfs.OpenDirectory(paths.Bundle, 0)
	if err != nil {
		return err
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("cortex: worker bundle must be private")
	}
	fd, err := unix.Openat(int(directory.Fd()), "config.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "config.json")
	_, writeErr := file.Write(body)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr, ctx.Err())
}
