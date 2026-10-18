package engine_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
)

func TestAtomicPreparationRecordsSnapshot(t *testing.T) {
	ctx := overlayCtx("atomic")
	s := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "atomic", SystemPrompt: "original"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(s), engine.WithAtomicPreparation(), engine.WithLLM(&recordingLLM{}))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := p.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if view.Provenance.SourceValidation != "atomic_store_snapshot" || view.Provenance.SourceSnapshot.ID == "" || view.Provenance.SourceSnapshot.Scope != cortex.ScopeFromContext(ctx).Canonical() {
		t.Fatalf("missing atomic receipt: %+v", view.Provenance)
	}
	executed, err := e.RunPrepared(ctx, p, "work", engine.PreparedRunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := s.GetRun(ctx, executed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Provenance.SourceSnapshot != view.Provenance.SourceSnapshot {
		t.Fatal("snapshot receipt lost during execution or persistence")
	}
	if !e.RuntimeCapabilities().AtomicSourceSnapshot {
		t.Fatal("installed snapshot capability missing")
	}
}

func TestAtomicPreparationRejectsExternalKnowledge(t *testing.T) {
	ctx := overlayCtx("atomic")
	s := newSQLiteStoreForTest(t)
	sk := &skill.Skill{ID: id.NewSkillID(), Name: "research", Knowledge: []skill.KnowledgeRef{{Source: "live"}}}
	if err := s.CreateSkill(ctx, sk); err != nil {
		t.Fatal(err)
	}
	ag := &agent.Config{ID: id.NewAgentID(), Name: "atomic", InlineSkills: []string{"research"}}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(s), engine.WithAtomicPreparation(), engine.WithKnowledge(staticKnowledge{}))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	if p != nil || !errors.Is(err, cortex.ErrPreparationUnavailable) {
		t.Fatalf("live knowledge claimed atomic: %v %v", p, err)
	}
}

type noSnapshotStore struct{ store.Store }

func TestAtomicPreparationCannotFallBack(t *testing.T) {
	ctx := overlayCtx("atomic")
	s := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "atomic"}
	if err := s.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.WithStore(noSnapshotStore{s}), engine.WithAtomicPreparation())
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	if p != nil || !errors.Is(err, cortex.ErrPreparationUnavailable) {
		t.Fatalf("unsupported store fell back: %v %v", p, err)
	}
	if e.RuntimeCapabilities().AtomicSourceSnapshot {
		t.Fatal("unsupported capability advertised")
	}
}

type controlledSnapshotStore struct {
	store.Store
	capture func(context.Context, func(store.PreparationSources) error) (*run.SourceSnapshot, error)
}

func (s controlledSnapshotStore) CapturePreparation(ctx context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
	return s.capture(ctx, collect)
}

type changingSnapshotSources struct {
	store.PreparationSources
	change func()
}

func (s *changingSnapshotSources) Get(ctx context.Context, key id.AgentID) (*agent.Config, error) {
	ag, err := s.PreparationSources.Get(ctx, key)
	if s.change != nil {
		change := s.change
		s.change = nil
		change()
	}
	return ag, err
}

func TestAtomicPreparationUsesOneSourceView(t *testing.T) {
	ctx := overlayCtx("atomic")
	base := openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "atomic.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", SystemPrompt: "original role", PersonaRef: "persona"}
	personaValue := &persona.Persona{ID: id.NewPersonaID(), Name: "persona", Identity: "original identity"}
	if err := base.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	if err := base.CreatePersona(ctx, personaValue); err != nil {
		t.Fatal(err)
	}
	s := controlledSnapshotStore{Store: base, capture: func(c context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
		return base.CapturePreparation(c, func(view store.PreparationSources) error {
			return collect(&changingSnapshotSources{PreparationSources: view, change: func() {
				personaValue.Identity = "new identity"
				if err := base.UpdatePersona(ctx, personaValue); err != nil {
					t.Fatal(err)
				}
			}})
		})
	}}
	e, err := engine.New(engine.WithStore(s), engine.WithAtomicPreparation())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := e.PrepareAgent(ctx, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, section := range view.Sections {
		if strings.Contains(section.Body, "new identity") {
			t.Fatal("preparation mixed source generations")
		}
		found = found || strings.Contains(section.Body, "original identity")
	}
	if !found {
		t.Fatal("captured identity missing")
	}
}

func TestAtomicPreparationRejectsFailedCaptureReceipts(t *testing.T) {
	for _, mode := range []string{"no_callback", "no_receipt", "duplicate_callback", "wrong_scope", "commit_failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx := overlayCtx("atomic")
			base := newSQLiteStoreForTest(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "worker"}
			if err := base.Create(ctx, ag); err != nil {
				t.Fatal(err)
			}
			s := controlledSnapshotStore{Store: base, capture: func(c context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
				receipt := &run.SourceSnapshot{ID: "capture", Provider: "fixture", Isolation: "fixture", Scope: cortex.ScopeFromContext(c).Canonical()}
				if mode == "no_callback" {
					return receipt, nil
				}
				if err := collect(base); err != nil {
					return nil, err
				}
				switch mode {
				case "no_receipt":
					return nil, nil
				case "duplicate_callback":
					_ = collect(base)
				case "wrong_scope":
					receipt.Scope = "workspace=elsewhere"
				case "commit_failure":
					return nil, errors.New("transaction failed")
				}
				return receipt, nil
			}}
			e, err := engine.New(engine.WithStore(s), engine.WithAtomicPreparation())
			if err != nil {
				t.Fatal(err)
			}
			p, err := e.PrepareAgent(ctx, ag.ID, nil)
			if p != nil || err == nil {
				t.Fatal("invalid capture produced a prepared artifact")
			}
		})
	}
}

type snapshotPublicationGate struct{ check func() }

func (g snapshotPublicationGate) Check(_ context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
	g.check()
	return safety.PublicationDecision{Text: r.Content, Status: safety.StatusApproved}, nil
}

func TestAtomicPreparationClosesSnapshotBeforePublication(t *testing.T) {
	ctx := overlayCtx("atomic")
	base := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "worker", Sections: []prompt.Section{{ID: "captured", Source: prompt.SourceKnowledge, Body: "original"}}}
	if err := base.Create(ctx, ag); err != nil {
		t.Fatal(err)
	}
	var source store.PreparationSources
	s := controlledSnapshotStore{Store: base, capture: func(c context.Context, collect func(store.PreparationSources) error) (*run.SourceSnapshot, error) {
		return base.CapturePreparation(c, func(view store.PreparationSources) error { source = view; return collect(view) })
	}}
	calls := 0
	e, err := engine.New(engine.WithStore(s), engine.WithAtomicPreparation(), engine.WithPublicationGate(snapshotPublicationGate{check: func() {
		calls++
		if source == nil {
			t.Fatal("publication preceded capture")
		}
		if _, readErr := source.Get(ctx, ag.ID); !errors.Is(readErr, cortex.ErrPreparationUnavailable) {
			t.Fatalf("publication held snapshot open: %v", readErr)
		}
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.PrepareAgent(ctx, ag.ID, nil); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("publication check skipped")
	}
}
