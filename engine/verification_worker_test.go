package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

type workerClock struct{}

func (workerClock) Now() time.Time { return time.Now().UTC() }

type workerAuthorizer func(context.Context, verification.Binding) error

func (f workerAuthorizer) AuthorizeVerification(ctx context.Context, b verification.Binding) error {
	return f(ctx, b)
}

type workerFence func(context.Context, verification.Binding, func(verification.Binding) error) error

func (f workerFence) WithCommitFence(ctx context.Context, b verification.Binding, commit func(verification.Binding) error) error {
	return f(ctx, b, commit)
}

type workerAdapter struct {
	estimates, evaluations, reconciliations int
	status                                  verification.Status
	retained                                verification.CallResult
	afterEvaluate                           func()
	lost                                    bool
}

func (a *workerAdapter) Estimate(context.Context, verification.EvaluationInput) (verification.EvaluationAllowance, error) {
	a.estimates++
	return verification.EvaluationAllowance{MaxTokens: 10}, nil
}
func (a *workerAdapter) Evaluate(_ context.Context, d verification.EvaluationDispatch) (verification.CallResult, error) {
	a.evaluations++
	now := time.Now().UTC()
	outcomes := make([]verification.Outcome, 0, len(d.Call.CriterionIDs))
	for _, criterion := range d.Call.CriterionIDs {
		outcomes = append(outcomes, verification.Outcome{CriterionID: criterion, Verifier: d.Call.Request.Verifier, VerifierRevision: d.Call.Request.VerifierRevision, Status: a.status})
	}
	report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: d.Input.Request.Binding, CheckedAt: now, ExpiresAt: now.Add(time.Minute), Outcomes: outcomes})
	if err != nil {
		return verification.CallResult{}, err
	}
	a.retained = verification.CallResult{CallID: d.Call.Request.ID, Report: &report, Usage: verification.EvaluationUsage{Tokens: 4, TokensKnown: true, MoneyKnown: true, Receipt: "provider-receipt"}}
	if a.afterEvaluate != nil {
		a.afterEvaluate()
	}
	if a.lost {
		return verification.CallResult{}, errors.New("lost provider reply")
	}
	return a.retained, nil
}
func (a *workerAdapter) Reconcile(_ context.Context, d verification.EvaluationDispatch) (verification.CallResult, error) {
	a.reconciliations++
	if a.retained.CallID != d.Call.Request.ID {
		return verification.CallResult{}, verification.ErrCallUncertain
	}
	return a.retained, nil
}

type workerLLM struct {
	recordingLLM
	calls, tools int
}

func (c *workerLLM) Complete(ctx context.Context, r *llm.Request) (*llm.Response, error) {
	c.calls++
	if c.calls == 1 {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe-1", Name: "probe", Arguments: `{}`}}, FinishReason: "tool_calls"}, nil
	}
	return c.recordingLLM.Complete(ctx, r)
}

type workerFixture struct {
	ctx     context.Context
	storage *sqlite.Store
	path    string
	request engine.VerifyRunRequest
	config  engine.VerificationWorkerConfig
	adapter *workerAdapter
	llm     *workerLLM
}

func newWorkerFixture(t *testing.T, criteria ...verification.Criterion) workerFixture {
	t.Helper()
	ctx := overlayCtx("verification-worker")
	path := filepath.Join(t.TempDir(), "worker.db")
	s := openSQLiteStoreForTest(t, path)
	scope, err := cortex.RequireScope(ctx)
	require.NoError(t, err)
	if len(criteria) == 0 {
		criteria = []verification.Criterion{{ID: "test", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute}}
	}
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "check saved candidate", Criteria: criteria})
	require.NoError(t, err)
	require.NoError(t, s.CreateContract(ctx, contract))
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", Tools: []string{"probe"}}
	require.NoError(t, s.Create(ctx, ag))
	client := &workerLLM{}
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(client), engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { client.tools++; return "observed", nil }))
	require.NoError(t, err)
	prepared, err := e.PrepareAgent(ctx, ag.ID, nil)
	require.NoError(t, err)
	r, err := e.RunPrepared(ctx, prepared, "work", engine.PreparedRunOptions{AcceptanceRef: contract.ID.String()})
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, r.State)
	now := time.Now().UTC()
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: scope, Version: "1", MediaType: "text/plain", Producer: "host", Sensitivity: evidence.SensitivityInternal, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte(r.Output)})
	require.NoError(t, err)
	require.NoError(t, s.SaveEvidence(ctx, artifact))
	binding := verification.Binding{Scope: scope, RunID: r.ID, Candidate: artifact.Ref, ContractDigest: contract.Digest, Context: verification.ContextSnapshot{PrincipalRef: "operator", AuthorityRevision: "1", PreparedDigest: r.Provenance.PreparedDigest, PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1"}}
	jobID := id.New(verification.PrefixJob)
	req := engine.VerifyRunRequest{Job: verification.JobRequest{SchemaVersion: 1, ID: jobID, ExpectedRunRevision: r.Revision, ContractID: contract.ID, Binding: binding, Deadline: now.Add(5 * time.Minute), MaxAttempts: 3, EvaluationBudget: &verification.EvaluationBudget{MaxCalls: 3, MaxTokens: 30, TokenOnly: true}}, Claim: verification.JobClaim{JobID: jobID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker-1", Duration: time.Minute}, ResultID: id.New(verification.PrefixResult)}
	adapter := &workerAdapter{status: verification.StatusPassed}
	config := engine.VerificationWorkerConfig{Adapter: adapter, Clock: workerClock{}, Authorizer: workerAuthorizer(func(context.Context, verification.Binding) error { return nil }), Fence: workerFence(func(_ context.Context, b verification.Binding, commit func(verification.Binding) error) error {
		return commit(b)
	})}
	return workerFixture{ctx: ctx, storage: s, path: path, request: req, config: config, adapter: adapter, llm: client}
}
func (f workerFixture) engine(t *testing.T, s store.Store) *engine.Engine {
	t.Helper()
	e, err := engine.New(engine.WithVerificationWorker(f.config), engine.WithStore(s), engine.WithLLM(f.llm))
	require.NoError(t, err)
	return e
}

func (f *workerFixture) prepareIntent(t *testing.T) (verification.CaptureIntent, *run.Run, string) {
	t.Helper()
	source, err := f.storage.GetRun(f.ctx, f.request.Job.Binding.RunID)
	require.NoError(t, err)
	// A fresh run reserves its policy before the fixture supplies its output.
	current := &run.Run{Entity: cortex.NewEntity(), ID: id.NewAgentRunID(), AgentID: source.AgentID, SessionID: source.SessionID, Scope: source.Scope, State: run.StateRunning, Provenance: source.Provenance}
	require.NoError(t, f.storage.CreateRun(f.ctx, current))
	template, _, err := f.request.Job.Freeze()
	require.NoError(t, err)
	template.ExpectedRunRevision = current.Revision
	template.Binding.RunID = current.ID
	template.Binding.Candidate.ID = id.New(evidence.PrefixArtifact)
	template.Binding.Candidate.SHA256 = evidence.Digest(nil)
	template.Binding.Candidate.Bytes = 0
	intent, err := verification.SealCaptureIntent(verification.CaptureIntent{SchemaVersion: 1, ID: id.New(verification.PrefixCaptureIntent), Template: template, ResultID: f.request.ResultID, MaxCandidateBytes: 1 << 20})
	require.NoError(t, err)
	require.NoError(t, f.storage.CreateCaptureIntent(f.ctx, intent))
	return intent, current, source.Output
}

func (f *workerFixture) registerIntent(t *testing.T) verification.CaptureIntent {
	t.Helper()
	intent, current, output := f.prepareIntent(t)
	current.State, current.StopReason, current.Output = run.StateCompleted, run.StopFinalAnswer, output
	require.NoError(t, f.storage.UpdateRun(f.ctx, current))
	var err error
	f.request.Job, err = intent.JobFor(current.Revision, current.Output)
	require.NoError(t, err)
	require.NoError(t, f.storage.SaveEvidence(f.ctx, evidence.Artifact{Ref: f.request.Job.Binding.Candidate, Data: []byte(output)}))
	return intent
}

func TestVerificationWorkerChecksIntentBeforeDispatch(t *testing.T) {
	for _, state := range []string{"absent", "queued", "running", "committed"} {
		t.Run(state, func(t *testing.T) {
			f := newWorkerFixture(t)
			intent := f.registerIntent(t)
			e := f.engine(t, f.storage)
			var before verification.Job
			var err error
			switch state {
			case "queued", "running":
				before, err = f.storage.EnqueueVerificationJob(f.ctx, f.request.Job)
				require.NoError(t, err)
				if state == "running" {
					before, err = f.storage.ClaimVerificationJob(f.ctx, f.request.Claim)
					require.NoError(t, err)
				}
			case "committed":
				result, verifyErr := e.VerifyRun(f.ctx, f.request)
				require.NoError(t, verifyErr)
				before = result.Job
			}
			estimates, evaluations := f.adapter.estimates, f.adapter.evaluations
			wrong := f.request
			wrong.ResultID = id.New(verification.PrefixResult)
			_, err = e.VerifyRun(f.ctx, wrong)
			require.ErrorIs(t, err, verification.ErrConflict)
			require.Equal(t, estimates, f.adapter.estimates, "identity must be checked before estimation")
			require.Equal(t, evaluations, f.adapter.evaluations, "identity must be checked before paid evaluation")
			after, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
			if state == "absent" {
				require.ErrorIs(t, err, verification.ErrJobNotFound)
			} else {
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
			result, err := e.VerifyRun(f.ctx, f.request)
			require.NoError(t, err)
			require.Equal(t, intent.ResultID, result.Receipt.Request.ResultID)
			require.Equal(t, 1, f.adapter.evaluations)
		})
	}
}

type workerIntentStore struct {
	*sqlite.Store
	read func(context.Context, id.AgentRunID) (verification.CaptureIntent, error)
}

func (s workerIntentStore) GetRunCaptureIntent(ctx context.Context, runID id.AgentRunID) (verification.CaptureIntent, error) {
	return s.read(ctx, runID)
}

func TestVerificationWorkerRejectsUnreadableIntent(t *testing.T) {
	for _, mode := range []string{"unavailable", "tampered", "wrong scope", "wrong run", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			intent := f.registerIntent(t)
			failure := errors.New("intent storage unavailable")
			expected := verification.ErrInvalid
			if mode == "unavailable" || mode == "unauthorized" {
				expected = failure
			}
			if mode == "unauthorized" {
				f.config.Authorizer = workerAuthorizer(func(context.Context, verification.Binding) error { return failure })
			}
			reads := 0
			s := workerIntentStore{Store: f.storage, read: func(context.Context, id.AgentRunID) (verification.CaptureIntent, error) {
				reads++
				switch mode {
				case "unavailable":
					return verification.CaptureIntent{}, failure
				case "tampered":
					intent.Digest = evidence.Digest([]byte("tampered"))
					return intent, nil
				case "wrong scope":
					intent.Template.Binding.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}}
					intent.Template.Binding.Candidate.Scope = intent.Template.Binding.Scope
				case "wrong run":
					intent.Template.Binding.RunID = id.NewAgentRunID()
				}
				sealed, err := verification.SealCaptureIntent(intent)
				require.NoError(t, err)
				return sealed, nil
			}}
			_, err := f.engine(t, s).VerifyRun(f.ctx, f.request)
			require.ErrorIs(t, err, expected)
			if mode == "unauthorized" {
				require.Zero(t, reads)
			} else {
				require.Equal(t, 1, reads)
			}
			require.Zero(t, f.adapter.estimates)
			require.Zero(t, f.adapter.evaluations)
			_, err = f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
			require.ErrorIs(t, err, verification.ErrJobNotFound)
		})
	}
}

type workerRequiredStore interface {
	store.Store
	verification.JobStore
	verification.ExecutorStore
	verification.CommitStore
}

func TestVerificationWorkerWithoutIntentCapability(t *testing.T) {
	f := newWorkerFixture(t)
	s := struct{ workerRequiredStore }{f.storage}
	_, hasIntents := any(s).(verification.CaptureIntentStore)
	require.False(t, hasIntents)
	result, err := f.engine(t, s).VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	require.Equal(t, verification.JobCommitted, result.Job.State)
}

func TestVerificationWorkerCommitsSavedRun(t *testing.T) {
	f := newWorkerFixture(t)
	e := f.engine(t, f.storage)
	result, err := e.VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	require.Equal(t, verification.JobCommitted, result.Job.State)
	require.NotNil(t, result.Receipt)
	require.NoError(t, result.Receipt.Validate())
	retry, err := e.VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	require.Equal(t, result, retry)
	require.Equal(t, 2, f.llm.calls)
	require.Equal(t, 1, f.llm.tools)
	require.Equal(t, 1, f.adapter.evaluations)
	require.Equal(t, 1, f.adapter.estimates)
	saved, err := f.storage.GetRun(f.ctx, f.request.Job.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, saved.VerificationStatus)
	require.Equal(t, f.request.ResultID.String(), saved.ResultRef)
}

func TestVerificationWorkerRecoversWithoutGeneration(t *testing.T) {
	for _, mode := range []string{"provider", "commit"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			lost := errors.New("lost commit reply")
			if mode == "provider" {
				f.adapter.lost = true
			} else {
				f.config.Fence = workerFence(func(_ context.Context, b verification.Binding, commit func(verification.Binding) error) error {
					if err := commit(b); err != nil {
						return err
					}
					return lost
				})
			}
			_, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
			require.Error(t, err)
			require.NoError(t, f.storage.Close())
			reopened := openSQLiteStoreForTest(t, f.path)
			f.config.Fence = workerFence(func(_ context.Context, b verification.Binding, commit func(verification.Binding) error) error {
				return commit(b)
			})
			result, err := f.engine(t, reopened).VerifyRun(f.ctx, f.request)
			require.NoError(t, err)
			require.Equal(t, verification.JobCommitted, result.Job.State)
			require.NotNil(t, result.Receipt)
			require.Equal(t, 2, f.llm.calls)
			require.Equal(t, 1, f.llm.tools)
			require.Equal(t, 1, f.adapter.evaluations)
			require.Equal(t, 1, f.adapter.estimates)
			if mode == "provider" {
				require.Equal(t, 1, f.adapter.reconciliations)
			}
		})
	}
}

func TestVerificationWorkerFailedCheckDoesNotCommit(t *testing.T) {
	f := newWorkerFixture(t)
	f.adapter.status = verification.StatusFailed
	e := f.engine(t, f.storage)
	result, err := e.VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	require.Equal(t, verification.JobFailed, result.Job.State)
	require.Nil(t, result.Receipt)
	retry, err := e.VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	require.Equal(t, result, retry)
	require.Equal(t, 1, f.adapter.evaluations)
	_, err = f.storage.GetCommittedResult(f.ctx, f.request.ResultID)
	require.ErrorIs(t, err, verification.ErrNotFound)
}

func TestVerificationWorkerRequiresExplicitServices(t *testing.T) {
	f := newWorkerFixture(t)
	unconfigured, err := engine.New()
	require.NoError(t, err)
	_, err = unconfigured.VerifyRun(f.ctx, f.request)
	require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	for _, s := range []store.Store{nil, struct{ store.Store }{Store: f.storage}} {
		_, err = engine.New(engine.WithStore(s), engine.WithVerificationWorker(f.config))
		require.ErrorIs(t, err, cortex.ErrVerificationUnavailable)
	}
	for _, mutate := range []func(*engine.VerificationWorkerConfig){
		func(c *engine.VerificationWorkerConfig) { c.Adapter = nil },
		func(c *engine.VerificationWorkerConfig) { c.Adapter = (*workerAdapter)(nil) },
		func(c *engine.VerificationWorkerConfig) { c.Clock = nil },
		func(c *engine.VerificationWorkerConfig) { c.Clock = (*workerClock)(nil) },
		func(c *engine.VerificationWorkerConfig) { c.Authorizer = nil },
		func(c *engine.VerificationWorkerConfig) { c.Authorizer = workerAuthorizer(nil) },
		func(c *engine.VerificationWorkerConfig) { c.Fence = nil },
		func(c *engine.VerificationWorkerConfig) { c.Fence = workerFence(nil) },
	} {
		config := f.config
		mutate(&config)
		_, err = engine.New(engine.WithStore(f.storage), engine.WithVerificationWorker(config))
		require.ErrorIs(t, err, verification.ErrInvalid)
	}
	var nilStore *sqlite.Store
	_, err = engine.New(engine.WithStore(nilStore), engine.WithVerificationWorker(f.config))
	require.ErrorIs(t, err, verification.ErrInvalid)
	configured, err := engine.New(engine.WithStore(f.storage), engine.WithVerificationWorker(f.config))
	require.NoError(t, err)
	require.False(t, configured.RuntimeCapabilities().ContextualVerification, "explicit work does not install a publication gate")
}

func TestVerificationWorkerRejectsBeforeEnqueue(t *testing.T) {
	for _, mode := range []string{"scope", "no scope", "cancelled", "revoked", "result", "claim job", "claim id", "owner", "lease", "budget", "incomplete", "candidate", "expired"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			ctx := f.ctx
			switch mode {
			case "scope":
				ctx = overlayCtx("another-workspace")
			case "no scope":
				ctx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "revoked":
				f.config.Authorizer = workerAuthorizer(func(context.Context, verification.Binding) error { return errors.New("revoked") })
			case "result":
				f.request.ResultID = id.New(verification.PrefixJob)
			case "claim job":
				f.request.Claim.JobID = id.New(verification.PrefixJob)
			case "claim id":
				f.request.Claim.ClaimID = id.ID{}
			case "owner":
				f.request.Claim.Owner = " "
			case "lease":
				f.request.Claim.Duration = verification.MaxJobLease + time.Second
			case "budget":
				f.request.Job.EvaluationBudget = nil
			case "expired":
				f.request.Job.Deadline = time.Now().Add(-time.Second)
			case "incomplete", "candidate":
				r, err := f.storage.GetRun(ctx, f.request.Job.Binding.RunID)
				require.NoError(t, err)
				if mode == "incomplete" {
					r.State = run.StateIncomplete
				} else {
					r.Output = "changed"
				}
				require.NoError(t, f.storage.UpdateRun(ctx, r))
				f.request.Job.ExpectedRunRevision = r.Revision
			}
			_, err := f.engine(t, f.storage).VerifyRun(ctx, f.request)
			require.Error(t, err)
			_, err = f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
			require.ErrorIs(t, err, verification.ErrJobNotFound)
			require.Zero(t, f.adapter.evaluations)
			require.Zero(t, f.adapter.estimates)
		})
	}
}

func TestVerificationWorkerRechecksCommitAuthority(t *testing.T) {
	for _, mode := range []string{"before fence", "inside fence", "changed binding", "deleted evidence", "after commit", "historical"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			denied := errors.New("authority revoked")
			revoked := false
			f.config.Authorizer = workerAuthorizer(func(context.Context, verification.Binding) error {
				if revoked {
					return denied
				}
				return nil
			})
			if mode == "before fence" {
				f.adapter.afterEvaluate = func() { revoked = true }
			}
			f.config.Fence = workerFence(func(ctx context.Context, b verification.Binding, commit func(verification.Binding) error) error {
				switch mode {
				case "inside fence":
					revoked = true
				case "changed binding":
					b.Context.AuthorityRevision = "2"
				case "deleted evidence":
					require.NoError(t, f.storage.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), b.Candidate, "deleted during verification"))
				}
				err := commit(b)
				if mode == "after commit" {
					revoked = true
				}
				return err
			})
			e := f.engine(t, f.storage)
			result, err := e.VerifyRun(f.ctx, f.request)
			if mode == "historical" {
				require.NoError(t, err)
				require.NotNil(t, result.Receipt)
				revoked = true
				result, err = e.VerifyRun(f.ctx, f.request)
			}
			require.Error(t, err)
			require.Nil(t, result.Receipt)
			if mode != "after commit" && mode != "historical" {
				_, err = f.storage.GetCommittedResult(f.ctx, f.request.ResultID)
				require.ErrorIs(t, err, verification.ErrNotFound)
			}
		})
	}
}

func TestVerificationWorkerDetachesAuthorizationInputs(t *testing.T) {
	f := newWorkerFixture(t)
	f.config.Authorizer = workerAuthorizer(func(_ context.Context, b verification.Binding) error { b.Scope.Levels[0].Value = "mutated"; return nil })
	result, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	require.Equal(t, verification.JobCommitted, result.Job.State)
	scope, err := cortex.RequireScope(f.ctx)
	require.NoError(t, err)
	require.Equal(t, scope.Canonical(), result.Job.Request.Binding.Scope.Canonical())
}

type workerStore struct {
	*sqlite.Store
	claim   func(context.Context, verification.JobClaim) (verification.Job, error)
	receipt func(context.Context, id.ID) (verification.CommitReceipt, error)
}

func (s workerStore) ClaimVerificationJob(ctx context.Context, c verification.JobClaim) (verification.Job, error) {
	return s.claim(ctx, c)
}
func (s workerStore) GetCommittedResult(ctx context.Context, result id.ID) (verification.CommitReceipt, error) {
	return s.receipt(ctx, result)
}

func TestVerificationWorkerRejectsWrongClaimReceipt(t *testing.T) {
	f := newWorkerFixture(t)
	s := workerStore{Store: f.storage, claim: func(ctx context.Context, c verification.JobClaim) (verification.Job, error) {
		c.ClaimID = id.New(verification.PrefixJobClaim)
		return f.storage.ClaimVerificationJob(ctx, c)
	}}
	result, err := f.engine(t, s).VerifyRun(f.ctx, f.request)
	require.ErrorIs(t, err, verification.ErrJobConflict)
	require.Nil(t, result.Receipt)
	require.Zero(t, f.adapter.evaluations)
}

func TestVerificationWorkerSchedulesUniqueGroups(t *testing.T) {
	for _, optionalFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "optional last", true: "optional first"}[optionalFirst], func(t *testing.T) {
			criteria := []verification.Criterion{
				{ID: "a", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute},
				{ID: "b", Kind: verification.KindTest, Required: true, Verifier: "other", VerifierRevision: "2", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute},
				{ID: "c", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute},
			}
			optional := verification.Criterion{ID: "optional", Kind: verification.KindTest, Verifier: "advice", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute}
			expected := 2
			if optionalFirst {
				criteria = append([]verification.Criterion{optional}, criteria...)
				expected = 3
			} else {
				criteria = append(criteria, optional)
			}
			f := newWorkerFixture(t, criteria...)
			result, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
			require.NoError(t, err)
			require.Equal(t, verification.JobCommitted, result.Job.State)
			require.Len(t, result.Job.ReportIDs, expected)
			require.Equal(t, expected, f.adapter.evaluations)
			require.Equal(t, expected, f.adapter.estimates)
			require.Equal(t, int64(4*expected), verification.DebitCalls(result.Job.Calls).Tokens)
		})
	}
}

func TestVerificationWorkerRejectsChangedHistoricalReceipt(t *testing.T) {
	for _, mode := range []string{"result id", "binding", "contract", "run revision", "reports", "lease"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			committed, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
			require.NoError(t, err)
			s := workerStore{Store: f.storage, receipt: func(context.Context, id.ID) (verification.CommitReceipt, error) {
				receipt := *committed.Receipt
				switch mode {
				case "result id":
					receipt.Request.ResultID = id.New(verification.PrefixResult)
					receipt.Outbox.ResultID = receipt.Request.ResultID
				case "binding":
					receipt.Request.Current.Context.AuthorityRevision = "2"
					receipt.Outbox.BindingDigest, _ = receipt.Request.Current.Digest()
				case "contract":
					receipt.Request.ContractID = id.New(verification.PrefixContract)
				case "run revision":
					receipt.Request.ExpectedRunRevision++
					receipt.RunRevision++
				case "reports":
					receipt.Request.ReportIDs = []id.ID{id.New(verification.PrefixReport)}
				case "lease":
					receipt.Request.JobLease = nil
				}
				_, receipt.RequestDigest, err = receipt.Request.Freeze()
				require.NoError(t, err)
				sealed, sealErr := verification.SealCommitReceipt(receipt)
				require.NoError(t, sealErr)
				return sealed, nil
			}}
			result, err := f.engine(t, s).VerifyRun(f.ctx, f.request)
			require.ErrorIs(t, err, verification.ErrConflict)
			require.Nil(t, result.Receipt)
			require.Equal(t, 1, f.adapter.evaluations)
		})
	}
}

func TestVerificationWorkerStableResultIDAndTerminalJobs(t *testing.T) {
	f := newWorkerFixture(t)
	e := f.engine(t, f.storage)
	_, err := e.VerifyRun(f.ctx, f.request)
	require.NoError(t, err)
	f.request.ResultID = id.New(verification.PrefixResult)
	result, err := e.VerifyRun(f.ctx, f.request)
	require.ErrorIs(t, err, verification.ErrConflict)
	require.Nil(t, result.Receipt)
	for _, mode := range []string{"inconclusive", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newWorkerFixture(t)
			if mode == "inconclusive" {
				fixture.adapter.status = verification.StatusInconclusive
			} else {
				_, enqueueErr := fixture.storage.EnqueueVerificationJob(fixture.ctx, fixture.request.Job)
				require.NoError(t, enqueueErr)
				_, cancelErr := fixture.storage.CancelVerificationJob(fixture.ctx, fixture.request.Job.ID)
				require.NoError(t, cancelErr)
			}
			terminal, verifyErr := fixture.engine(t, fixture.storage).VerifyRun(fixture.ctx, fixture.request)
			require.NoError(t, verifyErr)
			require.Nil(t, terminal.Receipt)
			require.Equal(t, verification.JobState(mode), terminal.Job.State)
		})
	}
}

func TestVerificationWorkerBudgetStopsRemainingGroups(t *testing.T) {
	criteria := []verification.Criterion{
		{ID: "a", Kind: verification.KindTest, Required: true, Verifier: "first", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute},
		{ID: "b", Kind: verification.KindTest, Required: true, Verifier: "second", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute},
	}
	f := newWorkerFixture(t, criteria...)
	f.request.Job.EvaluationBudget.MaxCalls = 1
	e := f.engine(t, f.storage)
	for range 2 {
		result, err := e.VerifyRun(f.ctx, f.request)
		require.ErrorIs(t, err, verification.ErrJobBudgetExceeded)
		require.Nil(t, result.Receipt)
	}
	require.Equal(t, 1, f.adapter.evaluations)
	job, err := f.storage.GetVerificationJob(f.ctx, f.request.Job.ID)
	require.NoError(t, err)
	require.Equal(t, verification.JobRunning, job.State)
	require.Len(t, job.ReportIDs, 1)
	require.Equal(t, 2, f.llm.calls)
	require.Equal(t, 1, f.llm.tools)
}

func TestVerificationWorkerCommitsPrivateCandidate(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "recovered"}[lost], func(t *testing.T) {
			f := newWorkerFixture(t)
			intent, current, output := f.prepareIntent(t)
			captured, err := f.storage.CaptureCandidate(f.ctx, run.CandidateCapture{SchemaVersion: 1, IntentID: intent.ID, ExpectedRunRevision: current.Revision, Output: output, FinishReason: "stop"})
			require.NoError(t, err)
			f.request.Job = captured.Request
			f.adapter.lost = lost
			result, err := f.engine(t, f.storage).VerifyRun(f.ctx, f.request)
			storage := f.storage
			if lost {
				require.Error(t, err)
				saved, readErr := storage.GetRun(f.ctx, current.ID)
				require.NoError(t, readErr)
				require.Empty(t, saved.Output)
				require.NoError(t, storage.Close())
				storage = openSQLiteStoreForTest(t, f.path)
				result, err = f.engine(t, storage).VerifyRun(f.ctx, f.request)
			}
			require.NoError(t, err)
			require.Equal(t, verification.JobCommitted, result.Job.State)
			saved, err := storage.GetRun(f.ctx, current.ID)
			require.NoError(t, err)
			require.Equal(t, output, saved.Output)
			require.Equal(t, verification.StatusPassed, saved.VerificationStatus)
			require.Equal(t, 1, f.adapter.evaluations)
			require.Equal(t, 2, f.llm.calls)
			require.Equal(t, 1, f.llm.tools)
		})
	}
}
