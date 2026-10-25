package optimization

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/store/scopespy"
	rootsqlite "github.com/xraph/cortex/store/sqlite"
)

type compatibilityHost func(context.Context, HarnessVersion) (CompatibilityReport, error)

func (f compatibilityHost) Check(ctx context.Context, version HarnessVersion) (CompatibilityReport, error) {
	return f(ctx, version)
}

type compatibilityModel struct{ t *testing.T }

func (m compatibilityModel) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	m.t.Error("compatibility executed a model")
	return nil, ErrUnavailable
}
func (m compatibilityModel) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	m.t.Error("compatibility streamed a model")
	return nil, ErrUnavailable
}

func compatibilityFixture(t *testing.T) (context.Context, *rootsqlite.Store, *engine.Engine, *agent.Config, HarnessVersion) {
	t.Helper()
	driver := sqlitedriver.New()
	if err := driver.Open(context.Background(), filepath.Join(t.TempDir(), "sources.db")); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(driver)
	if err != nil {
		t.Fatal(err)
	}
	source := rootsqlite.New(db)
	t.Cleanup(func() {
		if closeErr := source.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if err = source.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "compatibility"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", SystemPrompt: "Use the frozen source.", Model: "model", Tools: []string{"read"}}
	if err = source.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	runtime := compatibilityEngine(t, source, "model-v1", "tool-v1")
	prepared, err := runtime.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	version := fixtureVersion(t)
	version.Scope, version.AgentID, version.Prepared = scope, ag.ID, prepared
	version.RuntimeVersion = "runtime-v1"
	version, err = SealVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, source, runtime, ag, version
}
func compatibilityEngine(t *testing.T, s *rootsqlite.Store, modelRevision, toolRevision string) *engine.Engine {
	t.Helper()
	e, err := engine.New(engine.WithStore(s), engine.WithLLM(compatibilityModel{t}), engine.WithModelRevision("model", modelRevision), engine.WithToolRevision("read", toolRevision), engine.WithTool(llm.Tool{Name: "read", Description: "Read", Parameters: map[string]any{"type": "object"}}, func(context.Context, cortex.Invocation) (string, error) {
		t.Error("compatibility executed a tool")
		return "", ErrUnavailable
	}))
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func compatibleHost(_ context.Context, v HarnessVersion) (CompatibilityReport, error) {
	return CompatibilityReport{Version: VersionRef{ID: v.ID, Digest: v.Digest}, PolicyDigest: v.PolicyDigest, Compatible: true}, nil
}

func TestEngineCompatibilityUsesCurrentSourcesAndBindings(t *testing.T) {
	for _, kind := range []string{"unchanged", "derived", "agent", "model", "tool", "runtime", "foreign-scope", "tampered", "policy", "host-error", "host-mutates-copy", "host-wrong-version", "host-incompatible", "during-host-check", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			ctx, s, runtime, ag, version := compatibilityFixture(t)
			host := compatibilityHost(compatibleHost)
			runtimeVersion := "runtime-v1"
			want := ErrStaleVersion
			switch kind {
			case "unchanged", "derived":
				want = nil
			case "agent":
				ag.SystemPrompt = "changed"
				if err := s.Update(ctx, ag); err != nil {
					t.Fatal(err)
				}
			case "model":
				runtime = compatibilityEngine(t, s, "model-v2", "tool-v1")
			case "tool":
				runtime = compatibilityEngine(t, s, "model-v1", "tool-v2")
			case "runtime":
				runtimeVersion = "runtime-v2"
			case "foreign-scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}})
				want = ErrPolicyDenied
			case "tampered":
				version.Digest = "tampered"
			case "policy":
				host = func(ctx context.Context, v HarnessVersion) (CompatibilityReport, error) {
					r, _ := compatibleHost(ctx, v)
					r.PolicyDigest = "stale"
					return r, nil
				}
			case "host-error":
				host = func(context.Context, HarnessVersion) (CompatibilityReport, error) {
					return CompatibilityReport{}, ErrPolicyDenied
				}
				want = ErrPolicyDenied
			case "host-mutates-copy":
				host = func(ctx context.Context, v HarnessVersion) (CompatibilityReport, error) {
					v.Scope.Levels[0].Value = "host mutation"
					v.Policy.MutableSections[0] = "changed"
					return compatibleHost(ctx, v)
				}
				want = nil
			case "host-wrong-version":
				host = func(ctx context.Context, v HarnessVersion) (CompatibilityReport, error) {
					r, _ := compatibleHost(ctx, v)
					r.Version.Digest = "wrong"
					return r, nil
				}
			case "host-incompatible":
				host = func(ctx context.Context, v HarnessVersion) (CompatibilityReport, error) {
					r, _ := compatibleHost(ctx, v)
					r.Compatible = false
					return r, nil
				}
			case "during-host-check":
				host = func(ctx context.Context, v HarnessVersion) (CompatibilityReport, error) {
					ag.SystemPrompt = "changed during host read"
					if err := s.Update(ctx, ag); err != nil {
						return CompatibilityReport{}, err
					}
					return compatibleHost(ctx, v)
				}
			case "cancelled":
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
				want = context.Canceled
			}
			if kind == "derived" {
				p, err := version.Prepared.Derive(engine.PreparedEdits{Temperature: new(.25)})
				if err != nil {
					t.Fatal(err)
				}
				version.Prepared = p
				version, err = SealVersion(version)
				if err != nil {
					t.Fatal(err)
				}
			}
			checker, err := NewEngineCompatibility(EngineCompatibilityConfig{Engine: runtime, Host: host, RuntimeVersion: runtimeVersion})
			if err != nil {
				t.Fatal(err)
			}
			report, err := checker.Check(ctx, version)
			if want != nil {
				if !errors.Is(err, want) || report.Compatible {
					t.Fatalf("%s admitted: %+v %v", kind, report, err)
				}
				return
			}
			if err != nil || !report.Compatible || report.Version != (VersionRef{ID: version.ID, Digest: version.Digest}) || report.PolicyDigest != version.PolicyDigest {
				t.Fatalf("qualified version refused: %+v %v", report, err)
			}
		})
	}
}

func TestEngineCompatibilityMatchesBoundExecutionEngine(t *testing.T) {
	_, _, runtime, _, _ := compatibilityFixture(t)
	other, err := engine.New()
	if err != nil {
		t.Fatal(err)
	}
	checker, err := NewEngineCompatibility(EngineCompatibilityConfig{Engine: runtime, Host: compatibilityHost(compatibleHost), RuntimeVersion: "runtime-v1"})
	if err != nil {
		t.Fatal(err)
	}
	op := &Optimizer{config: OptimizerConfig{Compatibility: checker}}
	if err = WithActiveEngine(other)(op); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("execution bound a different engine than compatibility: %v", err)
	}
	if err = WithActiveEngine(runtime)(op); err != nil {
		t.Fatal(err)
	}
}

func TestEngineCompatibilityRequiresCurrentSourceSnapshot(t *testing.T) {
	version := fixtureVersion(t)
	runtime, err := engine.New(engine.WithStore(scopespy.New()), engine.WithLLM(compatibilityModel{t}), engine.WithModelRevision("test-model", "fixture-v1"), engine.WithToolRevision("read", "fixture-v1"), engine.WithTool(llm.Tool{Name: "read", Description: "Read fixture"}, func(context.Context, cortex.Invocation) (string, error) {
		t.Error("compatibility executed a tool")
		return "", ErrUnavailable
	}))
	if err != nil {
		t.Fatal(err)
	}
	checker, err := NewEngineCompatibility(EngineCompatibilityConfig{Engine: runtime, Host: compatibilityHost(compatibleHost), RuntimeVersion: version.RuntimeVersion})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Check(cortex.WithScope(context.Background(), version.Scope), version)
	if !errors.Is(err, ErrUnavailable) || report.Compatible {
		t.Fatalf("snapshotless engine qualified: %+v %v", report, err)
	}
}

func TestEngineCompatibilityDistinguishesStaleFromUnavailable(t *testing.T) {
	for _, test := range []struct{ cause, want error }{
		{cortex.ErrInvalidPrepared, ErrStaleVersion},
		{cortex.ErrPreparationConflict, ErrStaleVersion},
		{cortex.ErrNoStore, ErrUnavailable},
		{cortex.ErrNoLLM, ErrUnavailable},
		{cortex.ErrPreparationUnavailable, ErrUnavailable},
		{errors.New("source database unavailable"), ErrUnavailable},
		{context.Canceled, context.Canceled},
	} {
		mapped := compatibilityError(test.cause)
		if !errors.Is(mapped, test.want) || !errors.Is(mapped, test.cause) {
			t.Errorf("cause=%v mapped=%v want=%v", test.cause, mapped, test.want)
		}
	}
}
