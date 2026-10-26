package optimization

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Capability names a separately checked host integration.
type Capability string

const (
	CapabilityStorage           Capability = "storage"
	CapabilityDurableStorage    Capability = "durable_storage"
	CapabilityWorker            Capability = "worker"
	CapabilityController        Capability = "controller"
	CapabilityEnvironment       Capability = "trial_environment"
	CapabilityEvaluation        Capability = "evaluation"
	CapabilityAccounting        Capability = "accounting"
	CapabilityScopeMapping      Capability = "scope_mapping"
	CapabilityLessons           Capability = "lessons"
	CapabilityPublication       Capability = "publication"
	CapabilityProductionRouting Capability = "production_routing"
	CapabilityControllerRouting Capability = "controller_routing"
)

// CapabilityNames returns the bounded readiness vocabulary in display order.
func CapabilityNames() []Capability {
	return []Capability{CapabilityStorage, CapabilityDurableStorage, CapabilityWorker, CapabilityController, CapabilityEnvironment, CapabilityEvaluation, CapabilityAccounting, CapabilityScopeMapping, CapabilityLessons, CapabilityPublication, CapabilityProductionRouting, CapabilityControllerRouting}
}

// ReadinessState distinguishes missing, unverified and denied integrations.
type ReadinessState string

const (
	ReadinessReady       ReadinessState = "ready"
	ReadinessUnavailable ReadinessState = "unavailable"
	ReadinessUnknown     ReadinessState = "unknown"
	ReadinessDenied      ReadinessState = "denied"
)

// CapabilityTarget binds host checks to the exact frozen experiment. A nil
// ExperimentID requests scope-level diagnostics and cannot establish trial readiness.
type CapabilityTarget struct {
	Scope        cortex.Scope `json:"scope"`
	ExperimentID id.ID        `json:"experiment_id,omitempty"`
	AgentID      id.AgentID   `json:"agent_id,omitempty"`
	Mode         Mode         `json:"mode,omitempty"`
	Baseline     VersionRef   `json:"baseline"`
	Controller   VersionRef   `json:"controller"`
	Evaluation   ContentRef   `json:"evaluation"`
}

// HostCapabilities contains current host observations, never authorization grants.
// Missing checks remain unknown. Pricing must name its revision when qualified.
type HostCapabilities struct {
	Target          CapabilityTarget              `json:"target"`
	Checks          map[Capability]ReadinessState `json:"checks"`
	Environment     IsolationCapabilities         `json:"environment"`
	PricingQuality  UsageQuality                  `json:"pricing_quality"`
	PricingRevision string                        `json:"pricing_revision,omitempty"`
}

// CapabilityProbe inspects installed workers, mapped providers and environment
// guarantees without starting work or writing remote state. Implementations must
// honor cancellation and current host grants. Checks apply only to Target.
type CapabilityProbe interface {
	Inspect(context.Context, CapabilityTarget) (HostCapabilities, error)
}

// CapabilitySnapshot separates readiness from current permission. TrialsReady
// concerns the selected frozen experiment's prerequisites, not its lifecycle,
// remaining budget or a promise that a later command will be authorized.
type CapabilitySnapshot struct {
	HostCapabilities
	Permissions        map[Action]bool `json:"permissions"`
	EditableDimensions []EditKind      `json:"editable_dimensions"`
	TrialsReady        bool            `json:"trials_ready"`
	ObservedAt         time.Time       `json:"observed_at"`
}

// Capabilities reads current scoped diagnostics. It never starts a worker,
// executes a trial, or treats an absent host probe as an empty ready catalog.
func (o *Optimizer) Capabilities(ctx context.Context, experimentID id.ID) (CapabilitySnapshot, error) {
	var empty CapabilitySnapshot
	if err := o.authorizeRead(ctx, Resource{Kind: "capabilities"}); err != nil {
		return empty, err
	}
	actor, err := o.caller(ctx)
	if err != nil {
		return empty, err
	}
	target := CapabilityTarget{Scope: copyScope(cortex.ScopeFromContext(ctx))}
	var experiment Experiment
	if !experimentID.IsNil() {
		experiment, err = o.readExperiment(ctx, experimentID)
		if err != nil {
			return empty, err
		}
		target.ExperimentID = experiment.ID
		target.AgentID = experiment.Spec.AgentID
		target.Mode = experiment.Spec.Mode
		target.Baseline = experiment.Spec.Baseline
		target.Controller = experiment.Spec.Controller
		target.Evaluation = experiment.EvaluationPlan.Ref
	}
	result := CapabilitySnapshot{HostCapabilities: HostCapabilities{Target: target, Checks: map[Capability]ReadinessState{}, PricingQuality: UsageUnknown}, EditableDimensions: []EditKind{}}
	for _, name := range CapabilityNames() {
		result.Checks[name] = ReadinessUnavailable
	}
	if o.config.Capabilities != nil {
		probeCtx, cancel := context.WithTimeout(copyScopeContext(ctx), 5*time.Second)
		probeTarget := target
		probeTarget.Scope = copyScope(target.Scope)
		report, probeErr := o.config.Capabilities.Inspect(probeCtx, probeTarget)
		contextErr := probeCtx.Err()
		cancel()
		if contextErr != nil {
			return empty, contextErr
		}
		if probeErr != nil {
			return empty, capabilityError(probeErr)
		}
		if !reflect.DeepEqual(report.Target, target) {
			return empty, ErrPolicyDenied
		}
		if validationErr := validateCapabilityReport(report); validationErr != nil {
			return empty, validationErr
		}
		for _, name := range CapabilityNames() {
			value := report.Checks[name]
			if value == "" {
				value = ReadinessUnknown
			}
			result.Checks[name] = value
		}
		result.Environment = report.Environment
		result.PricingQuality = report.PricingQuality
		if result.PricingQuality == "" {
			result.PricingQuality = UsageUnknown
		}
		if result.PricingQuality != UsageUnknown {
			result.PricingRevision = report.PricingRevision
		}
	}
	result.Checks[CapabilityStorage] = ReadinessReady
	if err = o.config.Repository.Ping(copyScopeContext(ctx)); err != nil {
		result.Checks[CapabilityStorage] = ReadinessUnavailable
	}
	if o.activeEngine == nil || o.config.Compatibility == nil || o.config.ArtifactVerifier == nil {
		result.Checks[CapabilityProductionRouting] = ReadinessUnavailable
	}
	if o.config.ControllerCompatibility == nil || o.config.ArtifactVerifier == nil {
		result.Checks[CapabilityControllerRouting] = ReadinessUnavailable
	}
	if o.config.Lessons == nil {
		result.Checks[CapabilityLessons] = ReadinessUnavailable
		result.Checks[CapabilityPublication] = ReadinessUnavailable
	}
	if !experimentID.IsNil() {
		if validateTrialEnvironment(experiment.Spec, result.Environment) != nil && result.Checks[CapabilityEnvironment] == ReadinessReady {
			result.Checks[CapabilityEnvironment] = ReadinessUnavailable
		}
		version, readErr := o.GetVersion(ctx, experiment.Spec.Baseline.ID)
		if readErr != nil {
			return empty, readErr
		}
		if (VersionRef{ID: version.ID, Digest: version.Digest}) != experiment.Spec.Baseline {
			return empty, ErrStaleVersion
		}
		result.EditableDimensions = editableDimensions(version.Policy)
		result.TrialsReady = experiment.Spec.Budget.TokenOnly || ((result.PricingQuality == UsageKnown || result.PricingQuality == UsageEstimated) && result.PricingRevision == experiment.Spec.Budget.PricingRevision)
		for _, name := range []Capability{CapabilityStorage, CapabilityDurableStorage, CapabilityWorker, CapabilityController, CapabilityEnvironment, CapabilityEvaluation, CapabilityAccounting, CapabilityScopeMapping} {
			result.TrialsReady = result.TrialsReady && result.Checks[name] == ReadinessReady
		}
	}
	// Provider callbacks can take time or revoke authority. Resolve permissions
	// after probing and recheck the reader and actor before exposing the result.
	result.Permissions, err = o.capabilityPermissions(ctx, target)
	if err != nil {
		return empty, err
	}
	if authErr := o.authorizeRead(ctx, Resource{Kind: "capabilities"}); authErr != nil {
		return empty, authErr
	}
	if !experimentID.IsNil() {
		if _, err = o.readExperiment(ctx, experimentID); err != nil {
			return empty, err
		}
	}
	current, err := o.caller(ctx)
	if err != nil {
		return empty, err
	}
	if actor != current {
		return empty, ErrPolicyDenied
	}
	result.ObservedAt = time.Now().UTC()
	return result, nil
}

func validateCapabilityReport(report HostCapabilities) error {
	names := CapabilityNames()
	for name, state := range report.Checks {
		if !slices.Contains(names, name) {
			return ErrInvalidSpec
		}
		switch state {
		case ReadinessReady, ReadinessUnavailable, ReadinessUnknown, ReadinessDenied:
		default:
			return ErrInvalidSpec
		}
	}
	if len(report.PricingRevision) > 256 || (Usage{MoneyQuality: report.PricingQuality, PricingRevision: report.PricingRevision}).Validate() != nil {
		return ErrInvalidSpec
	}
	return nil
}

func capabilityError(err error) error {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded, ErrPolicyDenied} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	return ErrUnavailable
}

func (o *Optimizer) capabilityPermissions(ctx context.Context, target CapabilityTarget) (map[Action]bool, error) {
	permissions := map[Action]bool{}
	for _, action := range []Action{ActionRead, ActionCreate, ActionStart, ActionCancel, ActionResume, ActionRetain, ActionActivate, ActionRollback, ActionDeactivate, ActionRetryPublication, ActionRevokeLesson, ActionReadArtifact} {
		resource := Resource{Scope: copyScope(target.Scope), AgentID: target.AgentID, ID: target.ExperimentID, Kind: "experiment"}
		// Route permissions use the same resource shape as activation commands.
		if action == ActionActivate || action == ActionRollback || action == ActionDeactivate {
			resource.ID = id.Nil
			resource.Kind = "activation"
		}
		if action == ActionRetryPublication || action == ActionRevokeLesson {
			resource.ID = id.Nil
			resource.AgentID = id.Nil
			resource.Kind = "lesson"
		}
		if action == ActionReadArtifact {
			resource.Kind = "artifact"
		}
		err := o.config.Authorizer.Authorize(copyScopeContext(ctx), action, resource)
		if err != nil && !errors.Is(err, ErrPolicyDenied) {
			return nil, capabilityError(err)
		}
		permissions[action] = err == nil
	}
	return permissions, nil
}

func editableDimensions(policy EditPolicy) []EditKind {
	dimensions := []EditKind{}
	if len(policy.MutableSections) > 0 {
		dimensions = append(dimensions, EditSectionBody)
	}
	if len(policy.MutableToolDescriptions) > 0 {
		dimensions = append(dimensions, EditToolDescription)
	}
	if policy.AllowToolSubset {
		dimensions = append(dimensions, EditToolSubset)
	}
	if policy.TemperatureMin != nil {
		dimensions = append(dimensions, EditTemperature)
	}
	if len(policy.OrchestrationPresets) > 0 {
		dimensions = append(dimensions, EditOrchestrationPreset)
		if slices.ContainsFunc(policy.OrchestrationPresets, func(p OrchestrationPreset) bool {
			for _, sections := range p.Guidance {
				if len(sections) > 0 {
					return true
				}
			}
			return false
		}) {
			dimensions = append(dimensions, EditParticipantGuidance)
		}
	}
	return dimensions
}
