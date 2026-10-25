package optimization

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// ArtifactAdoptionRequest explicitly names a destination and selected artifacts.
// Key is a durable host idempotency key; changed content must conflict on retry.
type ArtifactAdoptionRequest struct {
	Key          string        `json:"key"`
	ExperimentID id.ID         `json:"experiment_id"`
	DecisionID   id.ID         `json:"decision_id"`
	TrialID      id.ID         `json:"trial_id"`
	Destination  ContentRef    `json:"destination"`
	Policy       ContentRef    `json:"policy"`
	Artifacts    []ArtifactRef `json:"artifacts"`
}

// ArtifactAdoption binds the authorized caller and exact scope to host effects.
type ArtifactAdoption struct {
	Request       ArtifactAdoptionRequest `json:"request"`
	Actor         IdentityRef             `json:"actor"`
	Scope         cortex.Scope            `json:"scope"`
	RequestDigest string                  `json:"request_digest"`
}

// ArtifactAdoptionReceipt is the host's durable commit evidence, not a trial run.
type ArtifactAdoptionReceipt struct {
	ID            string     `json:"id"`
	RequestDigest string     `json:"request_digest"`
	Evidence      ContentRef `json:"evidence"`
	AdoptedAt     time.Time  `json:"adopted_at"`
}

// ArtifactAdopter enforces current destination grants and the pinned host policy.
// It must read exact artifact content, persist key/digest/receipt atomically with
// its effects, reconcile retries, and report unknown outcomes without replay.
type ArtifactAdopter interface {
	AdoptArtifacts(context.Context, ArtifactAdoption) (ArtifactAdoptionReceipt, error)
}

// WithArtifactAdopter binds the explicit host commit operation for task artifacts.
func WithArtifactAdopter(adopter ArtifactAdopter) Option {
	return func(o *Optimizer) error {
		if adopter == nil {
			return ErrInvalidSpec
		}
		o.artifactAdopter = adopter
		return nil
	}
}

// AdoptArtifacts verifies selected evidence and current authority before asking
// the host to commit artifacts. It never reruns a trial or retries host effects.
func (o *Optimizer) AdoptArtifacts(ctx context.Context, request ArtifactAdoptionRequest) (ArtifactAdoptionReceipt, error) {
	var empty ArtifactAdoptionReceipt
	actor, err := o.caller(ctx)
	if err != nil {
		return empty, err
	}
	resolved, done, err := o.meter.authorizeAction(ctx, request.ExperimentID, ActionAdoptArtifacts)
	if err != nil {
		return empty, err
	}
	defer done()
	if o.artifactAdopter == nil || !ValidOpaqueKey(request.Key) || request.Destination.Validate() != nil || request.Policy.Validate() != nil || len(request.Artifacts) == 0 {
		return empty, ErrUnavailable
	}
	detached, err := copyModelJSON(&request, 16<<20)
	if err != nil {
		return empty, err
	}
	request = *detached
	result, err := o.GetTaskResult(ctx, request.ExperimentID)
	if err != nil {
		return empty, err
	}
	experiment, err := o.config.Repository.GetExperiment(resolved, request.ExperimentID)
	if err != nil {
		return empty, err
	}
	if experiment.Spec.Task == nil || experiment.Spec.Task.AdoptionPolicy == nil || *experiment.Spec.Task.AdoptionPolicy != request.Policy || result.Decision.ID != request.DecisionID || result.Trial.ID != request.TrialID {
		return empty, ErrPolicyDenied
	}
	bindings := make(map[string]bool, len(result.Artifacts))
	for _, ref := range result.Artifacts {
		key, bindingErr := ref.BindingDigest()
		if bindingErr != nil {
			return empty, bindingErr
		}
		bindings[key] = true
	}
	type keyedArtifact struct {
		ref ArtifactRef
		key string
	}
	ordered := make([]keyedArtifact, 0, len(request.Artifacts))
	for _, ref := range request.Artifacts {
		key, bindingErr := ref.BindingDigest()
		if bindingErr != nil || !bindings[key] {
			return empty, ErrIncompleteEvidence
		}
		delete(bindings, key)
		ordered = append(ordered, keyedArtifact{ref: ref, key: key})
	}
	slices.SortFunc(ordered, func(a, b keyedArtifact) int {
		return strings.Compare(a.key, b.key)
	})
	for i, value := range ordered {
		request.Artifacts[i] = value.ref
	}
	adoption := ArtifactAdoption{Request: request, Actor: actor, Scope: copyScope(experiment.Spec.Scope)}
	body, err := canonical.Marshal(adoption)
	if err != nil {
		return empty, err
	}
	adoption.RequestDigest = digestBytes(body)
	_, recheck, err := o.meter.authorizeAction(ctx, request.ExperimentID, ActionAdoptArtifacts)
	if err != nil {
		return empty, err
	}
	recheck()
	if evidenceErr := o.checkTaskEvidence(resolved, experiment, result.Evidence); evidenceErr != nil {
		return empty, evidenceErr
	}
	receipt, err := o.artifactAdopter.AdoptArtifacts(copyScopeContext(ctx), adoption)
	if err != nil {
		return empty, err
	}
	if !ValidOpaqueKey(receipt.ID) || receipt.RequestDigest != adoption.RequestDigest || receipt.Evidence.Validate() != nil || receipt.AdoptedAt.IsZero() || receipt.AdoptedAt.After(time.Now()) {
		return empty, ErrIncompleteEvidence
	}
	return receipt, nil
}
