//go:build linux || darwin

package grading

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/sandbox"
	"github.com/xraph/cortex/verification"
)

type gradingMemory struct {
	mu      sync.Mutex
	data    map[id.ID]evidence.Artifact
	deleted map[id.ID]bool
}

func (m *gradingMemory) Put(_ context.Context, a evidence.Artifact) (evidence.Ref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleted[a.Ref.ID] {
		return evidence.Ref{}, evidence.ErrDeleted
	}
	if prior, ok := m.data[a.Ref.ID]; ok && !bytes.Equal(prior.Data, a.Data) {
		return evidence.Ref{}, verification.ErrConflict
	}
	m.data[a.Ref.ID] = a
	return a.Ref, nil
}
func (m *gradingMemory) Open(_ context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleted[ref.ID] {
		return nil, evidence.ErrDeleted
	}
	a, ok := m.data[ref.ID]
	if !ok {
		return nil, evidence.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), a.Data...))), nil
}
func (m *gradingMemory) Delete(_ context.Context, ref evidence.Ref, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted[ref.ID] = true
	delete(m.data, ref.ID)
	return nil
}

type gradingClock struct{ now time.Time }

func (c gradingClock) Now() time.Time { return c.now }

type gradingAuthority struct{ denied atomic.Bool }

func (a *gradingAuthority) AuthorizeVerification(context.Context, verification.Binding) error {
	if a.denied.Load() {
		return verification.ErrResultAccessDenied
	}
	return nil
}

type gradingRunner struct {
	mu          sync.Mutex
	repo        evidence.Repository
	now         time.Time
	key         ed25519.PrivateKey
	jobs        map[id.ID]sandbox.Job
	submissions int
	lost        bool
	block       bool
	entered     chan struct{}
}

func gradingLimits() sandbox.Limits {
	return sandbox.Limits{CPUMillis: 1000, MemoryBytes: 64 << 20, Processes: 16, ScratchBytes: 1 << 20, WallTime: time.Minute, OutputBytes: 4096, ArtifactBytes: 4096}
}
func (r *gradingRunner) Capabilities(context.Context) (sandbox.Capabilities, error) {
	return sandbox.Capabilities{SchemaVersion: 1, Runtime: "runsc", RuntimeDigest: evidence.Digest([]byte("fixture-runtime")), Profile: sandbox.ProfileDenyAll, Qualification: "fixture-only-not-live", QualifiedUntil: r.now.Add(time.Hour), Limits: gradingLimits()}, nil
}
func (r *gradingRunner) receipt(ctx context.Context, j sandbox.Job) (sandbox.Receipt, error) {
	caps, _ := r.Capabilities(ctx)
	return signGradingReceipt(sandbox.Receipt{SchemaVersion: 1, Job: j, Capabilities: caps, Broker: "test-broker", AcceptedAt: r.now}, r.key)
}
func (r *gradingRunner) Submit(ctx context.Context, j sandbox.Job) (sandbox.Receipt, error) {
	r.mu.Lock()
	r.submissions++
	r.jobs[j.ID] = j
	lost := r.lost
	r.lost = false
	r.mu.Unlock()
	if lost {
		return sandbox.Receipt{}, verification.ErrCallUncertain
	}
	return r.receipt(ctx, j)
}
func (r *gradingRunner) Inspect(ctx context.Context, key id.ID) (sandbox.JobStatus, error) {
	r.mu.Lock()
	j, ok := r.jobs[key]
	block := r.block
	entered := r.entered
	r.mu.Unlock()
	if !ok {
		return sandbox.JobStatus{}, verification.ErrNotFound
	}
	if block {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return sandbox.JobStatus{}, ctx.Err()
	}
	receipt, err := r.receipt(ctx, j)
	return sandbox.JobStatus{Receipt: receipt, State: sandbox.StateExited, Revision: 1, UpdatedAt: r.now}, err
}
func (r *gradingRunner) Cancel(context.Context, id.ID) error { return nil }
func (r *gradingRunner) Collect(ctx context.Context, key id.ID) (sandbox.Result, error) {
	r.mu.Lock()
	j, ok := r.jobs[key]
	r.mu.Unlock()
	if !ok {
		return sandbox.Result{}, verification.ErrNotFound
	}
	data := []byte(`{"protocol":"cortex.json-equality.v1","values":{"answer":42}}`)
	if len(j.Inputs) == 4 {
		data = []byte(`{"protocol":"cortex.json-equality.v1","checks":{"answer":"passed"}}`)
	}
	out := j.Outputs[0]
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: out.ID, Scope: j.Scope, MediaType: out.MediaType, Version: j.Digest, Producer: sandbox.ArtifactProducer, SourceURI: j.OutputSource(out), Sensitivity: evidence.SensitivityRestricted, CreatedAt: r.now, RetainUntil: r.now.Add(time.Hour)}, Data: data})
	if err != nil {
		return sandbox.Result{}, err
	}
	if _, err = r.repo.Put(ctx, artifact); err != nil {
		return sandbox.Result{}, err
	}
	return signGradingResult(sandbox.Result{JobDigest: j.Digest, State: sandbox.StateExited, Artifacts: []evidence.Ref{artifact.Ref}}, r.key)
}
func (r *gradingRunner) count() int { r.mu.Lock(); defer r.mu.Unlock(); return r.submissions }

func gradingFixture(t *testing.T) (EvaluationConfig, verification.EvaluationDispatch, *gradingRunner, *gradingAuthority, *gradingMemory) {
	t.Helper()
	now := time.Now().UTC()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "grading"}}}
	c, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "exact JSON result", Criteria: []verification.Criterion{{ID: "test", Kind: verification.KindTest, Required: true, Verifier: "equality", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	repo := &gradingMemory{data: map[id.ID]evidence.Artifact{}, deleted: map[id.ID]bool{}}
	makeArtifact := func(body string) evidence.Ref {
		a, sealErr := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "application/json", Producer: "host", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now.Add(-time.Minute), RetainUntil: now.Add(2 * time.Hour)}, Data: []byte(body)})
		require.NoError(t, sealErr)
		_, putErr := repo.Put(t.Context(), a)
		require.NoError(t, putErr)
		return a.Ref
	}
	candidate := makeArtifact(`{"untrusted":"candidate"}`)
	tests := makeArtifact(`{"protocol":"cortex.json-equality.v1","checks":["answer"]}`)
	schema := makeArtifact(`{"protocol":"cortex.json-equality.v1","checks":["answer"]}`)
	fixtures := makeArtifact(`{"protocol":"cortex.json-equality.v1","values":{"answer":42}}`)
	binding := verification.Binding{Scope: scope, RunID: id.NewAgentRunID(), ContractDigest: c.Digest, Candidate: candidate, Evidence: []evidence.Ref{tests, fixtures, schema}, Context: verification.ContextSnapshot{PrincipalRef: "operator", AuthorityRevision: "1", PreparedDigest: evidence.Digest([]byte("prepared")), PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}
	m, err := verification.SealEvaluationManifest(verification.EvaluationManifest{SchemaVersion: 1, Revision: "1", Binding: binding, Criterion: c.Criteria[0], TestBundle: tests, FixtureDataset: fixtures, ResultSchema: schema, SupervisorRevision: EqualitySupervisorRevision, RequiredChecks: []string{"answer"}})
	require.NoError(t, err)
	job := func(key string, inputs []sandbox.Input) sandbox.Job {
		j, sealErr := sandbox.Seal(sandbox.Job{SchemaVersion: 1, ID: id.New(sandbox.PrefixJob), Key: key, Scope: scope, Principal: "operator", Image: "sha256:" + evidence.Digest([]byte(key)), Argv: []string{"/bin/equality"}, Inputs: inputs, Outputs: []sandbox.Output{{ID: id.New(evidence.PrefixArtifact), Path: "/tmp/outputs/result", MediaType: "application/json", MaxBytes: 4096}}, Network: sandbox.NetworkDenied, Limits: gradingLimits()})
		require.NoError(t, sealErr)
		return j
	}
	plan := verification.SandboxTestPlan{ManifestDigest: m.Digest, Candidate: job("candidate", []sandbox.Input{{Path: "/inputs/candidate", Ref: candidate}}), Grader: job("grader", []sandbox.Input{{Path: "/inputs/tests", Ref: tests}, {Path: "/inputs/fixtures", Ref: fixtures}, {Path: "/inputs/result-schema", Ref: schema}})}
	pub, key, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	runner := &gradingRunner{repo: repo, now: now, key: key, jobs: map[id.ID]sandbox.Job{}, entered: make(chan struct{}, 1)}
	authority := &gradingAuthority{}
	auth, err := NewAuthenticity("test-broker", pub)
	require.NoError(t, err)
	supervisor, err := NewEqualitySupervisor(SupervisorConfig{Registrations: []SupervisorRegistration{{Manifest: m, AssessmentID: id.New(evidence.PrefixArtifact)}}, Repository: repo, Authorizer: authority, Clock: gradingClock{now}})
	require.NoError(t, err)
	registry, err := verification.NewEvaluationRegistry([]verification.EvaluationManifest{m})
	require.NoError(t, err)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o700))
	cfg := EvaluationConfig{Tests: verification.SandboxTestConfig{Registry: registry, Plans: []verification.SandboxTestPlan{plan}, Runner: runner, Repository: repo, Authenticity: auth, Supervisor: supervisor, Authorizer: authority, Clock: gradingClock{now}, MaxConcurrent: 1, PollInterval: 10 * time.Millisecond}, Manifests: []verification.EvaluationManifest{m}, Allowance: verification.EvaluationAllowance{MaxUSDMicros: 100, PricingRevision: "host-price-1"}, JournalDirectory: dir, Owner: uint32(os.Getuid()), Interval: 10 * time.Millisecond}
	dispatch := verification.EvaluationDispatch{Input: verification.EvaluationInput{JobID: id.New(verification.PrefixJob), Request: verification.Request{Contract: c, Binding: binding, Deadline: now.Add(time.Minute), Verifier: "equality", VerifierRevision: "1"}, Budget: verification.EvaluationBudget{MaxCalls: 1, MaxUSDMicros: 100, PricingRevision: "host-price-1"}}, Call: verification.EvaluationCall{Request: verification.CallRequest{ID: id.New(verification.PrefixCall), Verifier: "equality", VerifierRevision: "1", MaxUSDMicros: 100}, Generation: 1, CriterionIDs: []string{"test"}, StartedAt: now}}
	return cfg, dispatch, runner, authority, repo
}
func waitGradingResult(t *testing.T, a *EvaluationAdapter, d verification.EvaluationDispatch) verification.CallResult {
	t.Helper()
	ctx := cortex.WithScope(t.Context(), d.Input.Request.Binding.Scope)
	var result verification.CallResult
	require.Eventually(t, func() bool { var err error; result, err = a.Reconcile(ctx, d); return err == nil }, 3*time.Second, 10*time.Millisecond)
	return result
}
func TestEvaluationRetainsExactOutcomeAndReconcileNeverDispatches(t *testing.T) {
	cfg, d, runner, authority, repo := gradingFixture(t)
	a, err := NewEvaluationAdapter(cfg)
	require.NoError(t, err)
	ctx := cortex.WithScope(t.Context(), d.Input.Request.Binding.Scope)
	_, err = a.Reconcile(ctx, d)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	require.Zero(t, runner.count())
	require.NoError(t, a.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, a.Stop(context.Background())) })
	_, err = a.Evaluate(ctx, d)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	result := waitGradingResult(t, a, d)
	require.Equal(t, verification.StatusPassed, result.Report.Outcomes[0].Status)
	require.Len(t, result.Report.Outcomes[0].ObservationRefs, 3)
	require.False(t, result.Usage.MoneyKnown)
	require.Equal(t, 2, runner.count())
	require.NoError(t, a.Stop(t.Context()))
	restored, err := NewEvaluationAdapter(cfg)
	require.NoError(t, err)
	repeated, err := restored.Reconcile(ctx, d)
	require.NoError(t, err)
	require.Equal(t, result.Report.Digest, repeated.Report.Digest)
	require.Equal(t, 2, runner.count())
	authority.denied.Store(true)
	_, err = restored.Reconcile(ctx, d)
	require.Error(t, err)
	authority.denied.Store(false)
	require.NoError(t, repo.Delete(ctx, result.Report.Outcomes[0].ObservationRefs[2], "revoked"))
	_, err = restored.Reconcile(ctx, d)
	require.ErrorIs(t, err, evidence.ErrDeleted)
	require.Equal(t, 2, runner.count())
}
func TestEvaluationLostSubmitAndRestartKeepOriginalJobs(t *testing.T) {
	cfg, d, runner, _, _ := gradingFixture(t)
	runner.lost = true
	a, err := NewEvaluationAdapter(cfg)
	require.NoError(t, err)
	require.NoError(t, a.Start(t.Context()))
	ctx := cortex.WithScope(t.Context(), d.Input.Request.Binding.Scope)
	_, err = a.Evaluate(ctx, d)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	result := waitGradingResult(t, a, d)
	require.NotNil(t, result.Report)
	require.Equal(t, 2, runner.count())
	require.NoError(t, a.Stop(t.Context()))
}
func TestEvaluationStopDrainsAndRestartAdvancesUnsubmittedGrader(t *testing.T) {
	cfg, d, runner, _, _ := gradingFixture(t)
	runner.block = true
	a, err := NewEvaluationAdapter(cfg)
	require.NoError(t, err)
	require.NoError(t, a.Start(t.Context()))
	ctx := cortex.WithScope(t.Context(), d.Input.Request.Binding.Scope)
	_, err = a.Evaluate(ctx, d)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	select {
	case <-runner.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("candidate did not begin")
	}
	duplicate, err := NewEvaluationAdapter(cfg)
	require.NoError(t, err)
	require.ErrorIs(t, duplicate.Start(t.Context()), verification.ErrConflict)
	require.NoError(t, a.Stop(t.Context()))
	require.Equal(t, 1, runner.count())
	runner.mu.Lock()
	runner.block = false
	runner.mu.Unlock()
	require.NoError(t, duplicate.Start(t.Context()))
	defer duplicate.Stop(context.Background())
	result := waitGradingResult(t, duplicate, d)
	require.NotNil(t, result.Report)
	require.Equal(t, 2, runner.count())
}
func TestEvaluationConsumedIntentWithoutBrokerReceiptNeverResubmits(t *testing.T) {
	cfg, d, runner, _, _ := gradingFixture(t)
	a, err := NewEvaluationAdapter(cfg)
	require.NoError(t, err)
	ctx := cortex.WithScope(t.Context(), d.Input.Request.Binding.Scope)
	entry, admission, err := a.admission(ctx, d)
	require.NoError(t, err)
	require.NoError(t, a.journal.write(ctx, d.Call.Request.ID.String()+".call", evaluationCallIdentity{entry.manifest.Digest, admission.Digest}))
	require.NoError(t, a.journal.write(ctx, entry.manifest.Digest+".admission", admission))
	job := cfg.Tests.Plans[0].Candidate
	require.NoError(t, a.journal.write(ctx, job.ID.String()+".intent", job))
	require.NoError(t, a.Start(t.Context()))
	defer a.Stop(context.Background())
	require.Eventually(t, func() bool { return a.Failures() > 1 }, time.Second, 10*time.Millisecond)
	_, err = a.Reconcile(ctx, d)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	require.Zero(t, runner.count())
}
func TestStrictJSONRejectsAmbiguousAndUnknownFields(t *testing.T) {
	for _, body := range []string{`{"protocol":"a","protocol":"b","checks":[]}`, `{"protocol":"a","checks":[],"unknown":true}`, `{"protocol":"a","checks":[]} {}`} {
		var value equalityDeclaration
		require.Error(t, strictJSON([]byte(body), &value))
	}
}
func TestEvaluationChangedCallAndPriceFailClosed(t *testing.T) {
	cfg, d, _, _, _ := gradingFixture(t)
	a, err := NewEvaluationAdapter(cfg)
	require.NoError(t, err)
	ctx := cortex.WithScope(t.Context(), d.Input.Request.Binding.Scope)
	changed := d
	changed.Call.Request.MaxUSDMicros--
	_, err = a.Evaluate(ctx, changed)
	require.Error(t, err)
	changed = d
	changed.Input.Budget.PricingRevision = "other"
	_, err = a.Estimate(ctx, changed.Input)
	require.Error(t, err)
}

func signGradingReceipt(r sandbox.Receipt, key ed25519.PrivateKey) (sandbox.Receipt, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return sandbox.Receipt{}, err
	}
	r.Signature = ed25519.Sign(key, body)
	return r, nil
}
func signGradingResult(r sandbox.Result, key ed25519.PrivateKey) (sandbox.Result, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return sandbox.Result{}, err
	}
	r.Signature = ed25519.Sign(key, body)
	return r, nil
}

type gradingReadHook struct {
	evidence.Repository
	after func(evidence.Ref)
}

func (r gradingReadHook) Open(ctx context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	reader, err := r.Repository.Open(ctx, ref)
	if err == nil {
		r.after(ref)
	}
	return reader, err
}

func TestEvaluationReconcileRechecksAuthorityAndExpiryAfterHydration(t *testing.T) {
	for _, expiry := range []bool{false, true} {
		t.Run(map[bool]string{false: "revocation", true: "expiry"}[expiry], func(t *testing.T) {
			cfg, d, runner, authority, repo := gradingFixture(t)
			a, err := NewEvaluationAdapter(cfg)
			require.NoError(t, err)
			require.NoError(t, a.Start(t.Context()))
			ctx := cortex.WithScope(t.Context(), d.Input.Request.Binding.Scope)
			_, err = a.Evaluate(ctx, d)
			require.ErrorIs(t, err, verification.ErrCallUncertain)
			result := waitGradingResult(t, a, d)
			require.NoError(t, a.Stop(t.Context()))
			finalRef := d.Input.Request.Binding.Evidence[len(d.Input.Request.Binding.Evidence)-1]
			a.config.Tests.Repository = gradingReadHook{Repository: repo, after: func(ref evidence.Ref) {
				if ref.ID == finalRef.ID {
					if expiry {
						a.config.Tests.Clock = gradingClock{now: result.Report.ExpiresAt}
					} else {
						authority.denied.Store(true)
					}
				}
			}}
			result, err = a.Reconcile(ctx, d)
			require.Error(t, err)
			require.Nil(t, result.Report)
			require.Equal(t, 2, runner.count())
		})
	}
}
