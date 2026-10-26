package cognitive

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

var (
	// ErrInvalidReflection rejects incomplete or unbounded controller inputs.
	ErrInvalidReflection = errors.New("cortex: invalid reflection")
	// ErrReflectionConflict rejects stale decisions and changed immutable policy.
	ErrReflectionConflict = errors.New("cortex: reflection conflict")
	// ErrUnsupportedReflection rejects actions without a controlled runtime route.
	ErrUnsupportedReflection = errors.New("cortex: unsupported reflection strategy or action")
)

// ReflectionAction is a deterministic next action, never an authority grant.
type ReflectionAction string

const (
	ActionContinue ReflectionAction = "continue"
	ActionRetrieve ReflectionAction = "retrieve"
	ActionVerify   ReflectionAction = "verify"
	ActionReplan   ReflectionAction = "replan"
	ActionDelegate ReflectionAction = "delegate"
	ActionAsk      ReflectionAction = "ask"
	ActionStop     ReflectionAction = "stop"
)

// ReflectionReason contains only bounded operational explanations.
type ReflectionReason string

const (
	ReflectionHealthy          ReflectionReason = "healthy"
	ReflectionStuck            ReflectionReason = "no_progress"
	ReflectionFailure          ReflectionReason = "repeated_failure"
	ReflectionConflict         ReflectionReason = "contradictory_evidence"
	ReflectionNoNewEvidence    ReflectionReason = "no_new_evidence"
	ReflectionLimit            ReflectionReason = "reflection_limit"
	ReflectionBudget           ReflectionReason = "budget_exhausted"
	ReflectionPending          ReflectionReason = "prior_action_unresolved"
	ReflectionEffects          ReflectionReason = "unresolved_effects"
	ReflectionMissingInput     ReflectionReason = "missing_input"
	ReflectionMissingAuthority ReflectionReason = "missing_authority"
	ReflectionUnresolvedRisk   ReflectionReason = "unresolved_risk"
	ReflectionSuperseded       ReflectionReason = "user_intervention"
	ReflectionInvalid          ReflectionReason = "invalid_observation"
)

// ReflectionInsufficientEvidence stops analytical phases until evidence is supplied.
const ReflectionInsufficientEvidence ReflectionReason = "insufficient_evidence"

// ReflectionPolicy is opt-in. Legacy persona phases do not enable it. These
// strategies map to bounded decisions, not separate model reasoning algorithms.
type ReflectionPolicy struct {
	SchemaVersion    int              `json:"schema_version"`
	Revision         string           `json:"revision"`
	MaxReflections   int              `json:"max_reflections"`
	StuckSteps       int              `json:"stuck_steps"`
	FailureThreshold int              `json:"failure_threshold"`
	OnStuck          ReflectionAction `json:"on_stuck"`
	Phases           []Phase          `json:"phases"`
}

// Normalize applies finite defaults and rejects unsupported runtime promises.
func (p ReflectionPolicy) Normalize() (ReflectionPolicy, error) {
	if p.SchemaVersion == 0 {
		p.SchemaVersion = 1
	}
	if p.MaxReflections == 0 {
		p.MaxReflections = 2
	}
	if p.StuckSteps == 0 {
		p.StuckSteps = 3
	}
	if p.FailureThreshold == 0 {
		p.FailureThreshold = 2
	}
	if p.OnStuck == "" {
		p.OnStuck = ActionStop
	}
	if len(p.Phases) == 0 {
		p.Phases = []Phase{{Strategy: StrategyReflective}}
	}
	if p.SchemaVersion != 1 || !contextText(p.Revision, 256) || p.MaxReflections < 1 || p.MaxReflections > 16 || p.StuckSteps < 1 || p.StuckSteps > 1024 || p.FailureThreshold < 1 || p.FailureThreshold > 1024 || len(p.Phases) > 16 {
		return ReflectionPolicy{}, ErrInvalidReflection
	}
	if p.OnStuck != ActionStop && p.OnStuck != ActionReplan {
		return ReflectionPolicy{}, ErrUnsupportedReflection
	}
	p.Phases = append([]Phase(nil), p.Phases...)
	for i, phase := range p.Phases {
		if phase.Strategy != StrategyAnalytical && phase.Strategy != StrategyReflective && phase.Strategy != StrategyReactive {
			return ReflectionPolicy{}, ErrUnsupportedReflection
		}
		if phase.MaxSteps < 0 || phase.MaxSteps > 1024 {
			return ReflectionPolicy{}, ErrInvalidReflection
		}
		switch phase.Transition {
		case "":
			if i != len(p.Phases)-1 {
				return ReflectionPolicy{}, ErrInvalidReflection
			}
		case TransitionAfterSteps:
			if phase.MaxSteps < 1 {
				return ReflectionPolicy{}, ErrInvalidReflection
			}
		case TransitionOnStuck, TransitionOnError, TransitionOnPlanComplete:
		default:
			return ReflectionPolicy{}, ErrInvalidReflection
		}
	}
	return p, nil
}

// ReflectionBinding pins immutable execution limits and required verification.
// SharedPlanID is optional and must be authenticated against current ownership.
type ReflectionBinding struct {
	Scope                cortex.Scope     `json:"scope"`
	RunID                id.AgentRunID    `json:"run_id"`
	AgentID              id.AgentID       `json:"agent_id"`
	PreparedDigest       string           `json:"prepared_digest"`
	Budget               execution.Budget `json:"budget"`
	VerificationRequired bool             `json:"verification_required"`
	SharedPlanID         id.ID            `json:"shared_plan_id,omitzero"`
}

// Validate checks identity and requires finite step and token ceilings.
func (b ReflectionBinding) Validate() error {
	if cortex.ValidateScope(b.Scope) != nil || b.RunID.IsNil() || b.RunID.Prefix() != id.PrefixAgentRun || b.AgentID.IsNil() || b.AgentID.Prefix() != id.PrefixAgent || !evidence.ValidDigest(b.PreparedDigest) || b.Budget.Validate() != nil || b.Budget.MaxSteps < 1 || b.Budget.MaxTokens < 1 {
		return ErrInvalidReflection
	}
	return nil
}

// ReflectionSnapshot contains trusted observable progress, never model scores.
// The host authenticates progress, plan completion, missing inputs and evidence
// semantics. Storage additionally requires each evidence object to be retained.
type ReflectionSnapshot struct {
	Binding             ReflectionBinding    `json:"binding"`
	ObservedAt          time.Time            `json:"observed_at"`
	Step                int                  `json:"step"`
	Usage               execution.Usage      `json:"usage"`
	ProgressDigest      string               `json:"progress_digest"`
	Evidence            []evidence.Ref       `json:"evidence,omitempty"`
	Assumptions         []Assumption         `json:"assumptions,omitempty"`
	Services            []ServiceObservation `json:"services,omitempty"`
	NoProgressSteps     int                  `json:"no_progress_steps"`
	ConsecutiveFailures int                  `json:"consecutive_failures"`
	PlanComplete        bool                 `json:"plan_complete"`
	HadError            bool                 `json:"had_error"`
	UnresolvedEffects   bool                 `json:"unresolved_effects"`
	RequiredInput       ReflectionReason     `json:"required_input,omitempty"`
}

// Validate bounds observations without certifying their producer or meaning.
func (s ReflectionSnapshot) Validate() error {
	if s.Binding.Validate() != nil || s.ObservedAt.IsZero() || s.Step < 0 || s.Step > 1_000_000 || s.NoProgressSteps < 0 || s.NoProgressSteps > s.Step || s.ConsecutiveFailures < 0 || s.ConsecutiveFailures > s.Step || !evidence.ValidDigest(s.ProgressDigest) || len(s.Evidence) > 64 || len(s.Assumptions) > 64 || len(s.Services) > 64 || s.Usage.Steps < 0 || s.Usage.Tokens < 0 || s.Usage.ReportedTokens < 0 || s.Usage.ReportedTokens > s.Usage.Tokens || s.Usage.ToolCalls < 0 {
		return ErrInvalidReflection
	}
	if s.RequiredInput != "" && s.RequiredInput != ReflectionMissingInput && s.RequiredInput != ReflectionMissingAuthority && s.RequiredInput != ReflectionUnresolvedRisk {
		return ErrInvalidReflection
	}
	seen := map[string]bool{}
	for _, a := range s.Assumptions {
		if a.validate() != nil || seen[a.Key] {
			return ErrInvalidReflection
		}
		seen[a.Key] = true
	}
	for _, ref := range s.References() {
		if ref.Validate() != nil || ref.Scope.Canonical() != s.Binding.Scope.Canonical() || ref.CreatedAt.After(s.ObservedAt) || !s.ObservedAt.Before(ref.RetainUntil) {
			return ErrInvalidReflection
		}
	}
	for _, health := range s.Services {
		if !contextText(health.Service, 256) || health.ObservedAt.IsZero() || !health.ExpiresAt.After(health.ObservedAt) || health.ExpiresAt.Sub(health.ObservedAt) > 5*time.Minute || (health.Health != HealthHealthy && health.Health != HealthUnavailable) {
			return ErrInvalidReflection
		}
	}
	return nil
}

// References returns detached reference values used by the observation.
func (s ReflectionSnapshot) References() []evidence.Ref {
	refs := append([]evidence.Ref(nil), s.Evidence...)
	for _, a := range s.Assumptions {
		refs = append(refs, a.Evidence...)
	}
	for _, health := range s.Services {
		refs = append(refs, health.Evidence)
	}
	for i := range refs {
		refs[i].Scope.Levels = append([]cortex.Level(nil), refs[i].Scope.Levels...)
	}
	return refs
}

// ReflectionState is recovered from the last immutable transition. Interventions
// advance ControlRevision; they never reset consumed attempts or raise limits.
type ReflectionState struct {
	Version            int64  `json:"version"`
	ControlRevision    int64  `json:"control_revision"`
	Phase              int    `json:"phase"`
	PhaseStartStep     int    `json:"phase_start_step"`
	Reflections        int    `json:"reflections"`
	LastEvidenceDigest string `json:"last_evidence_digest,omitempty"`
	LastStep           int    `json:"last_step"`
	Pending            bool   `json:"pending"`
}

// ReflectionDecision contains no replacement budget, grant or success criterion.
type ReflectionDecision struct {
	Action         ReflectionAction `json:"action"`
	Reason         ReflectionReason `json:"reason"`
	Reflect        bool             `json:"reflect"`
	Phase          int              `json:"phase"`
	PhaseChanged   bool             `json:"phase_changed"`
	EvidenceDigest string           `json:"evidence_digest"`
}

// Decide selects a bounded action from observable progress and retained state.
func Decide(s ReflectionSnapshot, p ReflectionPolicy, state ReflectionState) ReflectionDecision {
	d := ReflectionDecision{Action: ActionStop, Reason: ReflectionInvalid, Phase: state.Phase}
	policy, err := p.Normalize()
	if err != nil || s.Validate() != nil || state.Phase < 0 || state.Phase >= len(policy.Phases) || state.Reflections < 0 {
		return d
	}
	identities := make([]string, 0)
	for _, ref := range s.References() {
		identities = append(identities, ref.SHA256)
	}
	slices.Sort(identities)
	identities = slices.Compact(identities)
	data, encodeErr := json.Marshal(struct {
		Progress string
		Evidence []string
	}{s.ProgressDigest, identities})
	if encodeErr != nil {
		return d
	}
	d.EvidenceDigest = evidence.Digest(data)
	phase := policy.Phases[state.Phase]
	stuck := s.NoProgressSteps >= policy.StuckSteps
	transition := (phase.Transition == TransitionAfterSteps && s.Step-state.PhaseStartStep >= phase.MaxSteps) || (phase.Transition == TransitionOnStuck && stuck) || (phase.Transition == TransitionOnError && s.HadError) || (phase.Transition == TransitionOnPlanComplete && s.PlanComplete)
	if transition && state.Phase+1 < len(policy.Phases) {
		d.Phase++
		d.PhaseChanged = true
	}
	if s.UnresolvedEffects {
		d.Reason = ReflectionEffects
		return d
	}
	if s.Binding.Budget.Check(s.Usage, s.ObservedAt) != "" {
		d.Reason = ReflectionBudget
		return d
	}
	if state.Pending {
		d.Reason = ReflectionPending
		return d
	}
	d.Action, d.Reason = ActionContinue, ReflectionHealthy
	action := policy.OnStuck
	strategy := policy.Phases[d.Phase].Strategy
	if strategy == StrategyReactive {
		action = ActionStop
	}
	if s.RequiredInput != "" {
		d.Action, d.Reason = ActionAsk, s.RequiredInput
	} else {
		for _, a := range s.Assumptions {
			if strategy == StrategyAnalytical && a.State == AssumptionUnresolved {
				d.Action, d.Reason = ActionStop, ReflectionInsufficientEvidence
				break
			}
			if a.State == AssumptionDisputed {
				d.Action, d.Reason = action, ReflectionConflict
				break
			}
		}
		if d.Reason == ReflectionHealthy && s.ConsecutiveFailures >= policy.FailureThreshold {
			d.Action, d.Reason = action, ReflectionFailure
		}
		if d.Reason == ReflectionHealthy && stuck {
			d.Action, d.Reason = action, ReflectionStuck
		}
	}
	if d.Reason == ReflectionHealthy {
		return d
	}
	if state.Reflections >= policy.MaxReflections {
		d.Action, d.Reason = ActionStop, ReflectionLimit
		return d
	}
	if state.LastEvidenceDigest == d.EvidenceDigest {
		d.Action, d.Reason = ActionStop, ReflectionNoNewEvidence
		return d
	}
	d.Reflect = true
	return d
}

// ReflectionRequest pins optimistic state, current run revision and policy.
// Intervention is an authenticated user correction reference, never model text.
type ReflectionRequest struct {
	SchemaVersion           int                `json:"schema_version"`
	ExpectedVersion         int64              `json:"expected_version"`
	ExpectedControlRevision int64              `json:"expected_control_revision"`
	ExpectedRunRevision     int64              `json:"expected_run_revision"`
	Snapshot                ReflectionSnapshot `json:"snapshot"`
	Policy                  ReflectionPolicy   `json:"policy"`
	Intervention            *ContextSource     `json:"intervention,omitempty"`
}

// Freeze validates and detaches authority inputs before crossing host callbacks.
func (r ReflectionRequest) Freeze() (ReflectionRequest, string, error) {
	policy, err := r.Policy.Normalize()
	if err != nil {
		return ReflectionRequest{}, "", err
	}
	r.Policy = policy
	if r.SchemaVersion != 1 || r.ExpectedVersion < 0 || r.ExpectedVersion >= 4096 || r.ExpectedControlRevision < 0 || r.ExpectedControlRevision > 4096 || r.ExpectedRunRevision < 1 || r.Snapshot.Validate() != nil {
		return ReflectionRequest{}, "", ErrInvalidReflection
	}
	if r.Intervention != nil && (!contextText(r.Intervention.Reference, 1024) || !evidence.ValidDigest(r.Intervention.Digest)) {
		return ReflectionRequest{}, "", ErrInvalidReflection
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 512<<10 {
		return ReflectionRequest{}, "", ErrInvalidReflection
	}
	var frozen ReflectionRequest
	if json.Unmarshal(data, &frozen) != nil {
		return ReflectionRequest{}, "", ErrInvalidReflection
	}
	return frozen, evidence.Digest(data), nil
}

// ReflectionReceipt retains the checked decision before any action is dispatched.
// Fresh is process-local acknowledgement only and is never recovered as true.
type ReflectionReceipt struct {
	Request       ReflectionRequest  `json:"request"`
	RequestDigest string             `json:"request_digest"`
	Before        ReflectionState    `json:"before"`
	State         ReflectionState    `json:"state"`
	Decision      ReflectionDecision `json:"decision"`
	At            time.Time          `json:"at"`
	Fresh         bool               `json:"-"`
}

// AdvanceReflection derives the complete immutable transition.
func AdvanceReflection(req ReflectionRequest, before ReflectionState, at time.Time) (ReflectionReceipt, error) {
	r, digest, err := req.Freeze()
	if err != nil {
		return ReflectionReceipt{}, err
	}
	if at.IsZero() || r.ExpectedVersion != before.Version || r.ExpectedControlRevision != before.ControlRevision || r.Snapshot.Step < before.LastStep {
		return ReflectionReceipt{}, ErrReflectionConflict
	}
	next := before
	next.Version++
	next.LastStep = r.Snapshot.Step
	if next.ControlRevision == 0 {
		next.ControlRevision = 1
	}
	decision := Decide(r.Snapshot, r.Policy, before)
	if r.Intervention != nil {
		next.ControlRevision++
		next.Pending = false
		decision = ReflectionDecision{Action: ActionStop, Reason: ReflectionSuperseded, Phase: before.Phase}
	} else {
		if decision.Reason == ReflectionInvalid {
			return ReflectionReceipt{}, ErrInvalidReflection
		}
		if decision.PhaseChanged {
			next.Phase = decision.Phase
			next.PhaseStartStep = r.Snapshot.Step
		}
		if decision.Reflect {
			next.Reflections++
			next.LastEvidenceDigest = decision.EvidenceDigest
		}
		if decision.Action == ActionReplan || decision.Action == ActionAsk {
			next.Pending = true
		}
	}
	return ReflectionReceipt{Request: r, RequestDigest: digest, Before: before, State: next, Decision: decision, At: at}, nil
}

// Validate recomputes the transition, excluding its transient fresh flag.
func (r ReflectionReceipt) Validate() error {
	expected, err := AdvanceReflection(r.Request, r.Before, r.At)
	if err != nil {
		return err
	}
	if expected.RequestDigest != r.RequestDigest || !reflect.DeepEqual(expected.State, r.State) || !reflect.DeepEqual(expected.Decision, r.Decision) {
		return ErrInvalidReflection
	}
	return nil
}

// ReflectionStore is trusted persistence. Public callers need current host guards.
type ReflectionStore interface {
	GetReflection(context.Context, id.AgentRunID) (*ReflectionReceipt, error)
	AdmitReflection(context.Context, ReflectionRequest) (ReflectionReceipt, error)
}
