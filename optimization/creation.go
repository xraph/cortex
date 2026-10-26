package optimization

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// CreationSelection contains public catalog references and requested bounds.
// Prepared snapshots, rubrics, policies and caller identities are host-owned.
// Omitted bounds use the selected catalog revision's defaults.
type CreationSelection struct {
	Agent       ContentRef       `json:"agent"`
	Evaluation  ContentRef       `json:"evaluation"`
	Controller  ContentRef       `json:"controller"`
	Environment ContentRef       `json:"environment"`
	Mode        Mode             `json:"mode,omitempty"`
	Limits      *Limits          `json:"limits,omitempty"`
	Budget      *Budget          `json:"budget,omitempty"`
	Objective   *Objective       `json:"objective,omitempty"`
	TaskSplits  map[string]Split `json:"task_splits,omitempty"`
}

// CreationTarget assigns identity before host preparation so nested plans can
// bind their parent. Catalog implementations must freeze exactly Selection.
type CreationTarget struct {
	Scope        cortex.Scope
	Actor        IdentityRef
	ExperimentID id.ID
	Selection    CreationSelection
}

// CreationCatalog authorizes selected revisions independently of preparation.
// Authorize must enforce current access to every selected source, including
// mapped evaluation access. Prepare returns immutable host-approved content and
// enforces host bounds. It must not execute trials or start background work.
type CreationCatalog interface {
	Authorize(context.Context, CreationSelection) error
	Prepare(context.Context, CreationTarget) (ExperimentBundle, error)
}

// CreationRequest binds transport idempotency to public catalog choices.
type CreationRequest struct {
	Selection      CreationSelection `json:"selection"`
	IdempotencyKey string            `json:"idempotency_key"`
}

// CreationReceipt retains the accepted request independently of mutable progress.
// Key is a digest namespace, never the caller's raw transport key.
type CreationReceipt struct {
	Key          string            `json:"key"`
	Scope        cortex.Scope      `json:"scope"`
	Actor        IdentityRef       `json:"actor"`
	Selection    CreationSelection `json:"selection"`
	InputDigest  string            `json:"input_digest"`
	SpecDigest   string            `json:"spec_digest"`
	ExperimentID id.ID             `json:"experiment_id"`
	CreatedAt    time.Time         `json:"created_at"`
}

// CreationCommand atomically persists a host-frozen draft and its input receipt.
type CreationCommand struct {
	Key       string
	Actor     IdentityRef
	Selection CreationSelection
	Bundle    ExperimentBundle
}

func (selection CreationSelection) validate() error {
	for _, ref := range []ContentRef{selection.Agent, selection.Evaluation, selection.Controller, selection.Environment} {
		if ref.Validate() != nil || len(ref.Ref) > 1024 || len(ref.Revision) > 256 {
			return ErrInvalidSpec
		}
	}
	if _, err := id.ParseAgentID(selection.Agent.Ref); err != nil {
		return ErrInvalidSpec
	}
	if _, err := ParseID(selection.Controller.Ref, PrefixVersion); err != nil {
		return ErrInvalidSpec
	}
	if selection.Mode != "" && selection.Mode != ModeSequential && selection.Mode != ModeParallel && selection.Mode != ModeTaskLocal {
		return ErrInvalidSpec
	}
	if len(selection.TaskSplits) > 10000 {
		return ErrInvalidSpec
	}
	for key, split := range selection.TaskSplits {
		if !ValidOpaqueKey(key) || (split != SplitDiscovery && split != SplitValidation) {
			return ErrInvalidSpec
		}
	}
	return nil
}

func creationFingerprint(scope cortex.Scope, actor IdentityRef, selection CreationSelection) (string, error) {
	if _, err := ScopeKey(scope); err != nil {
		return "", err
	}
	if !ValidOpaqueKey(actor.Subject) || selection.validate() != nil {
		return "", ErrInvalidSpec
	}
	payload, err := canonical.Marshal(struct {
		Scope     cortex.Scope
		Actor     IdentityRef
		Selection CreationSelection
	}{scope, actor, selection})
	if err != nil || len(payload) > 64<<10 {
		return "", ErrInvalidSpec
	}
	return digestBytes(payload), nil
}

// SealCreationReceipt validates identity and computes the canonical request digest.
func SealCreationReceipt(receipt CreationReceipt) (CreationReceipt, error) {
	if !strings.HasPrefix(receipt.Key, "operator/") || !validDigest(strings.TrimPrefix(receipt.Key, "operator/")) || receipt.CreatedAt.IsZero() {
		return CreationReceipt{}, ErrInvalidSpec
	}
	if receipt.SpecDigest != "" && !validDigest(receipt.SpecDigest) {
		return CreationReceipt{}, ErrInvalidSpec
	}
	if _, err := ParseID(receipt.ExperimentID.String(), PrefixExperiment); err != nil {
		return CreationReceipt{}, err
	}
	digest, err := creationFingerprint(receipt.Scope, receipt.Actor, receipt.Selection)
	if err != nil {
		return CreationReceipt{}, err
	}
	receipt.InputDigest = digest
	copyReceipt, err := copyModelJSON(&receipt, 64<<10)
	if err != nil {
		return CreationReceipt{}, err
	}
	return *copyReceipt, nil
}

// ValidateCreationBundle verifies the provider honored explicit public choices.
// Catalog-specific bounds and source revisions remain the catalog's authority.
func ValidateCreationBundle(selection CreationSelection, bundle ExperimentBundle) error {
	if err := selection.validate(); err != nil {
		return err
	}
	spec := bundle.Experiment.Spec
	if spec.AgentID.String() != selection.Agent.Ref || spec.Evaluation != selection.Evaluation || spec.Controller.ID.String() != selection.Controller.Ref || spec.Controller.Digest != selection.Controller.Digest {
		return ErrConflict
	}
	if selection.Mode != "" && selection.Mode != spec.Mode {
		return ErrConflict
	}
	for _, pair := range []struct {
		requested, actual any
		present           bool
	}{
		{selection.Limits, spec.Limits, selection.Limits != nil},
		{selection.Budget, spec.Budget, selection.Budget != nil},
		{selection.Objective, spec.Objective, selection.Objective != nil},
	} {
		if !pair.present {
			continue
		}
		requested, err := canonical.Marshal(pair.requested)
		if err != nil {
			return ErrInvalidSpec
		}
		actual, err := canonical.Marshal(pair.actual)
		if err != nil {
			return ErrInvalidSpec
		}
		if !bytes.Equal(requested, actual) {
			return ErrConflict
		}
	}
	if len(selection.TaskSplits) > 0 {
		if len(selection.TaskSplits) != len(spec.Tasks) {
			return ErrConflict
		}
		for _, task := range spec.Tasks {
			if selection.TaskSplits[task.ID] != task.Split {
				return ErrConflict
			}
		}
	}
	return nil
}

// CreateCommand resolves host catalog choices once and saves a durable receipt.
// Identical retries recheck current catalog and creation grants, then return the
// original experiment's current state without preparing from live sources again.
func (o *Optimizer) CreateCommand(ctx context.Context, request CreationRequest) (Experiment, error) {
	var empty Experiment
	if err := allowOptimizationInvocation(ctx); err != nil {
		return empty, err
	}
	selection, err := copyModelJSON(&request.Selection, 64<<10)
	if err != nil {
		return empty, err
	}
	if validationErr := selection.validate(); validationErr != nil {
		return empty, validationErr
	}
	key, err := operatorCommandKey(request.IdempotencyKey)
	if err != nil {
		return empty, err
	}
	actor, err := o.caller(ctx)
	if err != nil {
		return empty, err
	}
	scope := copyScope(cortex.ScopeFromContext(ctx))
	digest, err := creationFingerprint(scope, actor, *selection)
	if err != nil {
		return empty, err
	}
	if grantErr := o.authorizeCatalog(ctx, *selection); grantErr != nil {
		return empty, grantErr
	}
	prior, err := o.config.Repository.GetCreationReceipt(copyScopeContext(ctx), key)
	if err == nil {
		return o.creationResult(ctx, prior, key, digest, actor)
	}
	if !errors.Is(err, ErrNotFound) {
		return empty, err
	}
	target := CreationTarget{Scope: scope, Actor: actor, ExperimentID: id.New(PrefixExperiment), Selection: *selection}
	if grantErr := o.authorizeCreationTarget(ctx, target); grantErr != nil {
		return empty, grantErr
	}
	// Detach both the context scope and the host argument. A provider cannot mutate
	// the identity or selection against which its returned bundle is checked.
	providerTarget, err := copyModelJSON(&target, 64<<10)
	if err != nil {
		return empty, err
	}
	bundle, err := o.config.CreationCatalog.Prepare(copyScopeContext(ctx), *providerTarget)
	if err != nil {
		return empty, err
	}
	frozen, err := o.creationBundle(ctx, bundle)
	if err != nil {
		return empty, err
	}
	if frozen.Experiment.ID != target.ExperimentID || frozen.Experiment.Spec.Scope.Canonical() != scope.Canonical() || frozen.Experiment.Spec.Requester != actor || frozen.ParentLease != nil || frozen.Experiment.Spec.Parent != nil {
		return empty, ErrPolicyDenied
	}
	if validationErr := ValidateCreationBundle(*selection, *frozen); validationErr != nil {
		return empty, validationErr
	}
	if grantErr := o.authorizeCatalog(ctx, *selection); grantErr != nil {
		return empty, grantErr
	}
	if grantErr := o.authorizeCreationTarget(ctx, target); grantErr != nil {
		return empty, grantErr
	}
	var receipt CreationReceipt
	err = o.config.Repository.Transact(ctx, func(tx Transaction) error {
		var writeErr error
		receipt, writeErr = tx.CreateExperimentCommand(CreationCommand{Key: key, Actor: actor, Selection: *selection, Bundle: *frozen})
		return writeErr
	})
	if err != nil {
		return empty, err
	}
	return o.creationResult(ctx, receipt, key, digest, actor)
}

func (o *Optimizer) authorizeCatalog(ctx context.Context, selection CreationSelection) error {
	if o.config.CreationCatalog == nil {
		return ErrUnavailable
	}
	detached, err := copyModelJSON(&selection, 64<<10)
	if err != nil {
		return err
	}
	return o.config.CreationCatalog.Authorize(copyScopeContext(ctx), *detached)
}
func (o *Optimizer) authorizeCreationTarget(ctx context.Context, target CreationTarget) error {
	current, err := o.caller(ctx)
	if err != nil {
		return err
	}
	if current != target.Actor || cortex.ScopeFromContext(ctx).Canonical() != target.Scope.Canonical() {
		return ErrPolicyDenied
	}
	agentID, err := id.ParseAgentID(target.Selection.Agent.Ref)
	if err != nil {
		return ErrInvalidSpec
	}
	return o.config.Authorizer.Authorize(copyScopeContext(ctx), ActionCreate, Resource{Scope: copyScope(target.Scope), AgentID: agentID, ID: target.ExperimentID, Kind: "experiment"})
}
func (o *Optimizer) creationResult(ctx context.Context, receipt CreationReceipt, key, digest string, actor IdentityRef) (Experiment, error) {
	var empty Experiment
	sealed, err := SealCreationReceipt(receipt)
	if err != nil || !reflect.DeepEqual(sealed, receipt) || receipt.Key != key || receipt.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return empty, ErrIncompleteEvidence
	}
	if receipt.InputDigest != digest || receipt.Actor != actor {
		return empty, ErrConflict
	}
	if grantErr := o.authorizeCreationTarget(ctx, CreationTarget{Scope: receipt.Scope, Actor: actor, ExperimentID: receipt.ExperimentID, Selection: receipt.Selection}); grantErr != nil {
		return empty, grantErr
	}
	value, err := o.config.Repository.GetExperiment(copyScopeContext(ctx), receipt.ExperimentID)
	if err != nil {
		return empty, err
	}
	if !validDigest(receipt.SpecDigest) || value.SpecDigest != receipt.SpecDigest || value.ID != receipt.ExperimentID || value.Spec.Scope.Canonical() != receipt.Scope.Canonical() || value.Spec.Requester != actor || value.Spec.AgentID.String() != receipt.Selection.Agent.Ref {
		return empty, ErrIncompleteEvidence
	}
	return value, nil
}
