package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
)

type optimizationHTTPProbe func(context.Context, opt.CapabilityTarget) (opt.HostCapabilities, error)

func (f optimizationHTTPProbe) Inspect(ctx context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
	return f(ctx, target)
}

func TestOptimizationCapabilitiesHTTPRechecksAfterRestart(t *testing.T) {
	workerReady := true
	calls := 0
	probe := optimizationHTTPProbe(func(ctx context.Context, target opt.CapabilityTarget) (opt.HostCapabilities, error) {
		calls++
		if cortex.ScopeFromContext(ctx).Canonical() != target.Scope.Canonical() {
			t.Fatal("probe scope changed")
		}
		checks := map[opt.Capability]opt.ReadinessState{}
		for _, name := range opt.CapabilityNames() {
			checks[name] = opt.ReadinessReady
		}
		if !workerReady {
			checks[opt.CapabilityWorker] = opt.ReadinessUnavailable
		}
		return opt.HostCapabilities{Target: target, Checks: checks, Environment: opt.IsolationCapabilities{Isolated: true, Resettable: true, Recoverable: true, Stoppable: true, FrozenRetrieval: true, AtomicSources: true, DeterministicApproval: true}}, ctx.Err()
	})
	service, _, bundle, ctx, reopen := optimizationHTTPRecoveryFixture(t, probe)
	handler := New(nil, nil, WithOptimization(service)).Handler()
	path := "/capabilities?experiment_id=" + bundle.Experiment.ID.String()
	read := func() opt.CapabilitySnapshot {
		response := optimizationGET(ctx, handler, path)
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("readiness response: %d %s", response.Code, response.Body)
		}
		var result opt.CapabilitySnapshot
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Target.ExperimentID != bundle.Experiment.ID || result.PricingQuality != opt.UsageUnknown || result.Checks[opt.CapabilityProductionRouting] != opt.ReadinessUnavailable {
			t.Fatalf("unqualified readiness: %+v", result)
		}
		return result
	}
	if result := read(); !result.TrialsReady {
		t.Fatalf("token-only fixture prerequisites: %+v", result)
	}
	workerReady = false
	if result := read(); result.TrialsReady || result.Checks[opt.CapabilityWorker] != opt.ReadinessUnavailable {
		t.Fatalf("cached worker status: %+v", result)
	}
	handler = New(nil, nil, WithOptimization(reopen())).Handler()
	if result := read(); result.TrialsReady {
		t.Fatal("restart restored stale readiness")
	}
	workerReady = true
	if result := read(); !result.TrialsReady {
		t.Fatal("repaired worker still unavailable")
	}
	if calls != 4 {
		t.Fatalf("probe calls: %d", calls)
	}
}
