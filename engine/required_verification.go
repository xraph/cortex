package engine

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

const requiredPreparedSchemaVersion = 6

// VerificationIntentInput supplies trusted identity before generation. Deadline
// includes the caller's narrower deadline. Inputs and Scope are detached.
// The builder must reference Inputs.Encode() in Context.ExecutionInputs and
// Binding.Evidence. Cortex authorizes and persists those exact bytes.
type VerificationIntentInput struct {
	Inputs         verification.ExecutionInputs
	RunID          id.AgentRunID
	Scope          cortex.Scope
	PreparedDigest string
	Deadline       time.Time
}

// VerificationIntentBuilder binds host context, evidence retention and stable
// job/result identities. Building an intent must not dispatch billable work.
// Cortex validates the returned sealed intent against its frozen requirement.
type VerificationIntentBuilder interface {
	BuildVerificationIntent(context.Context, VerificationIntentInput) (verification.CaptureIntent, error)
}

// RequiredVerificationConfig gates prepared, sync, stream and resumed runs.
// Evaluator tokens are reserved from the total run token budget at preparation.
// Money limits cover evaluation only. The builder owns context provenance; the
// capture authorizer checks artifact creation before generation and capture,
// and input-evidence reads on resume.
// Tools require both a ToolAuthorizer and HandoffAuthorizer. Checked-segment
// publication remains unsupported. Other ports and worker settings are required.
type RequiredVerificationConfig struct {
	HandoffAuthorizer ToolHandoffAuthorizer
	Revision          string
	ContractID        id.ID
	EvaluationBudget  verification.EvaluationBudget
	Builder           VerificationIntentBuilder
	CaptureAuthorizer evidence.Authorizer
	WorkerOwner       string
	LeaseDuration     time.Duration
}

type requiredPolicy struct {
	Revision         string
	ContractID       id.ID
	ContractDigest   string
	EvaluationBudget verification.EvaluationBudget
	TotalTokens      int
}

type requiredVerificationStore interface {
	verification.CaptureIntentStore
	run.CandidateStore
	evidence.Storage
}

// WithRequiredVerification enables fail-closed candidate publication. It also
// requires WithVerificationWorker, a real LLM and durable capture capabilities.
// Required runs keep conversation history private even after result commit;
// historical delivery and history publication need a separate authorized path.
func WithRequiredVerification(config RequiredVerificationConfig) Option {
	return func(e *Engine) error { copied := config; e.requiredVerification = &copied; return nil }
}

func (e *Engine) configureRequiredVerification() error {
	c := e.requiredVerification
	if c == nil {
		return nil
	}
	_, ok := e.store.(requiredVerificationStore)
	if !ok || e.verificationWorker == nil || e.llm == nil || c.Builder == nil || c.CaptureAuthorizer == nil || !utf8.ValidString(c.Revision) || !utf8.ValidString(c.WorkerOwner) || strings.TrimSpace(c.Revision) == "" || len(c.Revision) > 256 || c.ContractID.IsNil() || c.ContractID.Prefix() != verification.PrefixContract || c.EvaluationBudget.Validate() != nil || strings.TrimSpace(c.WorkerOwner) == "" || len(c.WorkerOwner) > 256 || c.LeaseDuration <= 0 || c.LeaseDuration > verification.MaxJobLease {
		return cortex.ErrVerificationUnavailable
	}
	return nil
}

func (e *Engine) prepareRequiredVerification(ctx context.Context, cfg *resolvedConfig, defs []llm.Tool) error {
	c := e.requiredVerification
	if c == nil {
		return nil
	}
	if strings.TrimSpace(e.modelRevisions[cfg.Model]) == "" {
		return cortex.ErrVerificationUnavailable
	}
	for _, def := range defs {
		if strings.TrimSpace(e.toolRevisions[def.Name]) == "" {
			return cortex.ErrVerificationUnavailable
		}
	}
	if cfg.Publication.Mode == safety.ModeCheckedSegments || cfg.Budget.MaxTokens <= 0 || c.EvaluationBudget.MaxTokens >= int64(cfg.Budget.MaxTokens) || cfg.Budget.Deadline.IsZero() {
		return cortex.ErrVerificationUnavailable
	}
	if len(defs) > 0 && (c.HandoffAuthorizer == nil || e.authorizer == nil) {
		return cortex.ErrVerificationUnavailable
	}
	cfg.Tools = nil
	for _, def := range defs {
		cfg.Tools = append(cfg.Tools, def.Name)
	}
	contract, err := e.verificationWorker.storage.GetContract(ctx, c.ContractID)
	if err != nil || contract.Validate() != nil || contract.ID != c.ContractID || contract.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return cortex.ErrVerificationUnavailable
	}
	cfg.Verification = &requiredPolicy{Revision: c.Revision, ContractID: c.ContractID, ContractDigest: contract.Digest, EvaluationBudget: c.EvaluationBudget, TotalTokens: cfg.Budget.MaxTokens}
	cfg.Budget.MaxTokens -= int(c.EvaluationBudget.MaxTokens)
	cfg.AllToolsRestricted = true
	cfg.ToolsRestricted = true
	return nil
}

func validRequiredSnapshot(s *preparedSnapshot) bool {
	p := s.Config.Verification
	if p == nil {
		return s.SchemaVersion == preparedSchemaVersion
	}
	return (s.SchemaVersion == requiredPreparedSchemaVersion) && utf8.ValidString(p.Revision) && strings.TrimSpace(p.Revision) != "" && len(p.Revision) <= 256 && !p.ContractID.IsNil() && p.ContractID.Prefix() == verification.PrefixContract && evidence.ValidDigest(p.ContractDigest) && p.EvaluationBudget.Validate() == nil && p.TotalTokens > 0 && p.EvaluationBudget.MaxTokens < int64(p.TotalTokens) && s.Config.Budget.MaxTokens == p.TotalTokens-int(p.EvaluationBudget.MaxTokens) && !s.Config.Budget.Deadline.IsZero() && s.Config.AllToolsRestricted && s.Config.ToolsRestricted && s.Config.Publication.Mode != safety.ModeCheckedSegments
}

func (e *Engine) requireVerification(s *preparedSnapshot) error {
	if !validRequiredSnapshot(s) {
		return cortex.ErrInvalidPrepared
	}
	p, c := s.Config.Verification, e.requiredVerification
	if p == nil && c == nil {
		return nil
	}
	if p == nil || c == nil || e.verificationWorker == nil || (len(s.Tools) > 0 && (c.HandoffAuthorizer == nil || e.authorizer == nil)) || p.Revision != c.Revision || p.ContractID != c.ContractID || p.EvaluationBudget != c.EvaluationBudget {
		return cortex.ErrVerificationUnavailable
	}
	return nil
}

func (e *Engine) buildRequiredIntent(ctx context.Context, p *PreparedAgent, cfg resolvedConfig, r *run.Run, messages []llm.Message) (verification.CaptureIntent, error) {
	if cfg.Verification == nil {
		return verification.CaptureIntent{}, nil
	}
	inputs := verification.ExecutionInputs{SchemaVersion: 1, RunID: r.ID, AgentID: r.AgentID, SessionID: r.SessionID, PreparedDigest: p.Digest(), Input: r.Input, History: messages, Model: cfg.Model, ModelRevision: r.Provenance.ModelRevision}
	for _, tool := range r.Provenance.Tools {
		inputs.Tools = append(inputs.Tools, verification.ToolRevision{Name: tool.Name, Revision: tool.Revision})
	}
	data, err := inputs.Encode()
	if err != nil {
		return verification.CaptureIntent{}, cortex.ErrVerificationUnavailable
	}
	detached, err := verification.DecodeExecutionInputs(data)
	if err != nil {
		return verification.CaptureIntent{}, cortex.ErrVerificationUnavailable
	}
	policy := cfg.Verification
	scope := r.Scope
	scope.Levels = append([]cortex.Level(nil), scope.Levels...)
	intent, err := e.requiredVerification.Builder.BuildVerificationIntent(ctx, VerificationIntentInput{Inputs: detached, RunID: r.ID, Scope: scope, PreparedDigest: p.Digest(), Deadline: cfg.Budget.Deadline})
	if err != nil || intent.Validate() != nil {
		return verification.CaptureIntent{}, cortex.ErrVerificationUnavailable
	}
	intent, err = verification.SealCaptureIntent(intent)
	if err != nil {
		return verification.CaptureIntent{}, cortex.ErrVerificationUnavailable
	}
	req := intent.Template
	if !requiredIntentMatchesPolicy(intent, policy, r) || req.Deadline.After(cfg.Budget.Deadline) || !req.Deadline.After(e.verificationWorker.config.Clock.Now()) || intent.MaxTranscriptBytes > 1<<20 || intent.MaxCandidateBytes > 1<<20 || intent.MaxCandidateBytes > int64(cfg.Publication.MaxOutputBytes) {
		return verification.CaptureIntent{}, cortex.ErrVerificationUnavailable
	}
	if authErr := e.authorizeCandidate(ctx, req.Binding.Candidate); authErr != nil {
		return verification.CaptureIntent{}, authErr
	}
	if authErr := e.authorizeCandidate(ctx, *intent.Transcript); authErr != nil {
		return verification.CaptureIntent{}, authErr
	}
	ref := req.Binding.Context.ExecutionInputs
	if ref == nil || ref.SHA256 != evidence.Digest(data) || ref.Bytes != int64(len(data)) {
		return verification.CaptureIntent{}, cortex.ErrVerificationUnavailable
	}
	if authErr := e.authorizeCandidate(ctx, *ref); authErr != nil {
		return verification.CaptureIntent{}, authErr
	}
	storage, ok := e.store.(requiredVerificationStore)
	if !ok || storage.SaveEvidence(ctx, evidence.Artifact{Ref: *ref, Data: data}) != nil {
		return verification.CaptureIntent{}, cortex.ErrVerificationUnavailable
	}
	r.Provenance.ExecutionInputsDigest = ref.SHA256
	r.Provenance.VerificationRequired = true
	r.Provenance.VerificationIntentID = intent.ID.String()
	r.Provenance.AcceptanceRef = policy.ContractID.String()
	return intent, nil
}

func (e *Engine) authorizeCandidate(ctx context.Context, ref evidence.Ref) error {
	ref.Scope.Levels = append([]cortex.Level(nil), ref.Scope.Levels...)
	if err := e.requiredVerification.CaptureAuthorizer.AuthorizeEvidence(ctx, evidence.ActionPut, ref); err != nil {
		return cortex.ErrVerificationUnavailable
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (e *Engine) finalizeRequiredCandidate(ctx context.Context, r *run.Run, outcome run.Outcome) error {
	storage, ok := e.store.(requiredVerificationStore)
	if !ok || e.requiredVerification == nil || e.verificationWorker == nil {
		return cortex.ErrVerificationUnavailable
	}
	intent, err := storage.GetRunCaptureIntent(ctx, r.ID)
	if err != nil || intent.Validate() != nil || intent.ID.String() != r.Provenance.VerificationIntentID || intent.Template.Binding.Scope.Canonical() != r.Scope.Canonical() || intent.Template.Binding.RunID != r.ID || intent.Template.Binding.Context.PreparedDigest != r.Provenance.PreparedDigest {
		return cortex.ErrVerificationUnavailable
	}
	// Persist scanner provenance and cumulative accounting before the atomic
	// capture. A failed capture leaves no public candidate and can be reconciled.
	r.StepCount = outcome.StepCount
	r.TokensUsed = outcome.TokensUsed
	if err = e.store.UpdateRun(ctx, r); err != nil {
		return cortex.ErrVerificationUnavailable
	}
	if outcome.Transcript == nil {
		return cortex.ErrVerificationUnavailable
	}
	transcript, err := outcome.Transcript.Encode()
	if err != nil {
		return cortex.ErrVerificationUnavailable
	}
	request, err := intent.JobFor(r.Revision+1, outcome.Output, transcript)
	if err != nil {
		return cortex.ErrVerificationUnavailable
	}
	if authErr := e.authorizeCandidate(ctx, request.Binding.Candidate); authErr != nil {
		return authErr
	}
	if authErr := e.authorizeCandidate(ctx, *request.Binding.Context.ExecutionTranscript); authErr != nil {
		return authErr
	}
	job, err := storage.CaptureCandidate(ctx, run.CandidateCapture{SchemaVersion: 2, Transcript: transcript, IntentID: intent.ID, ExpectedRunRevision: r.Revision, Output: outcome.Output, FinishReason: outcome.FinishReason, Usage: r.Usage})
	if err != nil {
		return cortex.ErrVerificationUnavailable
	}
	saved, err := e.store.GetRun(ctx, r.ID)
	if err != nil {
		return cortex.ErrVerificationUnavailable
	}
	*r = *saved
	e.emitTerminalOutcome(ctx, r)
	result, err := e.VerifyRun(ctx, VerifyRunRequest{Job: job.Request, ResultID: intent.ResultID, Claim: verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: e.requiredVerification.WorkerOwner, Duration: e.requiredVerification.LeaseDuration}})
	if err != nil {
		r.Output = ""
		return cortex.ErrVerificationUnavailable
	}
	saved, err = e.store.GetRun(ctx, r.ID)
	if err != nil {
		return cortex.ErrVerificationUnavailable
	}
	*r = *saved
	if result.Receipt == nil {
		r.Output = ""
		return cortex.ErrRunUnverified
	}
	if r.State != run.StateCompleted || r.VerificationStatus != verification.StatusPassed || r.ResultRef != intent.ResultID.String() || evidence.Digest([]byte(r.Output)) != job.Request.Binding.Candidate.SHA256 {
		r.Output = ""
		return cortex.ErrVerificationUnavailable
	}
	var duration time.Duration
	if r.StartedAt != nil {
		duration = time.Since(*r.StartedAt)
	}
	e.extensions.EmitRunCompleted(ctx, r.AgentID, r.ID, r.Output, duration)
	return nil
}

func publicGenerationText(r *run.Run, text string) string {
	if r.Provenance.VerificationRequired {
		return ""
	}
	return text
}

// Execution may have completed through another capture owner. It does not
// establish verified success for the caller waiting on generation.
func requiredCompletionError(r *run.Run) error {
	if r.Provenance.VerificationRequired && r.State == run.StateCompleted && (r.VerificationStatus != verification.StatusPassed || r.ResultRef == "") {
		r.Output = ""
		return cortex.ErrRunUnverified
	}
	return nil
}

// Registration and resume must agree on the same immutable policy binding.
func requiredIntentMatchesPolicy(intent verification.CaptureIntent, policy *requiredPolicy, r *run.Run) bool {
	req := intent.Template
	return intent.SchemaVersion == 2 && intent.Transcript != nil && policy != nil && req.EvaluationBudget != nil && req.ExpectedRunRevision == 1 && req.Binding.RunID == r.ID && req.Binding.Scope.Canonical() == r.Scope.Canonical() && req.Binding.Context.PreparedDigest == r.Provenance.PreparedDigest && req.Binding.Context.PolicyRevision == policy.Revision && req.ContractID == policy.ContractID && req.Binding.ContractDigest == policy.ContractDigest && *req.EvaluationBudget == policy.EvaluationBudget
}
