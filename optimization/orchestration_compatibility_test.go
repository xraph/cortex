package optimization

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
)

func TestTeamCompatibilityChecksEveryOriginalSource(t *testing.T) {
	for _, change := range []string{"none", "guidance", "participant", "configuration", "host-mutation"} {
		t.Run(change, func(t *testing.T) {
			ctx, sources, runtime, _, version := freshOrchestrationFixture(t)
			if change == "guidance" {
				var err error
				version, err = ValidateProposal(version, guidanceProposal("Review before delegation."), version.Policy)
				if err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			host := compatibilityHost(func(hostCtx context.Context, received HarnessVersion) (CompatibilityReport, error) {
				calls++
				switch change {
				case "participant":
					ag, err := sources.GetByName(hostCtx, "second")
					if err != nil {
						t.Fatal(err)
					}
					ag.SystemPrompt = "changed during external checks"
					if err = sources.Update(hostCtx, ag); err != nil {
						t.Fatal(err)
					}
				case "configuration":
					view, err := received.Orchestration.Prepared.Inspect()
					if err != nil {
						t.Fatal(err)
					}
					cfg, err := sources.GetOrchestration(hostCtx, view.Config.ID)
					if err != nil {
						t.Fatal(err)
					}
					cfg.Name = "changed configuration"
					if err = sources.UpdateOrchestration(hostCtx, cfg); err != nil {
						t.Fatal(err)
					}
				case "host-mutation":
					received.Policy.OrchestrationPresets[0].Limits.Tokens++
					received.Scope.Levels[0].Value = "foreign"
				}
				return CompatibilityReport{Version: VersionRef{ID: version.ID, Digest: version.Digest}, PolicyDigest: version.PolicyDigest, Compatible: true}, nil
			})
			checker, err := NewEngineCompatibility(EngineCompatibilityConfig{Engine: runtime, RuntimeVersion: version.RuntimeVersion, Host: host})
			if err != nil {
				t.Fatal(err)
			}
			report, err := checker.Check(ctx, version)
			if change == "participant" || change == "configuration" {
				if !errors.Is(err, ErrStaleVersion) || calls != 1 {
					t.Fatalf("accepted changed source: calls=%d err=%v", calls, err)
				}
			} else if err != nil || !report.Compatible || calls != 1 {
				t.Fatalf("compatible team: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestTeamCompatibilityChecksScopeAndRuntimeBindings(t *testing.T) {
	ctx, sources, _, _, version := freshOrchestrationFixture(t)
	runtime := compatibilityEngine(t, sources, "changed-model-revision", "tool-v1")
	checker, err := NewEngineCompatibility(EngineCompatibilityConfig{Engine: runtime, RuntimeVersion: version.RuntimeVersion, Host: compatibilityHost(compatibleHost)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = checker.Check(ctx, version); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("accepted changed model: %v", err)
	}
	foreign := cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "foreign"}}})
	if _, err = checker.Check(foreign, version); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("accepted foreign scope: %v", err)
	}
}
