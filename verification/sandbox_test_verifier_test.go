package verification_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
	"github.com/xraph/cortex/verification"
)

type evaluationMemory struct{ data map[id.ID]evidence.Artifact }

func (r *evaluationMemory) Put(_ context.Context, a evidence.Artifact) (evidence.Ref, error) {
	r.data[a.Ref.ID] = a
	return a.Ref, nil
}
func (r *evaluationMemory) Open(_ context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	a, ok := r.data[ref.ID]
	if !ok {
		return nil, evidence.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(a.Data)), nil
}
func (*evaluationMemory) Delete(context.Context, evidence.Ref, string) error {
	return errors.New("no deletion")
}

type evaluationClock struct{ now time.Time }

func (c evaluationClock) Now() time.Time { return c.now }

type evaluationAuthority struct{ denied bool }

func (a *evaluationAuthority) AuthorizeVerification(context.Context, verification.Binding) error {
	if a.denied {
		return errors.New("revoked")
	}
	return nil
}

type evaluationAuthenticity struct{ denied bool }

func (a *evaluationAuthenticity) VerifyReceipt(context.Context, sandbox.Receipt, sandbox.Job) error {
	if a.denied {
		return errors.New("unauthenticated")
	}
	return nil
}
func (a *evaluationAuthenticity) VerifyResult(context.Context, sandbox.Result, sandbox.Job) error {
	if a.denied {
		return errors.New("unauthenticated")
	}
	return nil
}

type evaluationSupervisor struct {
	calls  int
	assess func(verification.SandboxObservation) verification.SandboxAssessment
}

func (s *evaluationSupervisor) Assess(_ context.Context, o verification.SandboxObservation) (verification.SandboxAssessment, error) {
	s.calls++
	return s.assess(o), nil
}

type evaluationRunner struct {
	repo      *evaluationMemory
	now       time.Time
	jobs      []sandbox.Job
	submitErr bool
	running   bool
	corrupt   bool
	collected int
	cancelled int
	cancelErr error
}

func (r *evaluationRunner) Capabilities(context.Context) (sandbox.Capabilities, error) {
	return sandbox.Capabilities{SchemaVersion: 1, Runtime: "runsc", RuntimeDigest: evidence.Digest([]byte("runtime")), Profile: sandbox.ProfileDenyAll, Qualification: "fixture only", QualifiedUntil: r.now.Add(time.Hour), Limits: evaluationLimits()}, nil
}
func (r *evaluationRunner) Submit(ctx context.Context, j sandbox.Job) (sandbox.Receipt, error) {
	r.jobs = append(r.jobs, j)
	if r.submitErr {
		return sandbox.Receipt{}, errors.New("lost reply")
	}
	caps, _ := r.Capabilities(ctx)
	return sandbox.Receipt{SchemaVersion: 1, Job: j, Capabilities: caps, Broker: "fixture", AcceptedAt: r.now}, nil
}
func (r *evaluationRunner) Inspect(ctx context.Context, key id.ID) (sandbox.JobStatus, error) {
	for _, j := range r.jobs {
		if j.ID == key {
			caps, _ := r.Capabilities(ctx)
			state := sandbox.StateExited
			if r.running {
				state = sandbox.StateRunning
			}
			return sandbox.JobStatus{Receipt: sandbox.Receipt{SchemaVersion: 1, Job: j, Capabilities: caps, Broker: "fixture", AcceptedAt: r.now}, State: state, Revision: 1, UpdatedAt: r.now}, nil
		}
	}
	return sandbox.JobStatus{}, errors.New("absent")
}
func (r *evaluationRunner) Cancel(context.Context, id.ID) error {
	r.cancelled++
	return r.cancelErr
}
func (r *evaluationRunner) Collect(_ context.Context, key id.ID) (sandbox.Result, error) {
	r.collected++
	for _, j := range r.jobs {
		if j.ID == key {
			out := j.Outputs[0]
			a, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: out.ID, Scope: j.Scope, MediaType: out.MediaType, Version: j.Digest, Producer: sandbox.ArtifactProducer, SourceURI: j.OutputSource(out), Sensitivity: evidence.SensitivityRestricted, CreatedAt: r.now, RetainUntil: r.now.Add(time.Hour)}, Data: []byte("untrusted exported content")})
			if err != nil {
				return sandbox.Result{}, err
			}
			r.repo.data[a.Ref.ID] = a
			if r.corrupt {
				broken := a
				broken.Data = []byte("swapped")
				r.repo.data[a.Ref.ID] = broken
			}
			return sandbox.Result{JobDigest: j.Digest, State: sandbox.StateExited, Stdout: []byte(`{"passed":true}`), Artifacts: []evidence.Ref{a.Ref}}, nil
		}
	}
	return sandbox.Result{}, errors.New("absent")
}
func evaluationLimits() sandbox.Limits {
	return sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 1 << 20, WallTime: time.Minute, OutputBytes: 1024, ArtifactBytes: 1024}
}

func sandboxEvaluationFixture(t *testing.T) (verification.SandboxTestConfig, verification.TestRequest, *evaluationRunner, *evaluationSupervisor, *evaluationAuthority) {
	t.Helper()
	m, req, _ := evaluationManifestFixture(t)
	now := time.Now().UTC()
	repo := &evaluationMemory{data: map[id.ID]evidence.Artifact{}}
	refs := []*evidence.Ref{&m.Binding.Candidate, &m.TestBundle, &m.FixtureDataset, &m.ResultSchema}
	for i, ref := range refs {
		a, err := evidence.Seal(evidence.Artifact{Ref: *ref, Data: []byte{byte(i)}})
		require.NoError(t, err)
		a.Ref.CreatedAt = now.Add(-time.Minute)
		a.Ref.RetainUntil = now.Add(2 * time.Hour)
		*ref = a.Ref
		repo.data[a.Ref.ID] = a
	}
	m.Binding.Evidence = []evidence.Ref{m.TestBundle, m.FixtureDataset, m.ResultSchema}
	m.Binding.Context.Sources = nil
	m.StateProbes = nil
	req.Binding = m.Binding
	req.Deadline = now.Add(time.Minute)
	m, err := verification.SealEvaluationManifest(m)
	require.NoError(t, err)
	registry, err := verification.NewEvaluationRegistry([]verification.EvaluationManifest{m})
	require.NoError(t, err)
	job := func(key string, inputs []sandbox.Input) sandbox.Job {
		sealed, sealErr := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: key, Scope: m.Binding.Scope, Principal: m.Binding.Context.PrincipalRef, Image: "sha256:" + evidence.Digest([]byte(key)), Argv: []string{"/bin/evaluate"}, Inputs: inputs, Outputs: []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/result", MediaType: "application/octet-stream", MaxBytes: 1024}}, Network: sandbox.NetworkDenied, Limits: evaluationLimits()})
		require.NoError(t, sealErr)
		return sealed
	}
	plan := verification.SandboxTestPlan{ManifestDigest: m.Digest, Candidate: job("candidate", []sandbox.Input{{Path: "/inputs/candidate", Ref: m.Binding.Candidate}}), Grader: job("grader", []sandbox.Input{{Path: "/inputs/tests", Ref: m.TestBundle}, {Path: "/inputs/fixtures", Ref: m.FixtureDataset}, {Path: "/inputs/result-schema", Ref: m.ResultSchema}})}
	runner := &evaluationRunner{repo: repo, now: now}
	supervisor := &evaluationSupervisor{assess: func(o verification.SandboxObservation) verification.SandboxAssessment {
		checks := make([]verification.SupervisedCheck, len(o.Manifest.RequiredChecks))
		for i, name := range o.Manifest.RequiredChecks {
			checks[i] = verification.SupervisedCheck{Name: name, Status: verification.StatusPassed}
		}
		return verification.SandboxAssessment{ManifestDigest: o.Manifest.Digest, SupervisorRevision: o.Manifest.SupervisorRevision, CandidateDigest: o.Candidate.Digest, GraderDigest: o.Grader.Digest, Checks: checks}
	}}
	authority := &evaluationAuthority{}
	return verification.SandboxTestConfig{Registry: registry, Plans: []verification.SandboxTestPlan{plan}, Runner: runner, Repository: repo, Authenticity: &evaluationAuthenticity{}, Supervisor: supervisor, Authorizer: authority, Clock: evaluationClock{now}, MaxConcurrent: 1, PollInterval: time.Millisecond * 10}, req, runner, supervisor, authority
}

func TestSandboxEvaluationSeparatesJobsAndProtectedMaterial(t *testing.T) {
	cfg, req, runner, supervisor, _ := sandboxEvaluationFixture(t)
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	result, err := verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, result.Status)
	require.Len(t, runner.jobs, 2)
	require.NotEqual(t, runner.jobs[0].ID, runner.jobs[1].ID)
	require.Len(t, runner.jobs[0].Inputs, 1)
	require.Len(t, runner.jobs[1].Inputs, 4)
	require.Equal(t, "/inputs/candidate-export", runner.jobs[1].Inputs[3].Path)
	require.Empty(t, runner.jobs[1].Env)
	require.Equal(t, 1, supervisor.calls)
}
func TestForgedStdoutPassDoesNotPass(t *testing.T) {
	cfg, req, _, supervisor, _ := sandboxEvaluationFixture(t)
	original := supervisor.assess
	supervisor.assess = func(o verification.SandboxObservation) verification.SandboxAssessment {
		a := original(o)
		a.Checks[0].Status = verification.StatusFailed
		return a
	}
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	result, err := verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.NoError(t, err)
	require.Equal(t, verification.StatusFailed, result.Status)
}
func TestSandboxEvaluationRejectsWrongSupervisorBinding(t *testing.T) {
	cfg, req, _, supervisor, _ := sandboxEvaluationFixture(t)
	original := supervisor.assess
	supervisor.assess = func(o verification.SandboxObservation) verification.SandboxAssessment {
		a := original(o)
		a.CandidateDigest = evidence.Digest([]byte("other"))
		return a
	}
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	_, err = verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.Error(t, err)
}
func TestSandboxEvaluationRefusesAmbiguousDispatchWithoutReplacement(t *testing.T) {
	cfg, req, runner, supervisor, _ := sandboxEvaluationFixture(t)
	runner.submitErr = true
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	_, err = verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	require.Len(t, runner.jobs, 1)
	require.Zero(t, runner.collected)
	require.Zero(t, supervisor.calls)
}
func TestSandboxEvaluationChecksExportBytesBeforeGrader(t *testing.T) {
	cfg, req, runner, supervisor, _ := sandboxEvaluationFixture(t)
	runner.corrupt = true
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	_, err = verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.Error(t, err)
	require.Len(t, runner.jobs, 1)
	require.Zero(t, supervisor.calls)
}

func TestSandboxEvaluationCleanupFailureRemainsUncertain(t *testing.T) {
	cfg, req, runner, supervisor, _ := sandboxEvaluationFixture(t)
	cleanupErr := errors.New("broker unavailable during cancellation")
	runner.cancelErr = cleanupErr
	cfg.Authenticity = &evaluationAuthenticity{denied: true}
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	result, err := verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	require.ErrorIs(t, err, cleanupErr)
	require.NotEqual(t, verification.StatusPassed, result.Status)
	require.Equal(t, 1, runner.cancelled)
	require.Zero(t, supervisor.calls)
}

func TestSandboxEvaluationRejectsMalformedAssessment(t *testing.T) {
	for _, name := range []string{"duplicate", "unknown", "unsupported", "wrong manifest", "wrong supervisor", "wrong grader"} {
		t.Run(name, func(t *testing.T) {
			cfg, req, _, supervisor, _ := sandboxEvaluationFixture(t)
			original := supervisor.assess
			supervisor.assess = func(o verification.SandboxObservation) verification.SandboxAssessment {
				a := original(o)
				switch name {
				case "duplicate":
					a.Checks[1] = a.Checks[0]
				case "unknown":
					a.Checks[0].Name = "unregistered check"
				case "unsupported":
					a.Checks[0].Status = verification.Status("optimistic")
				case "wrong manifest":
					a.ManifestDigest = evidence.Digest([]byte("other"))
				case "wrong supervisor":
					a.SupervisorRevision = evidence.Digest([]byte("other"))
				case "wrong grader":
					a.GraderDigest = evidence.Digest([]byte("other"))
				}
				return a
			}
			verifier, err := verification.NewSandboxTestVerifier(cfg)
			require.NoError(t, err)
			_, err = verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
			require.ErrorIs(t, err, verification.ErrInvalid)
		})
	}
}
func TestSandboxEvaluationRechecksAuthorityBeforeVerdict(t *testing.T) {
	cfg, req, _, supervisor, authority := sandboxEvaluationFixture(t)
	original := supervisor.assess
	supervisor.assess = func(o verification.SandboxObservation) verification.SandboxAssessment {
		authority.denied = true
		return original(o)
	}
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	_, err = verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.Error(t, err)
}
func TestSandboxEvaluationRejectsUnsafePlanBeforeExecution(t *testing.T) {
	for _, mode := range []string{"tests in candidate", "credential", "same job", "same key", "wrong candidate", "unregistered", "missing authenticity", "missing supervisor"} {
		t.Run(mode, func(t *testing.T) {
			cfg, _, runner, _, _ := sandboxEvaluationFixture(t)
			switch mode {
			case "tests in candidate":
				cfg.Plans[0].Candidate.Inputs = append(cfg.Plans[0].Candidate.Inputs, cfg.Plans[0].Grader.Inputs[0])
			case "credential":
				cfg.Plans[0].Grader.Env = map[string]string{"TOKEN": "secret"}
			case "same job":
				cfg.Plans[0].Grader.ID = cfg.Plans[0].Candidate.ID
			case "same key":
				cfg.Plans[0].Grader.Key = cfg.Plans[0].Candidate.Key
			case "wrong candidate":
				cfg.Plans[0].Candidate.Inputs[0].Ref = cfg.Plans[0].Grader.Inputs[0].Ref
			case "unregistered":
				cfg.Plans[0].ManifestDigest = evidence.Digest([]byte("other"))
			case "missing authenticity":
				cfg.Authenticity = nil
			case "missing supervisor":
				cfg.Supervisor = nil
			}
			// Resealing cannot make a forbidden plan authoritative.
			if job, err := sandbox.Seal(cfg.Plans[0].Candidate); err == nil {
				cfg.Plans[0].Candidate = job
			}
			if job, err := sandbox.Seal(cfg.Plans[0].Grader); err == nil {
				cfg.Plans[0].Grader = job
			}
			_, err := verification.NewSandboxTestVerifier(cfg)
			require.Error(t, err)
			require.Empty(t, runner.jobs)
		})
	}
}

func TestSandboxEvaluationCancelsOwnedJobOnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, req, runner, supervisor, _ := sandboxEvaluationFixture(t)
		runner.running = true
		verifier, err := verification.NewSandboxTestVerifier(cfg)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(cortex.WithScope(t.Context(), req.Binding.Scope), 30*time.Millisecond)
		defer cancel()
		_, err = verifier.EvaluateTests(ctx, req)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Len(t, runner.jobs, 1)
		require.Equal(t, 1, runner.cancelled)
		require.Zero(t, supervisor.calls)
	})
}

func TestSandboxEvaluationDeadlineBeforeDispatchOwnsNoJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg, req, runner, supervisor, _ := sandboxEvaluationFixture(t)
		verifier, err := verification.NewSandboxTestVerifier(cfg)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(cortex.WithScope(t.Context(), req.Binding.Scope), 30*time.Millisecond)
		defer cancel()
		time.Sleep(30 * time.Millisecond)
		synctest.Wait()
		_, err = verifier.EvaluateTests(ctx, req)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Empty(t, runner.jobs)
		require.Zero(t, runner.cancelled)
		require.Zero(t, supervisor.calls)
	})
}

func TestSandboxEvaluationMissingCheckIsInconclusive(t *testing.T) {
	cfg, req, _, supervisor, _ := sandboxEvaluationFixture(t)
	original := supervisor.assess
	supervisor.assess = func(o verification.SandboxObservation) verification.SandboxAssessment {
		a := original(o)
		a.Checks = a.Checks[:1]
		return a
	}
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	result, err := verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.NoError(t, err)
	require.Equal(t, verification.StatusInconclusive, result.Status)
}
func TestSandboxEvaluationRefusesUnsignedReceiptBeforeCollection(t *testing.T) {
	cfg, req, runner, supervisor, _ := sandboxEvaluationFixture(t)
	cfg.Authenticity = &evaluationAuthenticity{denied: true}
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	_, err = verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.Error(t, err)
	require.Len(t, runner.jobs, 1)
	require.Zero(t, runner.collected)
	require.Zero(t, supervisor.calls)
	require.Equal(t, 1, runner.cancelled)
}
func TestSandboxEvaluationFreezesHostPlan(t *testing.T) {
	cfg, req, runner, _, _ := sandboxEvaluationFixture(t)
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	cfg.Plans[0].Candidate.Argv[0] = "/bin/mutated"
	cfg.Plans[0].Grader.Inputs[0].Path = "/inputs/changed"
	_, err = verifier.EvaluateTests(cortex.WithScope(t.Context(), req.Binding.Scope), req)
	require.NoError(t, err)
	require.Equal(t, "/bin/evaluate", runner.jobs[0].Argv[0])
	require.Equal(t, "/inputs/tests", runner.jobs[1].Inputs[0].Path)
}

func TestSandboxEvaluationBoundsConcurrentAdmissionAndReleasesSlot(t *testing.T) {
	cfg, req, _, supervisor, _ := sandboxEvaluationFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	original := supervisor.assess
	supervisor.assess = func(o verification.SandboxObservation) verification.SandboxAssessment {
		select {
		case entered <- struct{}{}:
		case <-release:
		}
		<-release
		return original(o)
	}
	verifier, err := verification.NewSandboxTestVerifier(cfg)
	require.NoError(t, err)
	ctx := cortex.WithScope(t.Context(), req.Binding.Scope)
	done := make(chan error, 1)
	go func() { _, callErr := verifier.EvaluateTests(ctx, req); done <- callErr }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("evaluation did not reach supervisor")
	}
	second := make(chan error, 1)
	go func() { _, callErr := verifier.EvaluateTests(ctx, req); second <- callErr }()
	select {
	case err = <-second:
		require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("overflow entered supervisor instead of being refused")
	}
	close(release)
	require.NoError(t, <-done)
	result, err := verifier.EvaluateTests(ctx, req)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, result.Status)
}
