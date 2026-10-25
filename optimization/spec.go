package optimization

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// Mode controls how discovery candidates share their baseline.
type Mode string

const (
	ModeSequential Mode = "sequential"
	ModeParallel   Mode = "parallel"
	ModeTaskLocal  Mode = "task_local"
)

// Split keeps controller-visible discovery separate from held-out validation.
type Split string

const (
	SplitDiscovery  Split = "discovery"
	SplitValidation Split = "validation"
)

// VersionRef binds a stored immutable version to the expected content.
type VersionRef struct {
	ID     id.ID  `json:"id"`
	Digest string `json:"digest"`
}

// ContentRef identifies a frozen external resource without carrying credentials.
type ContentRef struct {
	Ref      string `json:"ref"`
	Revision string `json:"revision"`
	Digest   string `json:"digest"`
}

// Validate checks the complete identity of a frozen external resource.
func (ref ContentRef) Validate() error { return validateContentRef(ref) }

// IdentityRef names a host subject whose current permissions workers must resolve.
type IdentityRef struct {
	Subject string `json:"subject"`
}

// TaskCase freezes input and environment identity. Private answers belong to the
// evaluator's plan and must never be placed in this controller-visible record.
type TaskCase struct {
	ID                  string       `json:"id"`
	Input               string       `json:"input"`
	EnvironmentSnapshot ContentRef   `json:"environment_snapshot"`
	Context             []ContentRef `json:"context"`
	DatasetItem         *ContentRef  `json:"dataset_item,omitempty"`
	Split               Split        `json:"split"`
	InputDigest         string       `json:"input_digest"`
	Digest              string       `json:"digest"`
}

// Limits bounds scheduling and each provider dispatch. Durations use nanoseconds
// in JSON. MaxCandidates includes every composed candidate across all rounds.
type Limits struct {
	MaxRounds         int           `json:"max_rounds"`
	MaxCandidates     int           `json:"max_candidates"`
	MaxTrials         int           `json:"max_trials"`
	Concurrency       int           `json:"concurrency"`
	ExperimentTimeout time.Duration `json:"experiment_timeout"`
	TrialTimeout      time.Duration `json:"trial_timeout"`
	MaxSteps          int           `json:"max_steps"`
	MaxOutputTokens   int           `json:"max_output_tokens"`
	Repetitions       int           `json:"repetitions"`
}

// DefaultLimits returns explicit scheduling defaults. Hosts can tighten them
// before freezing a spec; no zero field is interpreted as unlimited.
func DefaultLimits() Limits {
	return Limits{MaxRounds: 2, MaxCandidates: 6, MaxTrials: 500, Concurrency: 2,
		ExperimentTimeout: 30 * time.Minute, TrialTimeout: 2 * time.Minute,
		MaxSteps: 12, MaxOutputTokens: 4096, Repetitions: 3}
}

// Budget separates token-only execution from a priced monetary ceiling.
type Budget struct {
	MaxTokens       int64  `json:"max_tokens"`
	MaxUSDMicros    int64  `json:"max_usd_micros"`
	TokenOnly       bool   `json:"token_only"`
	PricingRevision string `json:"pricing_revision,omitempty"`
}

// Objective freezes selection thresholds. Insufficient distinct validation
// cases produce exploratory evidence, not a relaxed confidence requirement.
type Objective struct {
	MinImprovement         float64  `json:"min_improvement"`
	MaxProtectedRegression float64  `json:"max_protected_regression"`
	ProtectedCriteria      []string `json:"protected_criteria"`
	RequiredCriteria       []string `json:"required_criteria"`
	MinValidationCases     int      `json:"min_validation_cases"`
	Confidence             float64  `json:"confidence"`
	BootstrapSamples       int      `json:"bootstrap_samples"`
	BootstrapSeed          uint64   `json:"bootstrap_seed"`
	MaxCostRatio           *float64 `json:"max_cost_ratio,omitempty"`
	MaxLatencyRatio        *float64 `json:"max_latency_ratio,omitempty"`
	RequireReproducibility bool     `json:"require_reproducibility"`
}

// DefaultObjective returns the initial paired quality selection policy.
func DefaultObjective() Objective {
	return Objective{MinImprovement: .02, MaxProtectedRegression: .01,
		MinValidationCases: 20, Confidence: .95, BootstrapSamples: 2000,
		RequireReproducibility: true}
}

// ExperimentSpec is frozen before work begins. Host authorization must still
// validate its requested limits and references before persistence or dispatch.
type ExperimentSpec struct {
	Task          *TaskPolicy  `json:"task,omitempty"`
	SchemaVersion int          `json:"schema_version"`
	Scope         cortex.Scope `json:"scope"`
	AgentID       id.AgentID   `json:"agent_id"`
	Baseline      VersionRef   `json:"baseline"`
	Controller    VersionRef   `json:"controller"`
	Mode          Mode         `json:"mode"`
	Tasks         []TaskCase   `json:"tasks"`
	Evaluation    ContentRef   `json:"evaluation"`
	Objective     Objective    `json:"objective"`
	Limits        Limits       `json:"limits"`
	Budget        Budget       `json:"budget"`
	LessonCutoff  time.Time    `json:"lesson_cutoff"`
	Requester     IdentityRef  `json:"requester"`
}

// TrialPlan reserves final validation capacity before discovery is scheduled.
type TrialPlan struct {
	DiscoveryTrials         int `json:"discovery_trials"`
	ValidationTrials        int `json:"validation_trials"`
	TotalTrials             int `json:"total_trials"`
	DistinctValidationCases int `json:"distinct_validation_cases"`
}

// SealTaskCase detaches context and hashes original execution content. Input
// allocation identity normalizes line endings and surrounding whitespace only.
func SealTaskCase(task TaskCase) (TaskCase, error) {
	if strings.TrimSpace(task.ID) == "" || !utf8.ValidString(task.Input) || strings.TrimSpace(task.Input) == "" {
		return TaskCase{}, fmt.Errorf("%w: invalid task identity or input", ErrInvalidSpec)
	}
	if task.Split != SplitDiscovery && task.Split != SplitValidation {
		return TaskCase{}, fmt.Errorf("%w: invalid task split", ErrInvalidSpec)
	}
	refs := append([]ContentRef{task.EnvironmentSnapshot}, task.Context...)
	if task.DatasetItem != nil {
		refs = append(refs, *task.DatasetItem)
	}
	for _, ref := range refs {
		if err := validateContentRef(ref); err != nil {
			return TaskCase{}, err
		}
	}
	input := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(task.Input, "\r\n", "\n"), "\r", "\n"))
	task.InputDigest = digestBytes([]byte(input))
	task.Digest = ""
	data, err := canonical.Marshal(task)
	if err != nil {
		return TaskCase{}, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	var detached TaskCase
	if err = canonical.Decode(data, &detached); err != nil {
		return TaskCase{}, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	detached.Digest = digestBytes(data)
	return detached, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateContentRef(ref ContentRef) error {
	if strings.TrimSpace(ref.Ref) == "" || strings.TrimSpace(ref.Revision) == "" || !validDigest(ref.Digest) {
		return fmt.Errorf("%w: incomplete frozen content reference", ErrInvalidSpec)
	}
	return nil
}

// DecodeSpec rejects duplicate or unknown JSON members before validation.
func DecodeSpec(data []byte) (ExperimentSpec, error) {
	var spec ExperimentSpec
	if len(data) > 16<<20 {
		return spec, fmt.Errorf("%w: oversized experiment", ErrInvalidSpec)
	}
	if err := canonical.Decode(data, &spec); err != nil {
		return ExperimentSpec{}, fmt.Errorf("%w: %w", ErrInvalidSpec, err)
	}
	if err := ValidateSpec(spec); err != nil {
		return ExperimentSpec{}, err
	}
	return spec, nil
}

// ValidateSpec checks immutable references, task separation and finite bounds.
// It does not establish external dependency readiness or grant authorization.
func ValidateSpec(spec ExperimentSpec) error {
	_, err := Preflight(spec)
	return err
}

// Preflight counts the maximum permitted discovery and final paired trials,
// including baseline and composed candidates, with overflow-safe arithmetic.
func Preflight(spec ExperimentSpec) (TrialPlan, error) {
	var plan TrialPlan
	if err := validateSpecFields(spec); err != nil {
		return plan, err
	}
	ids, inputs := map[string]bool{}, map[string]Split{}
	discovery, validation := 0, 0
	for _, task := range spec.Tasks {
		sealed, err := SealTaskCase(task)
		if err != nil {
			return plan, err
		}
		if sealed.Digest != task.Digest || sealed.InputDigest != task.InputDigest || ids[task.ID] {
			return plan, fmt.Errorf("%w: changed or duplicate task", ErrInvalidSpec)
		}
		if _, exists := inputs[task.InputDigest]; exists {
			return plan, fmt.Errorf("%w: duplicate normalized input", ErrInvalidSpec)
		}
		ids[task.ID], inputs[task.InputDigest] = true, task.Split
		if task.Split == SplitDiscovery {
			discovery++
		} else {
			validation++
		}
	}
	if discovery == 0 || (spec.Mode != ModeTaskLocal && validation == 0) || (spec.Mode == ModeTaskLocal && (discovery != 1 || validation != 0)) {
		return plan, fmt.Errorf("%w: task split does not match mode", ErrInvalidSpec)
	}
	plan.DistinctValidationCases = validation
	var err error
	plan.ValidationTrials, err = trialProduct(validation, 2, spec.Limits.Repetitions, spec.Limits.MaxTrials)
	if err != nil {
		return plan, err
	}
	if spec.Limits.MaxCandidates == math.MaxInt {
		return plan, ErrBudgetExceeded
	}
	plan.DiscoveryTrials, err = trialProduct(discovery, spec.Limits.MaxCandidates+1, spec.Limits.Repetitions, spec.Limits.MaxTrials-plan.ValidationTrials)
	if err != nil {
		return plan, err
	}
	plan.TotalTrials = plan.ValidationTrials + plan.DiscoveryTrials
	return plan, nil
}

func trialProduct(cases, arms, repetitions, capTrials int) (int, error) {
	if cases == 0 {
		return 0, nil
	}
	if cases > capTrials/arms {
		return 0, ErrBudgetExceeded
	}
	value := cases * arms
	if value > capTrials/repetitions {
		return 0, ErrBudgetExceeded
	}
	return value * repetitions, nil
}

func validateSpecFields(spec ExperimentSpec) error {
	if spec.Mode == ModeTaskLocal {
		if spec.Task == nil || spec.Task.Deadline.IsZero() || ValidateTrialCapabilities(spec.Task.Isolation, spec.Objective.RequireReproducibility) != nil {
			return ErrUnavailable
		}
		if spec.Task.AdoptionPolicy != nil && spec.Task.AdoptionPolicy.Validate() != nil {
			return ErrInvalidSpec
		}
	} else if spec.Task != nil {
		return ErrInvalidSpec
	}
	if spec.SchemaVersion != 1 || strings.TrimSpace(spec.Requester.Subject) == "" {
		return fmt.Errorf("%w: invalid schema or requester", ErrInvalidSpec)
	}
	if _, err := ScopeKey(spec.Scope); err != nil {
		return err
	}
	if _, err := ParseID(spec.AgentID.String(), id.PrefixAgent); err != nil {
		return err
	}
	for _, ref := range []VersionRef{spec.Baseline, spec.Controller} {
		if _, err := ParseID(ref.ID.String(), PrefixVersion); err != nil {
			return err
		}
		if !validDigest(ref.Digest) {
			return fmt.Errorf("%w: invalid version digest", ErrInvalidSpec)
		}
	}
	if err := validateContentRef(spec.Evaluation); err != nil {
		return err
	}
	if spec.Mode != ModeSequential && spec.Mode != ModeParallel && spec.Mode != ModeTaskLocal {
		return fmt.Errorf("%w: invalid mode", ErrInvalidSpec)
	}
	l := spec.Limits
	if l.MaxRounds <= 0 || l.MaxCandidates <= 0 || l.MaxTrials <= 0 || l.Concurrency <= 0 || l.Concurrency > l.MaxTrials || l.ExperimentTimeout <= 0 || l.TrialTimeout <= 0 || l.TrialTimeout > l.ExperimentTimeout || l.MaxSteps <= 0 || l.MaxOutputTokens <= 0 || l.Repetitions <= 0 {
		return fmt.Errorf("%w: invalid execution limits", ErrInvalidSpec)
	}
	b := spec.Budget
	if b.MaxTokens <= 0 || b.MaxUSDMicros < 0 || (b.TokenOnly && (b.MaxUSDMicros != 0 || b.PricingRevision != "")) || (!b.TokenOnly && (b.MaxUSDMicros == 0 || strings.TrimSpace(b.PricingRevision) == "")) {
		return fmt.Errorf("%w: token and money accounting mode inconsistent", ErrInvalidSpec)
	}
	o := spec.Objective
	if !finite(o.MinImprovement) || o.MinImprovement < 0 || o.MinImprovement > 1 || !finite(o.MaxProtectedRegression) || o.MaxProtectedRegression < 0 || o.MaxProtectedRegression > 1 || !finite(o.Confidence) || o.Confidence <= 0 || o.Confidence >= 1 || o.MinValidationCases < 1 || o.BootstrapSamples < 1 {
		return fmt.Errorf("%w: invalid objective", ErrInvalidSpec)
	}
	for _, ratio := range []*float64{o.MaxCostRatio, o.MaxLatencyRatio} {
		if ratio != nil && (!finite(*ratio) || *ratio <= 0) {
			return fmt.Errorf("%w: invalid objective ratio", ErrInvalidSpec)
		}
	}
	if o.MaxCostRatio != nil && b.TokenOnly {
		return fmt.Errorf("%w: cost objective needs priced accounting", ErrInvalidSpec)
	}
	if err := uniqueNames(o.ProtectedCriteria); err != nil {
		return err
	}
	return uniqueNames(o.RequiredCriteria)
}
