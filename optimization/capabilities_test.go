package optimization_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type capabilityProbe func(context.Context, opt.CapabilityTarget) (opt.HostCapabilities, error)

func (f capabilityProbe) Inspect(ctx context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
	return f(ctx, target)
}

func TestCapabilitiesRequireCurrentScopedEvidence(t *testing.T) {
	repo, err := memory.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	bundle := storetest.Fixture(t, "capabilities")
	view, err := bundle.Versions[0].Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	bundle.Versions[0].Policy.MutableSections = []string{view.Sections[0].ID}
	bundle.Versions[0], err = opt.SealVersion(bundle.Versions[0])
	if err != nil {
		t.Fatal(err)
	}
	bundle.Experiment.Spec.Baseline = opt.VersionRef{ID: bundle.Versions[0].ID, Digest: bundle.Versions[0].Digest}
	bundle.Experiment.Spec.Budget = opt.Budget{MaxTokens: 1000, MaxUSDMicros: 1000000, PricingRevision: "fixture-prices/1"}
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	denied := map[opt.Action]bool{}
	actor := bundle.Experiment.Spec.Requester
	var callback func(context.Context, opt.CapabilityTarget) (opt.HostCapabilities, error)
	calls := 0
	probe := capabilityProbe(func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		calls++
		return callback(c, target)
	})
	config := opt.OptimizerConfig{Repository: repo, Authorizer: meterAuthorizer(func(_ context.Context, action opt.Action, resource opt.Resource) error {
		if resource.Scope.Canonical() != bundle.Experiment.Spec.Scope.Canonical() {
			return opt.ErrPolicyDenied
		}
		if denied[action] {
			return opt.ErrPolicyDenied
		}
		return nil
	}), Caller: callerIdentity(func(context.Context) (opt.IdentityRef, error) { return actor, nil }), IdentityResolver: meterResolver(func(c context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) { return c, nil }), Handler: phaseHandler(func(context.Context, opt.WorkItem) (opt.Transition, error) {
		return opt.Transition{}, opt.ErrUnavailable
	}), Worker: opt.DefaultWorkerConfig()}
	service, err := opt.NewOptimizer(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Create(ctx, bundle); err != nil {
		t.Fatal(err)
	}
	key := bundle.Experiment.ID
	unavailable, err := service.Capabilities(ctx, key)
	if err != nil || unavailable.TrialsReady || unavailable.Checks[opt.CapabilityEvaluation] != opt.ReadinessUnavailable {
		t.Fatalf("missing probe became ready: %+v %v", unavailable, err)
	}
	if len(unavailable.EditableDimensions) == 0 {
		t.Fatal("lost frozen edit policy")
	}
	config.Capabilities = probe
	service, err = opt.NewOptimizer(config)
	if err != nil {
		t.Fatal(err)
	}
	ready := func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		if cortex.ScopeFromContext(c).Canonical() != ctxScope(ctx) || target.ExperimentID != key || target.AgentID != bundle.Experiment.Spec.AgentID || target.Evaluation != bundle.Experiment.EvaluationPlan.Ref {
			t.Fatal("probe lost target binding")
		}
		checks := map[opt.Capability]opt.ReadinessState{}
		for _, check := range opt.CapabilityNames() {
			checks[check] = opt.ReadinessReady
		}
		return opt.HostCapabilities{Target: target, Checks: checks, PricingQuality: opt.UsageKnown, PricingRevision: "fixture-prices/1", Environment: opt.IsolationCapabilities{Isolated: true, Resettable: true, Recoverable: true, Stoppable: true, FrozenRetrieval: true, AtomicSources: true, DeterministicApproval: true}}, c.Err()
	}
	callback = ready
	result, err := service.Capabilities(ctx, key)
	if err != nil || !result.TrialsReady || !result.Permissions[opt.ActionStart] {
		t.Fatalf("qualified readiness: %+v %v", result, err)
	}
	for _, check := range []opt.Capability{opt.CapabilityProductionRouting, opt.CapabilityControllerRouting, opt.CapabilityLessons, opt.CapabilityPublication} {
		if result.Checks[check] != opt.ReadinessUnavailable {
			t.Fatalf("missing integration advertised %s", check)
		}
	}
	for _, check := range []opt.Capability{opt.CapabilityDurableStorage, opt.CapabilityWorker, opt.CapabilityController, opt.CapabilityEnvironment, opt.CapabilityEvaluation, opt.CapabilityAccounting, opt.CapabilityScopeMapping} {
		t.Run(string(check), func(t *testing.T) {
			callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
				r, e := ready(c, target)
				delete(r.Checks, check)
				return r, e
			}
			got, e := service.Capabilities(ctx, key)
			if e != nil || got.TrialsReady || got.Checks[check] != opt.ReadinessUnknown {
				t.Fatalf("missing evidence: %+v %v", got, e)
			}
		})
	}
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		r.PricingQuality = opt.UsageUnknown
		return r, e
	}
	if got, e := service.Capabilities(ctx, key); e != nil || got.TrialsReady {
		t.Fatalf("unknown pricing enabled trials: %+v %v", got, e)
	}
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		r.Environment.Recoverable = false
		return r, e
	}
	if got, e := service.Capabilities(ctx, key); e != nil || got.TrialsReady || got.Checks[opt.CapabilityEnvironment] != opt.ReadinessUnavailable {
		t.Fatalf("unrecoverable environment: %+v %v", got, e)
	}
	callback = ready
	denied[opt.ActionStart] = true
	if got, e := service.Capabilities(ctx, key); e != nil || got.Permissions[opt.ActionStart] {
		t.Fatalf("current action grant lost: %+v %v", got, e)
	}
	denied[opt.ActionRead] = true
	before := calls
	if _, err = service.Capabilities(ctx, key); !errors.Is(err, opt.ErrPolicyDenied) || calls != before {
		t.Fatalf("denied read reached probe: %v", err)
	}
	denied[opt.ActionRead] = false
	if _, err = service.Capabilities(context.Background(), key); !errors.Is(err, cortex.ErrNoScope) || calls != before {
		t.Fatalf("unscoped probe: %v", err)
	}
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "foreign"}}})
	if _, err = service.Capabilities(foreign, key); err == nil || calls != before {
		t.Fatalf("foreign probe: %v", err)
	}
	if _, err = service.Capabilities(ctx, id.NewAgentID()); !errors.Is(err, opt.ErrInvalidSpec) || calls != before {
		t.Fatalf("wrong ID kind: %v", err)
	}
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		r.Target.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "foreign"}}}
		return r, e
	}
	if _, err = service.Capabilities(ctx, key); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("foreign report: %v", err)
	}
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		r.Checks["provider-secret"] = opt.ReadinessReady
		return r, e
	}
	if _, err = service.Capabilities(ctx, key); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("unknown capability: %v", err)
	}
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		r.Checks[opt.CapabilityWorker] = "provider-secret"
		return r, e
	}
	if _, err = service.Capabilities(ctx, key); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("unknown state: %v", err)
	}
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		actor = opt.IdentityRef{Subject: "substituted"}
		return r, e
	}
	if _, err = service.Capabilities(ctx, key); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("changed actor: %v", err)
	}
	actor = bundle.Experiment.Spec.Requester
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		denied[opt.ActionRead] = true
		return r, e
	}
	if _, err = service.Capabilities(ctx, key); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("revoked read: %v", err)
	}
	denied[opt.ActionRead] = false
	var original opt.HostCapabilities
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		var e error
		original, e = ready(c, target)
		return original, e
	}
	got, err := service.Capabilities(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	original.Checks[opt.CapabilityWorker] = opt.ReadinessUnavailable
	if reflect.DeepEqual(got.Checks, original.Checks) || got.Checks[opt.CapabilityWorker] != opt.ReadinessReady {
		t.Fatal("response aliases provider state")
	}
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		r.PricingRevision = "changed-prices"
		return r, e
	}
	if value, e := service.Capabilities(ctx, key); e != nil || value.TrialsReady {
		t.Fatalf("different pricing revision became ready: %+v %v", value, e)
	}
	callback = func(context.Context, opt.CapabilityTarget) (opt.HostCapabilities, error) {
		return opt.HostCapabilities{}, errors.New("provider-secret")
	}
	if _, err = service.Capabilities(ctx, key); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("provider failure leaked: %v", err)
	}
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		r.PricingRevision = ""
		return r, e
	}
	if _, err = service.Capabilities(ctx, key); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("unqualified price: %v", err)
	}
	// Scope-level diagnostics have no frozen experiment and cannot enable trials.
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		return opt.HostCapabilities{Target: target}, c.Err()
	}
	scopeOnly, err := service.Capabilities(ctx, id.Nil)
	if err != nil || scopeOnly.TrialsReady || len(scopeOnly.EditableDimensions) != 0 || !scopeOnly.Target.ExperimentID.IsNil() {
		t.Fatalf("scope-level readiness: %+v %v", scopeOnly, err)
	}
	tokenBundle := storetest.Fixture(t, "capabilities")
	if _, err = service.Create(ctx, tokenBundle); err != nil {
		t.Fatal(err)
	}
	bundle = tokenBundle
	key = bundle.Experiment.ID
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		r, e := ready(c, target)
		r.PricingQuality = opt.UsageUnknown
		r.PricingRevision = ""
		return r, e
	}
	if value, e := service.Capabilities(ctx, key); e != nil || !value.TrialsReady || value.PricingQuality != opt.UsageUnknown {
		t.Fatalf("token-only pricing: %+v %v", value, e)
	}
	probeContext, cancelProbe := context.WithCancel(ctx)
	callback = func(c context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		report, e := ready(c, target)
		cancelProbe()
		return report, e
	}
	if _, err = service.Capabilities(probeContext, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("probe swallowed cancellation: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	before = calls
	if _, err = service.Capabilities(cancelled, key); !errors.Is(err, context.Canceled) || calls != before {
		t.Fatalf("cancelled probe: %v", err)
	}
}
func ctxScope(ctx context.Context) string { return cortex.ScopeFromContext(ctx).Canonical() }
