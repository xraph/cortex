package verification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
)

// SandboxTestPlan is host configuration for one registered manifest. Grader is
// a sealed template with protected inputs; the verified candidate export is
// appended at /inputs/candidate-export before the grader job is sealed again.
// IDs and keys must be reserved durably by the host before registration.
type SandboxTestPlan struct {
	ManifestDigest string
	Candidate      sandbox.Job
	Grader         sandbox.Job
}

// SandboxAuthenticity checks pinned broker signatures, not candidate assertions.
// Implementations must authenticate the expected broker and complete job binding.
type SandboxAuthenticity interface {
	VerifyReceipt(context.Context, sandbox.Receipt, sandbox.Job) error
	VerifyResult(context.Context, sandbox.Result, sandbox.Job) error
}

// SandboxObservation contains completed job identities and captured references.
// Stdout and stderr are deliberately absent. They cannot supply verdicts.
type SandboxObservation struct {
	Manifest           EvaluationManifest
	Candidate          sandbox.Job
	Grader             sandbox.Job
	CandidateArtifacts []evidence.Ref
	GraderArtifacts    []evidence.Ref
}

// SupervisedCheck is an external supervisor's observation of one required check.
type SupervisedCheck struct {
	Name   string
	Status Status
}

// SandboxAssessment binds external observations to both exact jobs. The trusted
// supervisor must retain authenticated evidence, independently corroborate tests
// that execute candidate code, and never interpret workload text as instructions.
type SandboxAssessment struct {
	ManifestDigest     string
	SupervisorRevision string
	CandidateDigest    string
	GraderDigest       string
	Checks             []SupervisedCheck
	ObservationRefs    []evidence.Ref
}

// SandboxSupervisor obtains authoritative observations outside both workloads.
// It receives no effect tools or production credentials through this interface.
// Implementations must reconcile retries by manifest and job digests, retaining
// the original observations so a repeated request cannot change its assessment.
type SandboxSupervisor interface {
	Assess(context.Context, SandboxObservation) (SandboxAssessment, error)
}

// SandboxTestConfig requires explicit host authority and authenticated runner
// services. Registration does not qualify a deployment. Ports must honor context
// cancellation; the caller owns total evaluator resource/cost accounting.
// MaxConcurrent bounds in-flight evaluations and must be between one and 64.
type SandboxTestConfig struct {
	Registry      *EvaluationRegistry
	Plans         []SandboxTestPlan
	Runner        sandbox.Runner
	Repository    evidence.Repository
	Authenticity  SandboxAuthenticity
	Supervisor    SandboxSupervisor
	Authorizer    BindingAuthorizer
	Clock         Clock
	MaxConcurrent int
	PollInterval  time.Duration
}

// SandboxTestVerifier runs registered candidate/grader jobs and asks an external
// supervisor for their outcome. It never publishes or exports to production.
type SandboxTestVerifier struct {
	config SandboxTestConfig
	plans  map[string]SandboxTestPlan
	slots  chan struct{}
}

var _ TestEvaluator = (*SandboxTestVerifier)(nil)

// NewSandboxTestVerifier freezes registered execution plans.
func NewSandboxTestVerifier(config SandboxTestConfig) (*SandboxTestVerifier, error) {
	if config.MaxConcurrent < 1 || config.MaxConcurrent > 64 || config.Registry == nil || len(config.Plans) == 0 || len(config.Plans) > 256 || nilHostPort(config.Runner) || nilHostPort(config.Repository) || nilHostPort(config.Authenticity) || nilHostPort(config.Supervisor) || nilHostPort(config.Authorizer) || nilHostPort(config.Clock) || config.PollInterval < 10*time.Millisecond || config.PollInterval > time.Second {
		return nil, ErrInvalid
	}
	manifests := make(map[string]EvaluationManifest)
	for _, body := range config.Registry.manifests {
		var m EvaluationManifest
		if json.Unmarshal(body, &m) != nil {
			return nil, ErrInvalid
		}
		manifests[m.Digest] = m
	}
	body, err := json.Marshal(config.Plans)
	if err != nil || len(body) > 16<<20 {
		return nil, ErrInvalid
	}
	var plans []SandboxTestPlan
	if json.Unmarshal(body, &plans) != nil {
		return nil, ErrInvalid
	}
	config.Plans = nil
	v := &SandboxTestVerifier{config: config, plans: make(map[string]SandboxTestPlan, len(plans)), slots: make(chan struct{}, config.MaxConcurrent)}
	jobs, keys, outputs := make(map[id.ID]bool), make(map[string]bool), make(map[id.ID]bool)
	inputs := make(map[id.ID]bool)
	for _, plan := range plans {
		m, ok := manifests[plan.ManifestDigest]
		if !ok || v.plans[plan.ManifestDigest].ManifestDigest != "" || ValidateSandboxTestPlan(plan, m) != nil {
			return nil, ErrInvalid
		}
		for _, job := range []sandbox.Job{plan.Candidate, plan.Grader} {
			if jobs[job.ID] || keys[job.Key] || outputs[job.Outputs[0].ID] {
				return nil, ErrInvalid
			}
			jobs[job.ID], keys[job.Key], outputs[job.Outputs[0].ID] = true, true, true
			for _, input := range job.Inputs {
				inputs[input.Ref.ID] = true
			}
		}
		v.plans[plan.ManifestDigest] = plan
	}
	for key := range outputs {
		if inputs[key] {
			return nil, ErrInvalid
		}
	}
	return v, nil
}

// ValidateSandboxTestPlan checks exact protected inputs and reserved job identities.
func ValidateSandboxTestPlan(plan SandboxTestPlan, m EvaluationManifest) error {
	if m.Validate() != nil || plan.ManifestDigest != m.Digest || plan.Candidate.ID == plan.Grader.ID || plan.Candidate.Key == plan.Grader.Key {
		return ErrInvalid
	}
	for _, job := range []sandbox.Job{plan.Candidate, plan.Grader} {
		if job.Validate() != nil || job.Scope.Canonical() != m.Binding.Scope.Canonical() || job.Principal != m.Binding.Context.PrincipalRef || len(job.Env) != 0 || len(job.Outputs) != 1 {
			return ErrInvalid
		}
	}
	if len(plan.Candidate.Inputs) != 1 || len(plan.Grader.Inputs) != 3 {
		return ErrInvalid
	}
	expected := []sandbox.Input{{Path: "/inputs/candidate", Ref: m.Binding.Candidate}, {Path: "/inputs/tests", Ref: m.TestBundle}, {Path: "/inputs/fixtures", Ref: m.FixtureDataset}, {Path: "/inputs/result-schema", Ref: m.ResultSchema}}
	actual := append([]sandbox.Input{plan.Candidate.Inputs[0]}, plan.Grader.Inputs...)
	for i, input := range actual {
		got, err := input.Ref.BindingDigest()
		if err != nil {
			return ErrInvalid
		}
		want, err := expected[i].Ref.BindingDigest()
		if err != nil || input.Path != expected[i].Path || got != want {
			return ErrInvalid
		}
	}
	return nil
}

// EvaluateTests dispatches only the reserved jobs. Ambiguous dispatch returns
// ErrCallUncertain; it never allocates replacement identities or falls back to a
// host process. Reconciliation and durable phase recovery belong to the host.
func (v *SandboxTestVerifier) EvaluateTests(ctx context.Context, req TestRequest) (CheckResult, error) {
	if v == nil || v.slots == nil {
		return CheckResult{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return CheckResult{}, err
	}
	select {
	case v.slots <- struct{}{}:
		defer func() { <-v.slots }()
	default:
		return CheckResult{}, cortex.ErrVerificationUnavailable
	}
	m, err := v.config.Registry.Resolve(ctx, req, v.config.Clock.Now())
	if err != nil {
		return CheckResult{}, err
	}
	plan, ok := v.plans[m.Digest]
	if !ok {
		return CheckResult{}, ErrNotFound
	}
	deadline := time.Now().Add(5 * time.Minute)
	if req.Deadline.Before(deadline) {
		deadline = req.Deadline
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// No caller-owned request slices are passed to host ports.
	req.Binding = m.Binding
	if checkErr := v.authorizeSandbox(ctx, req, m); checkErr != nil {
		return CheckResult{}, checkErr
	}
	remaining := int64(32 << 20)
	refs := append([]evidence.Ref{m.Binding.Candidate}, m.Binding.Evidence...)
	for _, ref := range refs {
		if ref.Bytes > remaining {
			return CheckResult{}, ErrInvalid
		}
		remaining -= ref.Bytes
		if checkErr := v.readSandboxEvidence(ctx, req, m, ref); checkErr != nil {
			return CheckResult{}, checkErr
		}
	}
	candidate, err := v.runSandboxJob(ctx, req, m, plan.Candidate)
	if err != nil {
		return sandboxCheckFailure(err)
	}
	if checkErr := v.readSandboxEvidence(ctx, req, m, candidate.Artifacts[0]); checkErr != nil {
		return CheckResult{}, checkErr
	}
	grader := plan.Grader
	grader.Inputs = append(append([]sandbox.Input(nil), grader.Inputs...), sandbox.Input{Path: "/inputs/candidate-export", Ref: candidate.Artifacts[0]})
	grader, err = sandbox.Seal(grader)
	if err != nil {
		return CheckResult{}, err
	}
	graded, err := v.runSandboxJob(ctx, req, m, grader)
	if err != nil {
		return sandboxCheckFailure(err)
	}
	if checkErr := v.readSandboxEvidence(ctx, req, m, graded.Artifacts[0]); checkErr != nil {
		return CheckResult{}, checkErr
	}
	observation := SandboxObservation{Manifest: m, Candidate: plan.Candidate, Grader: grader, CandidateArtifacts: candidate.Artifacts, GraderArtifacts: graded.Artifacts}
	body, err := json.Marshal(observation)
	if err != nil || len(body) > 4<<20 {
		return CheckResult{}, ErrInvalid
	}
	var detached SandboxObservation
	if json.Unmarshal(body, &detached) != nil {
		return CheckResult{}, ErrInvalid
	}
	if checkErr := v.authorizeSandbox(ctx, req, m); checkErr != nil {
		return CheckResult{}, checkErr
	}
	assessment, err := v.config.Supervisor.Assess(ctx, detached)
	if err != nil {
		return CheckResult{}, err
	}
	if checkErr := v.authorizeSandbox(ctx, req, m); checkErr != nil {
		return CheckResult{}, checkErr
	}
	status, err := assessmentStatus(assessment, m, plan.Candidate, grader)
	if err != nil {
		return CheckResult{}, err
	}
	if len(assessment.ObservationRefs) > 16 {
		return CheckResult{}, ErrInvalid
	}
	for _, ref := range assessment.ObservationRefs {
		if readErr := v.readSandboxEvidence(ctx, req, m, ref); readErr != nil {
			return CheckResult{}, readErr
		}
	}
	return CheckResult{Status: status, ObservationRefs: assessment.ObservationRefs, EvidenceIDs: []id.ID{m.Binding.Candidate.ID, m.TestBundle.ID, m.FixtureDataset.ID, m.ResultSchema.ID}, Reason: "protected supervisor assessment"}, nil
}

func (v *SandboxTestVerifier) authorizeSandbox(ctx context.Context, req TestRequest, m EvaluationManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.ValidateRequest(req, v.config.Clock.Now()) != nil {
		return ErrInvalid
	}
	detached, err := cloneBinding(m.Binding)
	if err != nil {
		return err
	}
	if checkErr := v.config.Authorizer.AuthorizeVerification(ctx, detached); checkErr != nil {
		return checkErr
	}
	return ctx.Err()
}

func (v *SandboxTestVerifier) readSandboxEvidence(ctx context.Context, req TestRequest, m EvaluationManifest, ref evidence.Ref) (err error) {
	now := v.config.Clock.Now()
	if ref.Validate() != nil || ref.Scope.Canonical() != m.Binding.Scope.Canonical() || ref.Bytes > 16<<20 || ref.CreatedAt.After(now) || !ref.RetainUntil.After(req.Deadline) {
		return ErrInvalid
	}
	if checkErr := v.authorizeSandbox(ctx, req, m); checkErr != nil {
		return checkErr
	}
	ref.Scope.Levels = append(ref.Scope.Levels[:0:0], ref.Scope.Levels...)
	reader, err := v.config.Repository.Open(ctx, ref)
	if reader != nil {
		defer func() { err = errors.Join(err, reader.Close()) }()
	}
	if err != nil {
		return err
	}
	if reader == nil {
		return ErrInvalid
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(reader, ref.Bytes+1))
	if err != nil {
		return err
	}
	if count != ref.Bytes || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return evidence.ErrDigestMismatch
	}
	return v.authorizeSandbox(ctx, req, m)
}

func assessmentStatus(a SandboxAssessment, m EvaluationManifest, candidate, grader sandbox.Job) (Status, error) {
	if a.ManifestDigest != m.Digest || a.SupervisorRevision != m.SupervisorRevision || a.CandidateDigest != candidate.Digest || a.GraderDigest != grader.Digest || len(a.Checks) > len(m.RequiredChecks) {
		return StatusInconclusive, ErrInvalid
	}
	required := make(map[string]bool, len(m.RequiredChecks))
	for _, name := range m.RequiredChecks {
		required[name] = true
	}
	status := StatusPassed
	for _, check := range a.Checks {
		if len(check.Name) > 1024 || !required[check.Name] {
			return StatusInconclusive, ErrInvalid
		}
		delete(required, check.Name)
		switch check.Status {
		case StatusFailed:
			status = StatusFailed
		case StatusInconclusive:
			if status != StatusFailed {
				status = StatusInconclusive
			}
		case StatusPassed:
		default:
			return StatusInconclusive, ErrInvalid
		}
	}
	if len(required) > 0 && status != StatusFailed {
		status = StatusInconclusive
	}
	return status, nil
}

var errSandboxIncomplete = errors.New("cortex: sandbox execution did not complete")

func sandboxCheckFailure(err error) (CheckResult, error) {
	if errors.Is(err, errSandboxIncomplete) {
		return CheckResult{Status: StatusInconclusive, Reason: "sandbox execution did not complete"}, nil
	}
	return CheckResult{}, err
}
