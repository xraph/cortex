// Package optimization adapts captured Cortex executions to Sentinel scoring.
package optimization

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/xraph/sentinel"
	se "github.com/xraph/sentinel/engine"
	"github.com/xraph/sentinel/evalplan"
	"github.com/xraph/sentinel/evalrun"
	sid "github.com/xraph/sentinel/id"
	"github.com/xraph/sentinel/recorded"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/run"
)

// ScopeMapper resolves an explicit host mapping while preserving caller cancellation.
type ScopeMapper interface {
	Map(context.Context, cortex.Scope) (context.Context, error)
}

// CaseBinding ties a frozen public task to its private Sentinel definition.
type CaseBinding struct {
	Task   opt.TaskCase
	CaseID sid.CaseID
}

// Binding pins one exact source scope, evaluator and task roster.
type Binding struct {
	Scope           cortex.Scope
	AppID, TenantID string
	Plan            opt.ContentRef
	Cases           []CaseBinding
}

// Config supplies a Sentinel engine and explicit scope and case bindings.
type Config struct {
	Engine   *se.Engine
	Mapper   ScopeMapper
	Bindings []Binding
}

// Evaluator scores recorded output without an executable target callback.
type Evaluator struct{ config Config }

// NewEvaluator installs explicit bindings. It does not start a global evaluation worker.
func NewEvaluator(c Config) (*Evaluator, error) {
	if c.Engine == nil || c.Engine.Store() == nil || c.Mapper == nil || len(c.Bindings) == 0 {
		return nil, opt.ErrInvalidSpec
	}
	data, err := json.Marshal(c.Bindings)
	if err != nil || len(data) > 4<<20 {
		return nil, opt.ErrInvalidSpec
	}
	if decodeErr := json.Unmarshal(data, &c.Bindings); decodeErr != nil {
		return nil, decodeErr
	}
	seen := map[string]bool{}
	for _, b := range c.Bindings {
		key, scopeErr := opt.ScopeKey(b.Scope)
		if scopeErr != nil || b.Plan.Validate() != nil || b.AppID == "" || b.TenantID == "" || len(b.Cases) == 0 || seen[key+"/"+b.Plan.Ref] {
			return nil, opt.ErrInvalidSpec
		}
		seen[key+"/"+b.Plan.Ref] = true
		tasks, cases := map[string]bool{}, map[sid.ID]bool{}
		for _, v := range b.Cases {
			sealed, sealErr := opt.SealTaskCase(v.Task)
			if sealErr != nil || sealed.Digest != v.Task.Digest || tasks[v.Task.ID] || v.CaseID.IsNil() || cases[v.CaseID] {
				return nil, opt.ErrInvalidSpec
			}
			tasks[v.Task.ID] = true
			cases[v.CaseID] = true
		}
	}
	return &Evaluator{config: c}, nil
}
func equal(a, b any) bool {
	x, e := json.Marshal(a)
	y, f := json.Marshal(b)
	return e == nil && f == nil && bytes.Equal(x, y)
}
func (e *Evaluator) binding(ctx context.Context, ref opt.ContentRef) (Binding, error) {
	key, err := opt.ScopeKey(cortex.ScopeFromContext(ctx))
	if err != nil {
		return Binding{}, err
	}
	for _, b := range e.config.Bindings {
		if b.Scope.Canonical() == key && b.Plan == ref {
			return b, nil
		}
	}
	return Binding{}, opt.ErrPolicyDenied
}
func (e *Evaluator) mapped(ctx context.Context, b Binding) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if app := sentinel.AppFromContext(ctx); app != "" && app != b.AppID {
		return nil, nil, opt.ErrPolicyDenied
	}
	if tenant := sentinel.TenantFromContext(ctx); tenant != "" && tenant != b.TenantID {
		return nil, nil, opt.ErrPolicyDenied
	}
	mapped, err := e.config.Mapper.Map(ctx, cortex.Scope{Levels: slices.Clone(b.Scope.Levels)})
	if err != nil {
		return nil, nil, err
	}
	if mapped == nil || sentinel.AppFromContext(mapped) != b.AppID || sentinel.TenantFromContext(mapped) != b.TenantID || cortex.ScopeFromContext(mapped).Canonical() != b.Scope.Canonical() {
		return nil, nil, opt.ErrPolicyDenied
	}
	var child context.Context
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		child, cancel = context.WithDeadline(mapped, deadline)
	} else {
		child, cancel = context.WithCancel(mapped)
	}
	stop := context.AfterFunc(ctx, cancel)
	if err = ctx.Err(); err != nil {
		stop()
		cancel()
		return nil, nil, err
	}
	return child, func() { stop(); cancel() }, nil
}
func (e *Evaluator) plan(ctx context.Context, b Binding) (*evalplan.Plan, opt.EvaluationPlan, error) {
	var empty opt.EvaluationPlan
	key, err := sid.Parse(b.Plan.Ref)
	if err != nil {
		return nil, empty, opt.ErrInvalidSpec
	}
	p, err := e.config.Engine.Store().GetEvaluationPlan(ctx, key)
	if err != nil {
		return nil, empty, err
	}
	if p == nil || p.Validate(ctx) != nil || p.ID != key || p.Digest != b.Plan.Digest || b.Plan.Revision != strconv.Itoa(p.SchemaVersion) || len(p.Cases) != len(b.Cases) {
		return nil, empty, opt.ErrIncompleteEvidence
	}
	result := opt.EvaluationPlan{Ref: b.Plan, CaseDigests: map[string]string{}, FrozenAt: p.CreatedAt}
	scorers := map[string]string{}
	for _, binding := range b.Cases {
		var found *evalplan.Case
		for i := range p.Cases {
			if p.Cases[i].Case.ID == binding.CaseID {
				found = &p.Cases[i]
				break
			}
		}
		if found == nil || found.Case.Input != binding.Task.Input {
			return nil, empty, opt.ErrIncompleteEvidence
		}
		result.CaseDigests[binding.Task.ID] = binding.Task.Digest
		criteria := make([]opt.CriterionDefinition, 0, len(found.Criteria))
		for _, c := range found.Criteria {
			// Only built-in local scorers are admitted without a metered judge binding.
			if c.Revision != "sentinel/builtin/"+c.Scorer+"/v1" {
				return nil, empty, opt.ErrUnavailable
			}
			switch c.Scorer {
			case "exact", "contains", "not_contains", "regex", "json_valid", "length", "latency", "cost":
			default:
				return nil, empty, opt.ErrUnavailable
			}
			revision, revErr := e.config.Engine.Scorers().Revision(c.Scorer)
			if revErr != nil || revision != c.Revision {
				return nil, empty, opt.ErrStaleVersion
			}
			if old, ok := scorers[c.Scorer]; ok && old != c.Revision {
				return nil, empty, opt.ErrIncompleteEvidence
			}
			scorers[c.Scorer] = c.Revision
			criteria = append(criteria, opt.CriterionDefinition{ID: c.ID, Required: c.Required})
		}
		slices.SortFunc(criteria, func(a, b opt.CriterionDefinition) int {
			if a.ID < b.ID {
				return -1
			}
			if a.ID > b.ID {
				return 1
			}
			return 0
		})
		if result.Criteria == nil {
			result.Criteria = criteria
		} else if !equal(result.Criteria, criteria) {
			return nil, empty, opt.ErrIncompleteEvidence
		}
	}
	names := make([]string, 0, len(scorers))
	for name := range scorers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		result.Scorers = append(result.Scorers, run.Revision{Name: name, Revision: scorers[name]})
	}
	return p, result, nil
}

// Freeze resolves a previously frozen, host-bound Sentinel plan.
func (e *Evaluator) Freeze(ctx context.Context, r opt.EvaluationSpec) (opt.EvaluationPlan, error) {
	b, err := e.binding(ctx, r.Source)
	if err != nil {
		return opt.EvaluationPlan{}, err
	}
	if len(r.Tasks) != len(b.Cases) {
		return opt.EvaluationPlan{}, opt.ErrIncompleteEvidence
	}
	seen := map[string]bool{}
	for _, task := range r.Tasks {
		found := false
		for _, v := range b.Cases {
			if equal(task, v.Task) {
				found = true
			}
		}
		if !found || seen[task.ID] {
			return opt.EvaluationPlan{}, opt.ErrIncompleteEvidence
		}
		seen[task.ID] = true
	}
	mapped, done, err := e.mapped(ctx, b)
	if err != nil {
		return opt.EvaluationPlan{}, err
	}
	defer done()
	_, plan, err := e.plan(mapped, b)
	if err != nil {
		return opt.EvaluationPlan{}, err
	}
	for _, required := range r.RequiredCriteria {
		if !slices.ContainsFunc(plan.Criteria, func(c opt.CriterionDefinition) bool { return c.ID == required && c.Required }) {
			return opt.EvaluationPlan{}, opt.ErrIncompleteEvidence
		}
	}
	return plan, nil
}

// ConvertExecution preserves original output, timing, usage quality and evidence identities.
func ConvertExecution(r opt.ExecutionRecord) (se.RecordedCase, error) {
	sealed, err := opt.SealExecution(r)
	if err != nil || sealed.Digest != r.Digest {
		return se.RecordedCase{}, opt.ErrIncompleteEvidence
	}
	outcome := recorded.Failed
	switch r.State {
	case run.StateCompleted:
		outcome = recorded.Completed
	case run.StateIncomplete:
		outcome = recorded.Incomplete
	case run.StateCancelled:
		outcome = recorded.Cancelled
	}
	value := se.RecordedCase{InputDigest: r.InputDigest, Outcome: outcome, Output: r.Outcome.Output, Duration: r.Duration, SourceRunID: r.RunID.String(), Usage: evalrun.Usage{Tokens: r.Usage.Tokens, TokensQuality: evalrun.Quality(r.Usage.TokenQuality), Cost: float64(r.Usage.USDMicros) / 1e6, CostQuality: evalrun.Quality(r.Usage.MoneyQuality)}}
	if r.Usage.MoneyQuality == opt.UsageKnown || r.Usage.MoneyQuality == opt.UsageEstimated {
		value.Usage.Currency = "USD"
	}
	for _, ref := range r.Artifacts {
		digest, bindingErr := ref.BindingDigest()
		if bindingErr != nil {
			return se.RecordedCase{}, bindingErr
		}
		value.ArtifactRefs = append(value.ArtifactRefs, "artifact/"+digest)
	}
	for _, step := range r.StepIDs {
		value.ArtifactRefs = append(value.ArtifactRefs, "cortex/step/"+step.String())
	}
	for _, tool := range r.ToolCallIDs {
		value.ArtifactRefs = append(value.ArtifactRefs, "cortex/tool-call/"+tool.String())
	}
	return value, nil
}

// Evaluate scores one durable captured trial, reusing the recorded job on retry.
func (e *Evaluator) Evaluate(ctx context.Context, r opt.EvaluationRequest) (opt.Evaluation, error) {
	var empty opt.Evaluation
	if _, parseErr := opt.ParseID(r.ExperimentID.String(), opt.PrefixExperiment); parseErr != nil {
		return empty, parseErr
	}
	if _, parseErr := opt.ParseID(r.TrialID.String(), opt.PrefixTrial); parseErr != nil {
		return empty, parseErr
	}
	if !r.CandidateID.IsNil() {
		if _, parseErr := opt.ParseID(r.CandidateID.String(), opt.PrefixCandidate); parseErr != nil {
			return empty, parseErr
		}
	}
	b, err := e.binding(ctx, r.Plan.Ref)
	if err != nil {
		return empty, err
	}
	mapped, done, err := e.mapped(ctx, b)
	if err != nil {
		return empty, err
	}
	defer done()
	p, plan, err := e.plan(mapped, b)
	if err != nil {
		return empty, err
	}
	if !equal(plan, r.Plan) || r.ExperimentID.IsNil() || r.TrialID.IsNil() || !opt.ValidOpaqueKey(r.IdempotencyKey) {
		return empty, opt.ErrIncompleteEvidence
	}
	var binding *CaseBinding
	for i := range b.Cases {
		if b.Cases[i].Task.ID == r.CaseID {
			binding = &b.Cases[i]
		}
	}
	if binding == nil || binding.Task.Digest != r.CaseDigest || binding.Task.InputDigest != r.Execution.InputDigest || binding.Task.EnvironmentSnapshot != r.Execution.Environment.Snapshot || r.Execution.Environment.Scope.Canonical() != b.Scope.Canonical() {
		return empty, opt.ErrIncompleteEvidence
	}
	capture, err := ConvertExecution(r.Execution)
	if err != nil {
		return empty, err
	}
	if slices.ContainsFunc(plan.Scorers, func(revision run.Revision) bool { return revision.Name == "cost" }) && !capture.Usage.CostKnown() {
		return empty, fmt.Errorf("%w: cost scorer requires measured usage", opt.ErrIncompleteEvidence)
	}
	capture.CaseID, capture.Repetition, capture.InputDigest = binding.CaseID, r.Repetition, recorded.InputDigest(binding.Task.Input)
	request := se.RecordedRunRequest{PlanID: p.ID, PlanDigest: p.Digest, IdempotencyKey: r.IdempotencyKey, SourceExperimentID: r.ExperimentID.String(), SourceCandidateID: r.CandidateID.String(), Cases: []se.RecordedCase{capture}}
	result, evalErr := e.config.Engine.EvaluateRecorded(mapped, request)
	if result == nil || result.Recorded == nil {
		return empty, errors.Join(opt.ErrIncompleteEvidence, evalErr)
	}
	job := result.Recorded
	digest, digestErr := request.Digest()
	if digestErr != nil || job.Validate(mapped) != nil || job.RequestDigest != digest || !equal(job.Request, request) {
		return empty, opt.ErrIncompleteEvidence
	}
	if !job.Terminal() || len(job.Results) != 1 || job.Results[0].Result == nil {
		return empty, fmt.Errorf("%w: evaluation job pending", opt.ErrUnknownAttempt)
	}
	entry := job.Results[0]
	out := opt.Evaluation{ID: job.ID.String(), ReportID: job.RunID.String(), ExperimentID: r.ExperimentID, TrialID: r.TrialID, CaseID: r.CaseID, CaseDigest: r.CaseDigest, Repetition: r.Repetition, PlanDigest: p.Digest, ExecutionDigest: r.Execution.Digest, Scorers: plan.Scorers, StartedAt: job.CreatedAt, CompletedAt: job.UpdatedAt, Complete: r.Execution.State == run.StateCompleted && job.State == recorded.JobCompleted && entry.ExecutionError == "" && entry.ScoringError == ""}
	values := map[string]evalrun.ScorerResult{}
	for _, v := range entry.Result.ScorerResults {
		if _, ok := values[v.CriterionID]; ok {
			return empty, opt.ErrIncompleteEvidence
		}
		values[v.CriterionID] = v
	}
	for _, definition := range plan.Criteria {
		v, ok := values[definition.ID]
		criterion := opt.CriterionValue{ID: definition.ID, Required: definition.Required}
		if !ok || v.Errored() {
			criterion.Error = "scoring result unavailable"
			out.Complete = false
		} else {
			if v.Required != definition.Required || math.IsNaN(v.Score) || v.Score < 0 || v.Score > 1 {
				return empty, opt.ErrIncompleteEvidence
			}
			score := v.Score
			criterion.Score = &score
			criterion.Passed = v.Passed
		}
		out.Criteria = append(out.Criteria, criterion)
	}
	for _, message := range []string{job.Error, entry.ExecutionError, entry.ScoringError} {
		if message != "" {
			out.Errors = append(out.Errors, message)
			out.Complete = false
		}
	}
	if out.Complete {
		if len(values) != len(plan.Criteria) || math.IsNaN(entry.Result.Score) || entry.Result.Score < 0 || entry.Result.Score > 1 {
			return empty, opt.ErrIncompleteEvidence
		}
		score := entry.Result.Score
		out.Score = &score
	}
	// Local scorers cannot make charged calls. No provider receipt is fabricated.
	out.JudgeUsage = opt.Usage{TokenQuality: opt.UsageKnown, MoneyQuality: opt.UsageUnknown}
	if evalErr != nil && len(out.Errors) == 0 {
		out.Errors = append(out.Errors, evalErr.Error())
		out.Complete = false
		out.Score = nil
	}
	return out, nil
}
