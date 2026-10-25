package optimization_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	opt "github.com/xraph/cortex/optimization"
)

type controllerHost func(context.Context, opt.HarnessVersion) (opt.CompatibilityReport, error)

func (f controllerHost) Check(ctx context.Context, v opt.HarnessVersion) (opt.CompatibilityReport, error) {
	return f(ctx, v)
}

func TestControllerCompatibilityChecksBindingsWithoutDispatch(t *testing.T) {
	repo, ctx, reservation, _ := meterFixture(t)
	experiment, err := repo.GetExperiment(ctx, reservation.ExperimentID)
	if err != nil {
		t.Fatal(err)
	}
	version, err := repo.GetVersion(ctx, experiment.Spec.Controller.ID)
	if err != nil {
		t.Fatal(err)
	}
	factory, err := opt.NewControllerFactory(repo, meterAuthorizer(func(context.Context, opt.Action, opt.Resource) error { return nil }), meterResolver(func(ctx context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) { return ctx, nil }), []opt.ModelBinding{{Model: "fixture", Revision: "1", Provider: modelClient{complete: func(context.Context, *llm.Request) (*llm.Response, error) {
		t.Fatal("compatibility dispatched model")
		return nil, opt.ErrUnavailable
	}}, Counter: inputBound(func(context.Context, *llm.Request) (opt.InputTokenBound, error) {
		t.Fatal("compatibility estimated a call")
		return opt.InputTokenBound{}, opt.ErrUnavailable
	})}}, opt.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 10, MaxProposals: 2, MaxLessons: 2})
	if err != nil {
		t.Fatal(err)
	}
	compatible := true
	hostCalls := 0
	checker, err := opt.NewControllerCompatibility(opt.ControllerCompatibilityConfig{Controllers: factory, Host: controllerHost(func(_ context.Context, v opt.HarnessVersion) (opt.CompatibilityReport, error) {
		hostCalls++
		report := opt.CompatibilityReport{Compatible: compatible, Version: opt.VersionRef{ID: v.ID, Digest: v.Digest}, PolicyDigest: v.PolicyDigest}
		v.Controller.ModelRevision = "mutated"
		return report, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	if report, checkErr := checker.Check(ctx, version); checkErr != nil || !report.Compatible || hostCalls != 1 {
		t.Fatalf("controller compatibility: %+v %v", report, checkErr)
	}
	if version.Controller.ModelRevision != "1" {
		t.Fatal("host mutated frozen controller")
	}
	compatible = false
	if _, err = checker.Check(ctx, version); !errors.Is(err, opt.ErrStaleVersion) {
		t.Fatalf("host denied controller: %v", err)
	}
	compatible = true
	for _, mode := range []string{"model-revision", "tools", "scope", "digest"} {
		t.Run(mode, func(t *testing.T) {
			changed := version
			spec := *version.Controller
			changed.Controller = &spec
			callCtx := ctx
			switch mode {
			case "model-revision":
				changed.Controller.ModelRevision = "unregistered"
			case "tools":
				changed.Controller.Tools = []string{"unsupported"}
			case "scope":
				callCtx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "other"}}})
			case "digest":
				changed.Digest = "invalid"
			}
			if mode != "digest" {
				changed, err = opt.SealVersion(changed)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, checkErr := checker.Check(callCtx, changed); checkErr == nil {
				t.Fatalf("accepted %s", mode)
			}
		})
	}
}
